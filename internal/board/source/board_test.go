package source_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/source"
)

// fakeRunner answers each argv with a captured payload from testdata. An
// argv it does not know fails the test, so the sources run exactly the
// read-only commands listed here.
type fakeRunner struct {
	t       *testing.T
	outputs map[string]string // argv joined by spaces -> testdata file
	failAMQ bool
	calls   map[string]int                 // optional: calls per key
	edit    map[string]func(string) string // argv -> rewrite of its payload
	fail    map[string]error               // argv -> command error
}

func (f fakeRunner) RunLimited(ctx context.Context, dir string, argv []string, _ int) ([]byte, error) {
	return f.Run(ctx, dir, argv)
}

func (f fakeRunner) Run(_ context.Context, _ string, argv []string) ([]byte, error) {
	if f.failAMQ && argv[0] == "amq" {
		return nil, errors.New("amq: base root not found")
	}
	key := strings.Join(argv, " ")
	// The query text is long; one key stands for it, plus the search cursor
	// of a follow-up page. Every call must name the host (Codex review of
	// PR #1200: GH_HOST could redirect the call).
	if strings.HasPrefix(key, "gh api graphql --hostname github.com ") {
		key = "gh api graphql"
		for _, a := range argv {
			if after, ok := strings.CutPrefix(a, "after="); ok {
				key += " after=" + after
			}
		}
	}
	if f.calls != nil {
		f.calls[key]++
	}
	if err := f.fail[key]; err != nil {
		return nil, err
	}
	name, ok := f.outputs[key]
	if !ok {
		f.t.Errorf("unexpected command: %s", key)
		return nil, errors.New("unexpected command")
	}
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if edit := f.edit[key]; edit != nil && err == nil {
		data = []byte(edit(string(data)))
	}
	return data, err
}

func outputs(threadOpen string) map[string]string {
	return map[string]string{
		"amq env --json":          "env.json",
		"amq session list --json": "session-list.json",
		"amq who --json":          "who.json",
		"amq doctor --ops --json --root /fixture/.agent-mail/s1":                                           "doctor-s1.json",
		"amq doctor --ops --json --root /fixture/.agent-mail/s2":                                           "doctor-s2.json",
		"amq thread open --all-sessions --json":                                                            threadOpen,
		"bd --readonly --sandbox list --json --brief --status open,in_progress,blocked,deferred --limit 0": "bd-list.json",
	}
}

func runBoard(t *testing.T, r fakeRunner, bdInstalled bool) board.Model {
	t.Helper()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	env := board.Env{
		Dir: "/fixture",
		Now: func() time.Time { return now },
		Run: r,
		LookPath: func(name string) (string, error) {
			if name == "bd" && !bdInstalled {
				return "", errors.New("not found")
			}
			return "/bin/" + name, nil
		},
		IsDir: func(string) bool { return true },
	}
	return board.Run(context.Background(), env, []board.Source{source.AMQ{}, source.Beads{}}, board.DefaultConfig(), "test")
}

func column(m board.Model, id board.Column) board.ColumnView {
	for _, c := range m.Columns {
		if c.ID == id {
			return c
		}
	}
	return board.ColumnView{}
}

func cardsByRef(m board.Model) map[string]board.Card {
	out := map[string]board.Card{}
	for _, c := range m.Columns {
		for _, card := range c.Cards {
			out[card.ID] = card
		}
	}
	return out
}

func source_(m board.Model, name string) board.SourceView {
	for _, s := range m.Sources {
		if s.Name == name {
			return s
		}
	}
	return board.SourceView{}
}

const (
	refT5   = "amq-obligation:s2/2026-10-22T08-00-00.000Z_pid101_aa11bb22/user"
	refTodo = "amq-obligation:s1/2026-10-22T09-00-00.000Z_pid102_cc33dd44/user"
	refR1   = "amq-obligation:s1/2026-10-22T10-00-00.000Z_pid103_ee55ff66/beta"
	refR2   = "amq-obligation:s1/2026-10-22T10-30-00.000Z_pid104_a1b2c3d4/beta"
)

