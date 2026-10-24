package source

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Beads reads the project's beads through `bd --readonly --sandbox` only:
// --readonly blocks writes and --sandbox disables Dolt auto-push. The board
// never writes a bead.
type Beads struct {
	// Humans are assignee names that belong to the board owner.
	Humans []string
	// GitHubRepo is the single configured "owner/repo"; with none, a bead's
	// external_ref yields no same-work relation.
	GitHubRepo string
}

// beadsListArgv reads every non-closed bead. Deferred beads are read so a
// deferred blocker still blocks; they are not shown. --limit 0 because bd
// otherwise stops at 50 rows without saying so.
var beadsListArgv = []string{"bd", "--readonly", "--sandbox", "list", "--json", "--brief", "--status", "open,in_progress,blocked,deferred", "--limit", "0"}

func (Beads) Name() string { return "beads" }

func (Beads) Discover(env board.Env) (bool, string) {
	if _, err := env.LookPath("bd"); err != nil {
		return false, "bd not on PATH"
	}
	if !env.IsDir(filepath.Join(env.Dir, ".beads")) {
		return false, "no .beads directory in the project"
	}
	return true, ""
}

func (b Beads) Read(ctx context.Context, env board.Env) board.Result {
	out, err := env.Run.Run(ctx, env.Dir, beadsListArgv)
	return b.Build(out, err, env.Now())
}

// ResolveBeads looks up bead ids that are not in the open set, so a closed
// bead mentioned elsewhere keeps its identity.
func (Beads) ResolveBeads(ctx context.Context, env board.Env, ids []string) (map[string]string, error) {
	argv := []string{"bd", "--readonly", "--sandbox", "list", "--all", "--id", strings.Join(ids, ","), "--json", "--brief", "--limit", "0"}
	out, err := env.Run.Run(ctx, env.Dir, argv)
	if err != nil {
		return nil, err
	}
	rows, err := parseBeadRows(out)
	if err != nil {
		return nil, err
	}
	found := map[string]string{}
	for _, r := range rows {
		if _, ok := board.Bead(r.ID); ok {
			found[r.ID] = r.Status
		}
	}
	return found, nil
}

var beadsFeeds = []board.Column{board.ColumnWaiting, board.ColumnInFlight, board.ColumnBlocked, board.ColumnUnclaimed}

type beadRow struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Priority     *int   `json:"priority"`
	Assignee     string `json:"assignee"`
	UpdatedAt    string `json:"updated_at"`
	StartedAt    string `json:"started_at"`
	ExternalRef  string `json:"external_ref"`
	Dependencies []struct {
		IssueID     string `json:"issue_id"`
		DependsOnID string `json:"depends_on_id"`
		Type        string `json:"type"`
	} `json:"dependencies"`
}

