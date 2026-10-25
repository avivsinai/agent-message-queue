package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// ClaudePermissions reads only Claude Code's live session status files. It
// never opens the adjacent keys, transcripts, or an AMQ root.
type ClaudePermissions struct{}

func (ClaudePermissions) Name() string { return "claude-permissions" }

func (ClaudePermissions) Discover(board.Env) (bool, string) { return true, "" }

// These private-format versions were checked against session records, and
// 2.1.310 against its installed producer. A new version needs that check too.
func claudePermissionVersion(version string) bool {
	switch version {
	case "2.1.289", "2.1.292", "2.1.294", "2.1.296", "2.1.299", "2.1.308", "2.1.309", "2.1.310":
		return true
	}
	return false
}

func (ClaudePermissions) Read(ctx context.Context, env board.Env) (res board.Result) {
	res = board.Result{Status: board.StatusOK, ObservedAt: env.Now(), Feeds: []board.Column{board.ColumnWaiting}}
	var problems []string
	versions := map[string]bool{}
	latest := map[string]int64{}
	waits := map[string]board.Item{}
	fail := func(err error) { res.Status = board.StatusDegraded; problems = append(problems, err.Error()) }
	defer func() {
		ids := make([]string, 0, len(waits))
		for id := range waits {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			res.Items = append(res.Items, waits[id])
		}
		res.Detail = strings.Join(problems, "; ")
		var names []string
		for v := range versions {
			names = append(names, v)
		}
		sort.Strings(names)
		res.Version = strings.Join(names, ", ")
		if res.Coverage.Skipped > 0 {
			res.Coverage.Detail = fmt.Sprintf("%d unmapped sessions outside the 36h discovery window", res.Coverage.Skipped)
		}
	}()
	home := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			fail(err)
			return res
		}
		home = filepath.Join(userHome, ".claude")
	}
	dir, err := os.Open(filepath.Join(home, "sessions"))
	if err != nil {
		if os.IsNotExist(err) {
			res.Status, problems = board.StatusUnavailable, []string{"Claude session status directory not found"}
		} else {
			fail(err)
		}
		return res
	}
	defer func() { _ = dir.Close() }()
	mapping, err := claudePermissionMapping(ctx, env)
	if err != nil {
		fail(err)
		return res
	}
	cutoff := env.Now().Add(-board.DefaultConfig().StaleAfter)
	for {
		if err := ctx.Err(); err != nil {
			fail(err)
			return res
		}
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			pid, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil || pid <= 0 || entry.Name() != strconv.Itoa(pid)+".json" || !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				fail(fmt.Errorf("claude session %s: %w", entry.Name(), err))
				continue
			}
			old := info.ModTime().Before(cutoff)
			if old && len(mapping.owners) == 0 {
				res.Coverage.Skipped++
				continue
			}
			path := filepath.Join(home, "sessions", entry.Name())
			f, _, err := fsq.OpenRegularNoFollowBounded(path, 64<<10)
			if err != nil {
				fail(fmt.Errorf("claude session %s: %w", entry.Name(), err))
				continue
			}
			raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
			_ = f.Close()
			if err != nil || len(raw) > 64<<10 {
				fail(fmt.Errorf("claude session %s: incomplete or oversized status file", entry.Name()))
				continue
			}
			// Project identity is projected before version or payload. AMQ
			// exposes UUIDs, not filenames/PIDs, so an old file's identity
			// must be checked when ready Claude bindings exist.
			var identity struct {
				ID  string `json:"sessionId"`
				Cwd string `json:"cwd"`
				PID int    `json:"pid"`
			}
			if err := json.Unmarshal(raw, &identity); err != nil {
				fail(fmt.Errorf("claude session %s: unreadable identity", entry.Name()))
				continue
			}
			owner, bound := mapping.owners[identity.ID]
			if old && !bound {
				res.Coverage.Skipped++
				continue
			}
			cwd, cwdErr := filepath.EvalSymlinks(identity.Cwd)
			if !bound && (cwdErr != nil || !mapping.worktrees[cwd]) {
				continue
			}
			ref, valid := board.ClaudeSession(identity.ID)
			if !valid || identity.PID != pid {
				fail(fmt.Errorf("claude session %s: invalid session identity", entry.Name()))
				continue
			}
			var format struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(raw, &format) != nil || !claudePermissionVersion(format.Version) {
				fail(fmt.Errorf("claude session %s: version %q not supported", entry.Name(), format.Version))
				continue
			}
			versions[format.Version] = true
			var status struct {
				Name      string `json:"name"`
				Status    string `json:"status"`
				Waiting   string `json:"waitingFor"`
				UpdatedAt int64  `json:"statusUpdatedAt"`
			}
			if err := json.Unmarshal(raw, &status); err != nil {
				fail(fmt.Errorf("claude session %s: unreadable status", entry.Name()))
				continue
			}
			if status.UpdatedAt <= 0 {
				fail(fmt.Errorf("claude session %s: status has no timestamp", entry.Name()))
				continue
			}
			// A resume can leave another PID file for this session. The
			// latest status wins, including a status that cleared the wait.
			if status.UpdatedAt <= latest[identity.ID] {
				continue
			}
			latest[identity.ID] = status.UpdatedAt
			delete(waits, identity.ID)
			if status.Status != "waiting" || status.Waiting != "permission prompt" {
				continue
			}
			alive, err := claudePermissionProcessAlive(pid)
			if err != nil {
				fail(fmt.Errorf("claude session %s: %w", entry.Name(), err))
				continue
			}
			if !alive {
				continue
			}
			if !bound {
				owner = board.Owner{Kind: board.OwnerUnmapped, Display: "Claude session " + identity.ID + " in " + cwd}
				if mapping.hint.Kind == board.OwnerAgent && !mapping.notReady[identity.ID] {
					owner = mapping.hint
				}
			}
			name := status.Name
			if name == "" {
				name = identity.ID
			}
			at := time.UnixMilli(status.UpdatedAt).UTC()
			waits[identity.ID] = board.Item{Ref: ref, Title: "Claude session " + name + " waits for a permission prompt",
				State: board.StateNeedsHuman, Owner: owner, Structural: bound && owner.Kind == board.OwnerAgent,
				UpdatedAt: at, WaitsOn: []board.Owner{{Kind: board.OwnerHuman, Handle: reservedHuman, Display: "you"}},
				Reason: board.Reason{Field: "waitingFor", Text: "permission prompt", Since: at}, Relations: []board.Relation{},
				Evidence: []board.Evidence{{Field: "status", Value: status.Status, At: at}, {Field: "waitingFor", Value: status.Waiting, At: at}}}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				fail(readErr)
			}
			return res
		}
	}
}