func TestBoardPlacesCapturedAMQAndBeadsPayloads(t *testing.T) {
	m := runBoard(t, fakeRunner{t: t, outputs: outputs("thread-open.json")}, true)
	cards := cardsByRef(m)

	// Each rule of the column table, from real row shapes.
	want := map[string]board.Column{
		refT5: board.ColumnWaiting, // rule 1: obligation owed by user
		// Installed board: five blocked approval features with no assignee or
		// named blocker appeared as owner questions after the owner declined them.
		"bead:demo-b1":  board.ColumnBlocked,
		"bead:demo-b2":  board.ColumnBlocked,      // rule 3: blocks dependency on an open bead
		"bead:demo-b10": board.ColumnBlocked,      // a deferred blocker still blocks
		"bead:demo-b3":  board.ColumnInFlight,     // rule 4: in progress
		refR1:           board.Column("requests"), // an unanswered request does not prove work started
		"bead:demo-b5":  board.ColumnUnclaimed,    // rule 5: open, no assignee
		"bead:demo-b8":  board.ColumnUnclaimed,    // assigned open work has not started
		// Pro finding 5: an agent item waiting on the human, and a
		// human-owned blocked bead, are both the human's queue.
		"bead:demo-b7": board.ColumnWaiting,
		"bead:demo-b6": board.ColumnWaiting,
		// Pro finding 5: an unmapped assignee is held, not unclaimed.
		"bead:demo-b4": board.ColumnInFlight,
	}
	for ref, col := range want {
		card, ok := cards[ref]
		if !ok {
			t.Errorf("%s: no card", ref)
			continue
		}
		if card.Column != col {
			t.Errorf("%s: column %s, want %s", ref, card.Column, col)
		}
	}
	for _, action := range m.Next {
		if action.CardID == "bead:demo-b1" {
			t.Error("unassigned blocked work appears in Next without a named blocker")
		}
	}
	if _, ok := cards["bead:demo-b9"]; ok {
		t.Error("deferred bead is shown on the columns")
	}
	if marks := strings.Join(cards["bead:demo-b4"].Marks, ","); !strings.Contains(marks, "assignee not mapped: Jane Doe") {
		t.Errorf("demo-b4 marks = %q", marks)
	}

	// Pro finding 1: two obligations in one thread stay two cards with a
	// related token; a bead in an AMQ subject is related, never merged.
	r1, r2 := cards[refR1], cards[refR2]
	if len(r1.Items) != 1 || len(r2.Items) != 1 {
		t.Fatalf("thread obligations merged: %d and %d items", len(r1.Items), len(r2.Items))
	}
	if !hasToken(r2, "amq-thread:s1/p2p/alpha__beta", "same thread as 1 other obligation") {
		t.Errorf("r2 related = %+v", r2.Related)
	}
	if !hasToken(r1, "bead:demo-b3", "subject mention") {
		t.Errorf("r1 related = %+v", r1.Related)
	}
	if len(cards["bead:demo-b3"].Items) != 1 {
		t.Error("mentioned bead merged into the obligation card")
	}

	// Pro finding 4: the complete action answers T5 in s1, where the
	// requester is, with --kind from completion; a todo completes with
	// --labels done, and acknowledge is a separate command.
	if got := actionCommand(cards[refT5], "complete"); got != "amq send --session s1 --me user --to alpha --thread p2p/alpha__user --refs 2026-10-22T08-00-00.000Z_pid101_aa11bb22 --kind answer --body -" {
		t.Errorf("T5 complete = %q", got)
	}
	if got := actionCommand(cards[refTodo], "complete"); !strings.Contains(got, "--kind status --labels done --body -") {
		t.Errorf("todo complete = %q", got)
	}
	if got := actionCommand(cards[refTodo], "acknowledge"); !strings.Contains(got, "--kind status --body -") {
		t.Errorf("todo acknowledge = %q", got)
	}

	// Waiting on you sorts oldest reason first and keeps the unread-for-user
	// counts apart from its cards.
	if first := column(m, board.ColumnWaiting).Cards[0].ID; first != "bead:demo-b6" {
		t.Errorf("first waiting card = %s", first)
	}
	if len(m.Gates) != 2 || m.Gates[0].Session != "s1" || m.Gates[0].OpenCount != 1 {
		t.Errorf("gates = %+v", m.Gates)
	}

	// The roster is keyed by session: alpha is live in s1 and quiet in s2.
	if got := liveness(m, "s1", "alpha"); got != "live" {
		t.Errorf("s1/alpha = %q", got)
	}
	if got := liveness(m, "s2", "alpha"); got != "quiet" {
		t.Errorf("s2/alpha = %q", got)
	}
}

// Pro finding 13: with AMQ failing, beads cards still place and the roster
// says why it is missing.
func TestBoardKeepsBeadsCardsWhenAMQFails(t *testing.T) {
	m := runBoard(t, fakeRunner{t: t, outputs: outputs("thread-open.json"), failAMQ: true}, true)
	if s := source_(m, "amq"); s.Status != board.StatusDegraded {
		t.Errorf("amq status = %s", s.Status)
	}
	if card, ok := cardsByRef(m)["bead:demo-b5"]; !ok || card.Column != board.ColumnUnclaimed {
		t.Errorf("demo-b5 not in Unclaimed: %+v", card)
	}
	if !strings.Contains(m.RosterNote, "AMQ unavailable") {
		t.Errorf("roster note = %q", m.RosterNote)
	}
}

