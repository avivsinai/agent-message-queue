package source

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// ProviderSchema is the one document format an external provider may emit.
const ProviderSchema = "amq/board-provider/v1"

const (
	maxProviderBytes = 1 << 20
	maxProviderItems = 2000
)

// External runs one provider the owner registered in ~/.amq/board.json and
// reads one JSON document from its stdout. The document is data only: the
// core assigns the source name, builds every ref, and validates every field;
// a provider can never emit an action, a column, or a command. It runs as
// the owner, as trusted local code; this is not a sandbox.
type External struct {
	Provider board.Provider
}

func (e External) Name() string { return "ext:" + e.Provider.Name }

func (e External) Discover(board.Env) (bool, string) { return true, "" }

func (e External) Read(ctx context.Context, env board.Env) board.Result {
	argv := append([]string{e.Provider.Exec}, e.Provider.Args...)
	out, err := env.Run.RunLimited(ctx, env.Dir, argv, maxProviderBytes)
	return e.Build(out, err, env.Now())
}

var externalFeeds = []board.Column{board.ColumnWaiting, board.ColumnInFlight, board.ColumnBlocked, board.ColumnUnclaimed}

// externalRefKinds is the closed set of ref kinds a provider may name.
var externalRefKinds = map[string]bool{"pr": true, "issue": true, "task": true}

var externalStates = map[board.State]bool{
	board.StateNeedsHuman: true, board.StateActive: true, board.StateBlocked: true,
	board.StateOpenUnowned: true, board.StatePlanned: true, board.StateDone: true,
}

type extOwner struct {
	Kind    board.OwnerKind `json:"kind"`
	Handle  string          `json:"handle"`
	Session string          `json:"session"`
	Display string          `json:"display"`
}

type extDoc struct {
	Schema  string `json:"schema"`
	Version string `json:"version"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
	Items   []struct {
		Ref struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"ref"`
		Title    string     `json:"title"`
		State    string     `json:"state"`
		Owner    extOwner   `json:"owner"`
		WaitsOn  []extOwner `json:"waits_on"`
		LinkKeys []string   `json:"link_keys"`
		Evidence []struct {
			Field string `json:"field"`
			Value string `json:"value"`
			Text  string `json:"text"`
			At    string `json:"at"`
			URL   string `json:"url"`
		} `json:"evidence"`
		UpdatedAt  string `json:"updated_at"`
		Structural bool   `json:"structural"`
	} `json:"items"`
}

// Build validates one provider document. It is pure.
func (e External) Build(out []byte, runErr error, now time.Time) board.Result {
	res := board.Result{Status: board.StatusOK, ObservedAt: now, Feeds: externalFeeds}
	fail := func(detail string) board.Result {
		res.Status, res.Detail = board.StatusDegraded, detail
		return res
	}
	if runErr != nil {
		return fail("provider failed: " + errLine(runErr))
	}
	if len(out) > maxProviderBytes {
		return fail(fmt.Sprintf("provider output over %d bytes", maxProviderBytes))
	}
	var doc extDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return fail("provider output unreadable: " + err.Error())
	}
	if doc.Schema != ProviderSchema {
		return fail(fmt.Sprintf("schema %q, want %q", doc.Schema, ProviderSchema))
	}
	res.Version = cleanText(doc.Version, 64)
	switch doc.Status {
	case "ok", "":
	case "degraded":
		res.Status, res.Detail = board.StatusDegraded, cleanText(doc.Detail, 200)
	default:
		return fail(fmt.Sprintf("status %q is not ok or degraded", cleanText(doc.Status, 32)))
	}
	if len(doc.Items) > maxProviderItems {
		res.Coverage.Truncated = len(doc.Items) - maxProviderItems
		doc.Items = doc.Items[:maxProviderItems]
	}
	for _, d := range doc.Items {
		it, ok := e.item(d.Ref.Kind, d.Ref.ID, d.Title, d.State, d.Owner, d.WaitsOn, d.UpdatedAt, d.Structural)
		if ok {
			for _, ev := range d.Evidence {
				at, _ := time.Parse(time.RFC3339, ev.At)
				url, urlOK := httpsURL(ev.URL)
				if !urlOK || board.HasControl(ev.Field+ev.Value) {
					ok = false
					break
				}
				it.Evidence = append(it.Evidence, board.Evidence{Field: ev.Field, Value: ev.Value, Text: cleanText(ev.Text, 500), At: at, URL: url})
			}
		}
		if ok {
			for _, key := range d.LinkKeys {
				e.linkKey(&it, key)
			}
		}
		if !ok {
			res.Coverage.Skipped++
			continue
		}
		res.Items = append(res.Items, it)
	}
	var gaps []string
	if res.Coverage.Skipped > 0 {
		gaps = append(gaps, fmt.Sprintf("%d items failed validation", res.Coverage.Skipped))
	}
	if res.Coverage.Truncated > 0 {
		gaps = append(gaps, fmt.Sprintf("%d items over the %d cap", res.Coverage.Truncated, maxProviderItems))
	}
	res.Coverage.Detail = strings.Join(gaps, "; ")
	return res
}

