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
}

func (f fakeRunner) Run(_ context.Context, _ string, argv []string) ([]byte, error) {
	if f.failAMQ && argv[0] == "amq" {
		return nil, errors.New("amq: base root not found")
	}
	key := strings.Join(argv, " ")
	name, ok := f.outputs[key]
	if !ok {
		f.t.Errorf("unexpected command: %s", key)
		return nil, errors.New("unexpected command")
	}
	return os.ReadFile(filepath.Join("testdata", name))
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
		refT5:           board.ColumnWaiting,   // rule 1: obligation owed by user
		"bead:demo-b1":  board.ColumnWaiting,   // rule 2: blocked, no owner, nothing to wait on
		"bead:demo-b2":  board.ColumnBlocked,   // rule 3: blocks dependency on an open bead
		"bead:demo-b10": board.ColumnBlocked,   // a deferred blocker still blocks
		"bead:demo-b3":  board.ColumnInFlight,  // rule 4: in progress
		refR1:           board.ColumnInFlight,  // rule 4: obligation owed by an agent
		"bead:demo-b5":  board.ColumnUnclaimed, // rule 5: open, no assignee
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
	if first := column(m, board.ColumnWaiting).Cards[0].ID; first != "bead:demo-b1" {
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