// Pro finding 8: an empty Waiting on you column says what was not assessed
// instead of "nothing".
func TestBoardEmptyWaitingNamesCoverageGap(t *testing.T) {
	m := runBoard(t, fakeRunner{t: t, outputs: outputs("thread-open-agents-only.json")}, false)
	if s := source_(m, "beads"); s.Status != board.StatusUnavailable {
		t.Errorf("beads status = %s", s.Status)
	}
	waiting := column(m, board.ColumnWaiting)
	if len(waiting.Cards) != 0 {
		t.Fatalf("waiting cards = %d", len(waiting.Cards))
	}
	for _, want := range []string{"Not assessed", "obligations before 2026-10-08", "1 ambiguous replies"} {
		if !strings.Contains(waiting.Empty, want) {
			t.Errorf("empty state %q lacks %q", waiting.Empty, want)
		}
	}
}

// Regressions from the Codex review of PR #1172: each row is one P2 finding.
func TestBoardSourceDefects(t *testing.T) {
	replace := func(old, new string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, old) {
				panic("fixture lacks " + old)
			}
			return strings.Replace(s, old, new, 1)
		}
	}
	for _, tc := range []struct {
		name  string
		edit  map[string]func(string) string
		fail  map[string]error
		check func(t *testing.T, m board.Model)
	}{
		{
			// P2 1: an inbox doctor could not read looked empty and counted.
			// Live board 2026-10-25: one incomplete mailbox marked the whole
			// AMQ source degraded; it is a coverage note on an ok source.
			name: "unreadable inbox is not counted",
			edit: map[string]func(string) string{"amq doctor --ops --json --root /fixture/.agent-mail/s1": replace(
				`{"handle": "alpha", "unread_count": 0,`,
				`{"handle": "alpha", "unread_count": 0, "unread_count_error": "open inbox/new: permission denied",`)},
			check: func(t *testing.T, m board.Model) {
				if s := source_(m, "amq"); s.Status != board.StatusOK || !s.Partial || !strings.Contains(s.Coverage.Detail, "1 mailbox not counted: s1/alpha") {
					t.Errorf("amq source = %s partial=%v %q", s.Status, s.Partial, s.Coverage.Detail)
				}
				if a, ok := rosterAgent(m, "s1", "alpha"); !ok || a.Counted {
					t.Errorf("s1/alpha found=%v counted=%v", ok, a.Counted)
				}
				var b strings.Builder
				if err := board.WriteText(&b, m); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(b.String(), "alpha · live · not counted: open inbox/new: permission denied") {
					t.Errorf("roster text lacks the count error:\n%s", b.String())
				}
			},
		},
		{
			// P2 2: a failed amq env left the source healthy with no peers.
			name: "failed env degrades the source and keeps its data",
			fail: map[string]error{"amq env --json": errors.New("amq env: config unreadable")},
			check: func(t *testing.T, m board.Model) {
				if s := source_(m, "amq"); s.Status != board.StatusDegraded || !strings.Contains(s.Detail, "config unreadable") {
					t.Errorf("amq source = %s %q", s.Status, s.Detail)
				}
				if _, ok := cardsByRef(m)[refT5]; !ok {
					t.Error("obligation card dropped")
				}
			},
		},
		{
			// P2 3: a thread named "$USER" was rendered for shell expansion.
			name: "source value equal to the user placeholder is quoted",
			edit: map[string]func(string) string{"amq thread open --all-sessions --json": replace(
				`"thread": "p2p/alpha__user"`, `"thread": "\"$USER\""`)},
			check: func(t *testing.T, m board.Model) {
				cards := cardsByRef(m)
				if got := actionCommand(cards[refT5], "complete"); !strings.Contains(got, `--thread '"$USER"'`) {
					t.Errorf("T5 complete = %q", got)
				}
				if got := actionCommand(cards["bead:demo-b5"], "claim"); !strings.HasSuffix(got, `--assignee "$USER"`) {
					t.Errorf("demo-b5 claim = %q", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := runBoard(t, fakeRunner{t: t, outputs: outputs("thread-open.json"), edit: tc.edit, fail: tc.fail}, true)
			tc.check(t, m)
		})
	}
}

func rosterAgent(m board.Model, session, handle string) (board.RosterAgent, bool) {
	for _, rs := range m.Roster {
		if rs.Session != session {
			continue
		}
		for _, a := range append(append([]board.RosterAgent{}, rs.Agents...), rs.Quiet...) {
			if a.Handle == handle {
				return a, true
			}
		}
	}
	return board.RosterAgent{}, false
}

func hasToken(c board.Card, ref, basis string) bool {
	for _, tok := range c.Related {
		if tok.Ref.String() == ref && tok.Basis == basis {
			return true
		}
	}
	return false
}

func actionCommand(c board.Card, prefix string) string {
	for _, a := range c.Actions {
		if strings.HasPrefix(a.Label, prefix) {
			return a.Command
		}
	}
	return ""
}

func liveness(m board.Model, session, handle string) string {
	for _, rs := range m.Roster {
		if rs.Session != session {
			continue
		}
		for _, a := range append(rs.Agents, rs.Quiet...) {
			if a.Handle == handle {
				return a.Liveness
			}
		}
	}
	return ""
}