func parseBeadRows(data []byte) ([]beadRow, error) {
	var rows []beadRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Build maps one `bd list --json` output into a result. It is pure.
func (b Beads) Build(out []byte, runErr error, now time.Time) board.Result {
	res := board.Result{Status: board.StatusOK, ObservedAt: now, Feeds: beadsFeeds}
	if runErr != nil {
		res.Status, res.Detail = board.StatusDegraded, "bd list failed: "+errLine(runErr)
		return res
	}
	rows, err := parseBeadRows(out)
	if err != nil {
		res.Status, res.Detail = board.StatusDegraded, "bd list output unreadable: "+err.Error()
		return res
	}
	humans := map[string]bool{reservedHuman: true}
	for _, h := range b.Humans {
		humans[h] = true
	}
	byID := map[string]beadRow{}
	res.BeadIDs = map[string]string{}
	for _, r := range rows {
		if _, ok := board.Bead(r.ID); !ok {
			res.Coverage.Skipped++
			continue
		}
		byID[r.ID] = r
		res.BeadIDs[r.ID] = r.Status
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := byID[id]
		if it, ok := b.item(r, byID, humans); ok {
			res.Items = append(res.Items, it)
		}
	}
	if res.Coverage.Skipped > 0 {
		res.Coverage.Detail = fmt.Sprintf("%d beads with an invalid id", res.Coverage.Skipped)
	}
	return res
}

func (b Beads) item(r beadRow, byID map[string]beadRow, humans map[string]bool) (board.Item, bool) {
	ref, _ := board.Bead(r.ID)
	owner := beadOwner(r.Assignee, humans)
	updated, _ := time.Parse(time.RFC3339Nano, r.UpdatedAt)
	started, _ := time.Parse(time.RFC3339Nano, r.StartedAt)
	it := board.Item{
		Ref:        ref,
		Title:      r.Title,
		Owner:      owner,
		WaitsOn:    []board.Owner{},
		UpdatedAt:  updated,
		Structural: true,
		Relations:  []board.Relation{},
		Evidence:   []board.Evidence{{Field: "status", Value: r.Status, At: updated}},
	}
	if r.Priority != nil {
		it.Evidence = append(it.Evidence, board.Evidence{Field: "priority", Value: "P" + strconv.Itoa(*r.Priority), At: updated})
	}

	var blockers []string
	for _, d := range r.Dependencies {
		to, ok := board.Bead(d.DependsOnID)
		if !ok || d.DependsOnID == r.ID {
			continue
		}
		it.Relations = append(it.Relations, board.Relation{Kind: board.RelationRelated, To: to, Basis: d.Type})
		if d.Type == "blocks" {
			if blocker, open := byID[d.DependsOnID]; open && blocker.Status != "closed" {
				blockers = append(blockers, d.DependsOnID)
				it.WaitsOn = append(it.WaitsOn, beadOwner(blocker.Assignee, humans))
			}
		}
	}
	if n, ok := strings.CutPrefix(r.ExternalRef, "gh-"); ok && b.GitHubRepo != "" {
		if num, err := strconv.Atoi(n); err == nil {
			if to, ok := board.GitHubRepoRef(b.GitHubRepo, num); ok {
				it.Relations = append(it.Relations, board.Relation{Kind: board.RelationSameWork, To: to, Basis: "external_ref"})
			}
		}
	}

	since := updated
	switch {
	case r.Status == "blocked" || ((r.Status == "open" || r.Status == "in_progress") && len(blockers) > 0):
		it.State = board.StateBlocked
		text := "bead blocked"
		if len(blockers) > 0 {
			text = "blocked by " + strings.Join(blockers, ", ")
		}
		it.Reason = board.Reason{Field: "status", Text: text, Since: since}
	case r.Status == "in_progress":
		it.State = board.StateActive
		if !started.IsZero() {
			since = started
		}
		it.Reason = board.Reason{Field: "status", Text: "bead in progress", Since: since}
	case r.Status == "open" && owner.Kind != board.OwnerNone:
		it.State = board.StateActive
		it.Reason = board.Reason{Field: "assignee", Text: "bead assigned, not started", Since: since}
	case r.Status == "open":
		it.State = board.StateOpenUnowned
		it.Reason = board.Reason{Field: "status", Text: "bead open, no assignee", Since: since}
	default:
		// deferred, closed, or a status this reader does not place.
		return board.Item{}, false
	}
	return it, true
}

// beadOwner maps an assignee: the owner's names are human, a valid AMQ handle
// is an agent, anything else is unmapped (never "no owner").
func beadOwner(assignee string, humans map[string]bool) board.Owner {
	switch {
	case assignee == "":
		return board.Owner{Kind: board.OwnerNone}
	case humans[assignee]:
		return board.Owner{Kind: board.OwnerHuman, Display: assignee}
	case fsq.ValidateHandle(assignee) == nil:
		return board.Owner{Kind: board.OwnerAgent, Handle: assignee, Display: assignee}
	}
	return board.Owner{Kind: board.OwnerUnmapped, Display: assignee}
}
