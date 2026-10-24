// Package source holds the board's in-tree sources. Each one only runs
// read-only commands through board.Runner (argv, never a shell) and maps the
// captured output with a pure function.
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// ThreadOpenSchema is the obligation format this reader accepts.
const ThreadOpenSchema = "amq/thread-open/v1"

// AMQ reads the project's queue through the amq CLI only. It never opens a
// file under an AMQ root.
type AMQ struct{}

func (AMQ) Name() string { return "amq" }

func (AMQ) Discover(env board.Env) (bool, string) {
	if _, err := env.LookPath("amq"); err != nil {
		return false, "amq not on PATH"
	}
	return true, ""
}

// AMQCapture is the raw output of every AMQ command one read runs.
type AMQCapture struct {
	Env         []byte
	EnvErr      error
	SessionList []byte
	SessionErr  error
	Who         []byte
	WhoErr      error
	Doctor      map[string][]byte // session name -> doctor --ops --json
	DoctorErr   map[string]error
	ThreadOpen  []byte
	ThreadErr   error
	ObservedAt  time.Time
}

func (a AMQ) Read(ctx context.Context, env board.Env) board.Result {
	c := AMQCapture{Doctor: map[string][]byte{}, DoctorErr: map[string]error{}}
	c.Env, c.EnvErr = env.Run.Run(ctx, env.Dir, []string{"amq", "env", "--json"})
	c.SessionList, c.SessionErr = env.Run.Run(ctx, env.Dir, []string{"amq", "session", "list", "--json"})
	if c.SessionErr == nil {
		c.Who, c.WhoErr = env.Run.Run(ctx, env.Dir, []string{"amq", "who", "--json"})
		if sessions, err := parseSessionList(c.SessionList); err == nil {
			for _, s := range sessions {
				c.Doctor[s.Name], c.DoctorErr[s.Name] = env.Run.Run(ctx, env.Dir, []string{"amq", "doctor", "--ops", "--json", "--root", s.Path})
			}
		}
		c.ThreadOpen, c.ThreadErr = env.Run.Run(ctx, env.Dir, []string{"amq", "thread", "open", "--all-sessions", "--json"})
	}
	c.ObservedAt = env.Now()
	return BuildAMQ(c)
}

// amqFeeds are the columns AMQ obligations can fill.
var amqFeeds = []board.Column{board.ColumnWaiting, board.ColumnInFlight}