func (e External) item(kind, id, title, state string, owner extOwner, waitsOn []extOwner, updatedAt string, structural bool) (board.Item, bool) {
	if !externalRefKinds[kind] || board.HasControl(title) || len(title) > 1000 {
		return board.Item{}, false
	}
	ref, ok := board.External(e.Provider.Name, kind+":"+id)
	if !ok || !externalStates[board.State(state)] {
		return board.Item{}, false
	}
	o, ok := extOwnerOf(owner)
	if !ok {
		return board.Item{}, false
	}
	it := board.Item{Ref: ref, Title: title, State: board.State(state), Owner: o, WaitsOn: []board.Owner{}, Relations: []board.Relation{}, Structural: structural}
	for _, w := range waitsOn {
		wo, ok := extOwnerOf(w)
		if !ok {
			return board.Item{}, false
		}
		it.WaitsOn = append(it.WaitsOn, wo)
	}
	it.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	it.Reason = board.Reason{Field: "state", Text: string(it.State)}
	return it, true
}

func extOwnerOf(o extOwner) (board.Owner, bool) {
	if board.HasControl(o.Display) || len(o.Display) > 200 {
		return board.Owner{}, false
	}
	switch o.Kind {
	case board.OwnerAgent:
		if fsq.ValidateHandle(o.Handle) != nil || (o.Session != "" && fsq.ValidateHandle(o.Session) != nil) {
			return board.Owner{}, false
		}
		return board.Owner{Kind: board.OwnerAgent, Handle: o.Handle, Session: o.Session, Display: o.Display}, true
	case board.OwnerHuman, board.OwnerUnmapped, board.OwnerNone:
		return board.Owner{Kind: o.Kind, Display: o.Display}, true
	}
	return board.Owner{}, false
}

// linkKey reads a provider link key as a related reference only, never as
// the same work. Bead ids still need vocabulary membership.
func (e External) linkKey(it *board.Item, key string) {
	kind, rest, _ := strings.Cut(key, ":")
	switch kind {
	case board.RefBead:
		it.Mentions = append(it.Mentions, board.Mention{Token: rest, Basis: "link key"})
	case board.RefBranch:
		repo, name, ok := strings.Cut(rest, ":")
		parts := strings.Split(repo, "/")
		if !ok || len(parts) != 3 {
			return
		}
		if ref, ok := board.Branch(parts[0], parts[1], parts[2], name); ok {
			it.Relations = append(it.Relations, board.Relation{Kind: board.RelationRelated, To: ref, Basis: "link key", Inferred: true})
		}
	}
}

func httpsURL(u string) (string, bool) {
	if u == "" {
		return "", true
	}
	if !strings.HasPrefix(u, "https://") || len(u) > 2048 || board.HasControl(u) || strings.ContainsAny(u, " \"'<>`") {
		return "", false
	}
	return u, true
}

// cleanText caps display text and drops control characters.
func cleanText(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		if board.HasControl(string(r)) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= n {
			break
		}
	}
	return b.String()
}