// BuildAMQ maps one AMQ capture into a result. It is pure.
func BuildAMQ(c AMQCapture) (res board.Result) {
	res = board.Result{Status: board.StatusOK, ObservedAt: c.ObservedAt, Feeds: amqFeeds, AMQ: &board.AMQContext{Sessions: []board.SessionInfo{}}}
	var problems []string
	fail := func(format string, args ...any) {
		res.Status = board.StatusDegraded
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	defer func() { res.Detail = strings.Join(problems, "; ") }()

	if c.EnvErr == nil {
		var env struct {
			Project string            `json:"project"`
			Peers   map[string]string `json:"peers"`
		}
		if err := json.Unmarshal(c.Env, &env); err == nil {
			res.AMQ.Project = env.Project
			for p := range env.Peers {
				res.AMQ.Peers = append(res.AMQ.Peers, p)
			}
			sort.Strings(res.AMQ.Peers)
		}
	}

	if c.SessionErr != nil {
		fail("session list failed: %s", errLine(c.SessionErr))
		return res
	}
	sessions, err := parseSessionList(c.SessionList)
	if err != nil {
		fail("session list unreadable: %v", err)
		return res
	}
	for _, s := range sessions {
		res.AMQ.Sessions = append(res.AMQ.Sessions, s.SessionInfo)
		res.AMQ.Conversations = append(res.AMQ.Conversations, s.conversations...)
	}

	who := map[[2]string]whoAgent{}
	if c.WhoErr != nil {
		fail("who failed: %s", errLine(c.WhoErr))
	} else if w, err := parseWho(c.Who); err != nil {
		fail("who output unreadable: %v", err)
	} else {
		who = w
	}
	for _, s := range sessions {
		counted := map[string]board.RosterEntry{}
		if err := c.DoctorErr[s.Name]; err != nil {
			fail("doctor failed for session %s: %s", s.Name, errLine(err))
		} else if entries, gate, err := parseDoctorOps(s.Name, c.Doctor[s.Name]); err != nil {
			fail("doctor output unreadable for session %s: %v", s.Name, err)
		} else {
			for _, e := range entries {
				counted[e.Handle] = e
			}
			if gate != nil && gate.OpenCount > 0 {
				res.AMQ.Gates = append(res.AMQ.Gates, *gate)
			}
		}
		for _, h := range s.agents {
			if e, ok := counted[h]; ok {
				res.Roster = append(res.Roster, e)
				continue
			}
			// doctor --ops reports configured agents only. For any other
			// mailbox, liveness comes from who (its presence and verified
			// notifier facts); unread and DLQ stay unknown.
			w := who[[2]string{s.Name, h}]
			e := board.RosterEntry{
				Session:        s.Name,
				Handle:         h,
				Human:          h == reservedHuman,
				ValidHandle:    fsq.ValidateHandle(h) == nil,
				PresenceSource: w.PresenceSource,
				NotifierStatus: w.NotifierStatus,
				PresenceStatus: "unknown",
			}
			if w.Active {
				e.PresenceStatus = "active"
			}
			res.Roster = append(res.Roster, e)
		}
	}

	if c.ThreadErr != nil {
		fail("thread open failed: %s", errLine(c.ThreadErr))
		return res
	}
	items, cov, version, err := parseThreadOpen(c.ThreadOpen)
	if err != nil {
		fail("thread open output not accepted: %v", err)
		return res
	}
	res.Items, res.Coverage, res.Version = items, cov, version
	return res
}

type sessionRow struct {
	board.SessionInfo
	agents        []string
	conversations []board.Conversation
}

func parseSessionList(data []byte) ([]sessionRow, error) {
	var out struct {
		Sessions []struct {
			Name          string   `json:"name"`
			Path          string   `json:"path"`
			Kind          string   `json:"kind"`
			Agents        []string `json:"agents"`
			Conversations []struct {
				Handle   string `json:"handle"`
				Provider string `json:"provider"`
				ID       string `json:"id"`
				State    string `json:"state"`
				Source   string `json:"source"`
			} `json:"conversations"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	var sessions []sessionRow
	for _, s := range out.Sessions {
		if s.Kind != "canonical" || fsq.ValidateHandle(s.Name) != nil || s.Path == "" {
			continue
		}
		row := sessionRow{SessionInfo: board.SessionInfo{Name: s.Name, Path: s.Path}, agents: s.Agents}
		for _, c := range s.Conversations {
			if fsq.ValidateHandle(c.Handle) != nil || c.ID == "" {
				continue
			}
			row.conversations = append(row.conversations, board.Conversation{
				Session: s.Name, Handle: c.Handle, Provider: c.Provider, ID: c.ID, State: c.State, Source: c.Source,
			})
		}
		sessions = append(sessions, row)
	}
	return sessions, nil
}

type whoAgent struct {
	Active         bool   `json:"active"`
	PresenceSource string `json:"presence_source"`
	NotifierStatus string `json:"notifier_status"`
}

func parseWho(data []byte) (map[[2]string]whoAgent, error) {
	var out []struct {
		Name   string `json:"name"`
		Agents []struct {
			Handle string `json:"handle"`
			whoAgent
		} `json:"agents"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	m := map[[2]string]whoAgent{}
	for _, s := range out {
		for _, a := range s.Agents {
			m[[2]string{s.Name, a.Handle}] = a.whoAgent
		}
	}
	return m, nil
}

func parseDoctorOps(session string, data []byte) ([]board.RosterEntry, *board.Gate, error) {
	var out struct {
		Ops *struct {
			Agents []struct {
				Handle                 string  `json:"handle"`
				UnreadCount            int     `json:"unread_count"`
				OldestUnreadAgeSeconds float64 `json:"oldest_unread_age_seconds"`
				DLQCount               int     `json:"dlq_count"`
				PresenceStatus         string  `json:"presence_status"`
				PresenceAgeSeconds     float64 `json:"presence_age_seconds"`
				PresenceSource         string  `json:"presence_source"`
				NotifierStatus         string  `json:"notifier_status"`
			} `json:"agents"`
			OperatorGate *struct {
				OpenCount            int     `json:"open_count"`
				OldestGateAgeSeconds float64 `json:"oldest_gate_age_seconds"`
			} `json:"operator_gate"`
		} `json:"ops"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, nil, err
	}
	if out.Ops == nil {
		return nil, nil, fmt.Errorf("no ops section")
	}
	var entries []board.RosterEntry
	for _, a := range out.Ops.Agents {
		entries = append(entries, board.RosterEntry{
			Session:                session,
			Handle:                 a.Handle,
			Human:                  a.Handle == reservedHuman,
			ValidHandle:            fsq.ValidateHandle(a.Handle) == nil,
			Counted:                true,
			UnreadCount:            a.UnreadCount,
			OldestUnreadAgeSeconds: a.OldestUnreadAgeSeconds,
			DLQCount:               a.DLQCount,
			PresenceStatus:         a.PresenceStatus,
			PresenceAgeSeconds:     a.PresenceAgeSeconds,
			PresenceSource:         a.PresenceSource,
			NotifierStatus:         a.NotifierStatus,
		})
	}
	var gate *board.Gate
	if g := out.Ops.OperatorGate; g != nil {
		gate = &board.Gate{Session: session, OpenCount: g.OpenCount, OldestAgeSeconds: g.OldestGateAgeSeconds}
	}
	return entries, gate, nil
}

const reservedHuman = "user"

type participant struct {
	Project string `json:"project"`
	Session string `json:"session"`
	Handle  string `json:"handle"`
}

type recipe struct {
	Kind   string   `json:"kind"`
	Labels []string `json:"labels"`
}

type threadOpenDoc struct {
	Schema      string `json:"schema"`
	Obligations []struct {
		Key    string `json:"key"`
		Thread string `json:"thread"`
		Opener struct {
			ID      string   `json:"id"`
			Kind    string   `json:"kind"`
			Subject string   `json:"subject"`
			Created string   `json:"created"`
			Labels  []string `json:"labels"`
		} `json:"opener"`
		OwedBy           participant `json:"owed_by"`
		OwedByKind       string      `json:"owed_by_kind"`
		OwedTo           participant `json:"owed_to"`
		ReplyDestination *struct {
			Handle  string `json:"handle"`
			Session string `json:"session"`
			Project string `json:"project"`
		} `json:"reply_destination"`
		State        string `json:"state"`
		Completion   recipe `json:"completion"`
		Acknowledge  recipe `json:"acknowledge"`
		LastActivity *struct {
			Kind    string `json:"kind"`
			From    string `json:"from"`
			Created string `json:"created"`
		} `json:"last_activity"`
	} `json:"obligations"`
	Coverage struct {
		AssessedSince     string            `json:"assessed_since"`
		Skipped           []json.RawMessage `json:"skipped"`
		UnresolvedLineage []json.RawMessage `json:"unresolved_lineage"`
		Ambiguous         []json.RawMessage `json:"ambiguous"`
		OutboundRouted    []struct {
			Count int `json:"count"`
		} `json:"outbound_routed"`
		TruncatedByLimit struct {
			Omitted int `json:"omitted"`
		} `json:"truncated_by_limit"`
	} `json:"coverage"`
}

func parseThreadOpen(data []byte) ([]board.Item, board.Coverage, string, error) {
	var doc threadOpenDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, board.Coverage{}, "", err
	}
	if doc.Schema != ThreadOpenSchema {
		return nil, board.Coverage{}, "", fmt.Errorf("schema %q, want %q", doc.Schema, ThreadOpenSchema)
	}
	var cov board.Coverage
	var gaps []string
	if t, err := time.Parse(time.RFC3339Nano, doc.Coverage.AssessedSince); err == nil {
		cov.Window = "obligations since " + t.UTC().Format("2006-01-02")
		gaps = append(gaps, "obligations before "+t.UTC().Format("2006-01-02"))
	}
	cov.Skipped = len(doc.Coverage.Skipped)
	cov.Unresolved = len(doc.Coverage.UnresolvedLineage) + len(doc.Coverage.Ambiguous)
	cov.Truncated = doc.Coverage.TruncatedByLimit.Omitted
	if cov.Skipped > 0 {
		gaps = append(gaps, fmt.Sprintf("%d unreadable messages", cov.Skipped))
	}
	if n := len(doc.Coverage.UnresolvedLineage); n > 0 {
		gaps = append(gaps, fmt.Sprintf("%d messages with unresolved lineage", n))
	}
	if n := len(doc.Coverage.Ambiguous); n > 0 {
		gaps = append(gaps, fmt.Sprintf("%d ambiguous replies", n))
	}
	if cov.Truncated > 0 {
		gaps = append(gaps, fmt.Sprintf("%d obligations over the limit", cov.Truncated))
	}
	routed := 0
	for _, r := range doc.Coverage.OutboundRouted {
		routed += r.Count
	}
	if routed > 0 {
		gaps = append(gaps, fmt.Sprintf("%d requests sent to other projects", routed))
	}
	cov.Detail = strings.Join(gaps, "; ")

	var items []board.Item
	for _, o := range doc.Obligations {
		ref, ok := board.Obligation(o.Key)
		if !ok || fsq.ValidateHandle(o.OwedBy.Handle) != nil {
			cov.Skipped++
			continue
		}
		owner := toOwner(o.OwedBy)
		if o.OwedByKind == "human" {
			owner.Kind = board.OwnerHuman
		}
		requester := toOwner(o.OwedTo)
		created, _ := time.Parse(time.RFC3339Nano, o.Opener.Created)
		state := board.StateActive
		if owner.Kind == board.OwnerHuman {
			state = board.StateNeedsHuman
		}
		title := o.Opener.Subject
		if title == "" {
			title = o.Opener.Kind + " from " + requester.Display
		}
		it := board.Item{
			Ref:        ref,
			Title:      title,
			State:      state,
			Owner:      owner,
			Requester:  &requester,
			WaitsOn:    []board.Owner{owner},
			Reason:     board.Reason{Field: "kind", Text: o.Opener.Kind + " from " + requester.Display, Since: created},
			UpdatedAt:  created,
			Structural: true,
			Evidence: []board.Evidence{
				{Field: "kind", Value: o.Opener.Kind, Text: o.Opener.Subject, At: created},
				{Field: "state", Value: o.State, At: created},
			},
			Obligation: &board.ObligationFacts{
				OpenerID:         o.Opener.ID,
				Thread:           o.Thread,
				OwnerSession:     o.OwedBy.Session,
				CompletionKind:   o.Completion.Kind,
				CompletionLabels: o.Completion.Labels,
				AcknowledgeKind:  o.Acknowledge.Kind,
			},
		}
		if d := o.ReplyDestination; d != nil {
			it.Obligation.Destination = &board.Destination{Handle: d.Handle, Session: d.Session, Project: d.Project}
		}
		if la := o.LastActivity; la != nil {
			if t, err := time.Parse(time.RFC3339Nano, la.Created); err == nil {
				it.UpdatedAt = t
				it.Evidence = append(it.Evidence, board.Evidence{Field: "last_activity", Value: la.Kind, Text: "from " + la.From, At: t})
			}
		}
		if th, ok := board.AMQThread(sessionKey(o.OwedBy.Session), o.Thread); ok {
			it.Thread = &th
		}
		for _, tok := range board.BeadTokens(o.Opener.Subject) {
			it.Mentions = append(it.Mentions, board.Mention{Token: tok, Basis: "subject mention"})
		}
		for _, l := range o.Opener.Labels {
			for _, tok := range board.BeadTokens(l) {
				it.Mentions = append(it.Mentions, board.Mention{Token: tok, Basis: "label"})
			}
		}
		for _, tok := range board.BeadTokens(o.Thread) {
			it.Mentions = append(it.Mentions, board.Mention{Token: tok, Basis: "thread name"})
		}
		for _, fr := range board.ForgeURLRefs(o.Opener.Subject) {
			it.Relations = append(it.Relations, board.Relation{Kind: board.RelationRelated, To: fr, Basis: "subject mention", Inferred: true})
		}
		items = append(items, it)
	}
	return items, cov, doc.Schema, nil
}

// sessionKey names a base root "." as obligation keys do.
func sessionKey(session string) string {
	if session == "" {
		return "."
	}
	return session
}

func toOwner(p participant) board.Owner {
	o := board.Owner{Kind: board.OwnerAgent, Handle: p.Handle, Session: p.Session, Display: p.Handle}
	if p.Project != "" {
		o.Display = p.Handle + "@" + p.Project
	}
	if p.Handle == reservedHuman && p.Project == "" {
		o.Kind = board.OwnerHuman
	}
	if fsq.ValidateHandle(p.Handle) != nil {
		o = board.Owner{Kind: board.OwnerUnmapped, Display: p.Handle}
	}
	return o
}

// errLine keeps the first line of a command error for the sources strip.
func errLine(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
