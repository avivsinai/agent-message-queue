package board

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// All send attempts go through this in-memory stdin runner in turn order.
type noticeCall struct {
	argv []string
	body string
}

type noticeRunner struct {
	calls  []noticeCall
	failTo string
}

func (r *noticeRunner) Run(_ context.Context, _ string, _ []string) ([]byte, error) {
	panic("sender did not obtain its scoped stdin runner")
}

func (r *noticeRunner) RunLimited(ctx context.Context, dir string, argv []string, _ int) ([]byte, error) {
	return r.Run(ctx, dir, argv)
}

func (r *noticeRunner) WithInput(body io.Reader) Runner {
	raw, err := io.ReadAll(body)
	if err != nil {
		panic(err)
	}
	return &inputRunner{parent: r, body: string(raw)}
}

type inputRunner struct {
	parent *noticeRunner
	body   string
}

func (r *inputRunner) Run(_ context.Context, _ string, argv []string) ([]byte, error) {
	r.parent.calls = append(r.parent.calls, noticeCall{argv: slices.Clone(argv), body: r.body})
	if to := argv[5]; r.parent.failTo == "*" || to == r.parent.failTo {
		return nil, errors.New("delivery failed")
	}
	return nil, nil
}

func (r *inputRunner) RunLimited(ctx context.Context, dir string, argv []string, _ int) ([]byte, error) {
	return r.Run(ctx, dir, argv)
}

func noticeTestPlan(t *testing.T) (NotifyPlan, *noticeRunner) {
	t.Helper()
	r := &noticeRunner{}
	return NotifyPlan{
		Sender: "floor", Recipients: []string{"lead", "alt"}, Session: "s1", Env: Env{Run: r},
		Store: Store{Dir: t.TempDir()}, Roster: []RosterEntry{
			{Session: "s1", Handle: "floor", ValidHandle: true},
			{Session: "s1", Handle: "lead", ValidHandle: true},
			{Session: "s1", Handle: "alt", ValidHandle: true},
		},
	}, r
}

func TestNotifyIncidentDeliverySet(t *testing.T) {
	ctx := context.Background()
	plan, r := noticeTestPlan(t)
	problems := []Problem{{Kind: ProblemSeatStalled, Session: "s1", Handle: "worker", Text: "no output"}, {Kind: ProblemSeatAtLimit, Session: "s1", Handle: "lead", CardID: "c1", Text: "paused"}}
	send := func() error { return Notify(ctx, plan, false, problems) }
	if err := send(); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 4 {
		t.Fatalf("first run sends = %d, want 4", len(r.calls))
	}
	for i, call := range r.calls {
		want := "amq send --me floor --to " + []string{"lead", "alt"}[i%2] + " --kind status --labels floor --subject Floor: " + string(problems[i/2].Kind) + " --body -"
		if got := strings.Join(call.argv, " "); got != want {
			t.Fatalf("argv %d = %q, want %q", i, got, want)
		}
		if call.body != problems[i/2].Text {
			t.Fatalf("body %d = %q", i, call.body)
		}
	}
	if err := send(); err != nil || len(r.calls) != 4 {
		t.Fatalf("second run sends = %d, error %v; want none", len(r.calls), err)
	}
	problems[1].CardID, problems[1].Text = "c2", "later"
	if err := send(); err != nil || len(r.calls) != 4 {
		t.Fatalf("new example card or text reissued: %d %+v", len(r.calls), r.calls)
	}
	problems = []Problem{{Kind: ProblemSeatAtLimit, Session: "s1", Handle: "lead", CardID: "c2", Text: "later"}}
	if err := send(); err != nil {
		t.Fatal(err) // only the first incident cleared
	}
	problems = append(problems, Problem{Kind: ProblemSeatStalled, Session: "s1", Handle: "worker", Text: "no output"})
	if err := send(); len(r.calls) != 6 || err != nil {
		t.Fatalf("cleared incident returning = %d %v, want two sends", len(r.calls), err)
	}
	problems = []Problem{{Kind: ProblemSeatAtLimit, Session: "s1", Handle: "lead", CardID: "fresh", Text: "back"}}
	if err := send(); err != nil {
		t.Fatal(err)
	}
	before := len(r.calls)
	r.failTo = "alt"
	if err := Notify(ctx, plan, true, problems); err == nil || len(r.calls) != before+2 {
		t.Fatalf("stale delivery does not force exit 1: %d %v", len(r.calls), err)
	}
	manifest, err := plan.Store.readDelivered()
	if err != nil || !manifest[deliveredPair((Problem{Kind: ProblemBoardStale}).Incident(), "lead")] || manifest[deliveredPair((Problem{Kind: ProblemBoardStale}).Incident(), "alt")] {
		t.Fatalf("stale partial-success record: %+v %v", manifest, err)
	}
	r.failTo = ""
	if err := Notify(ctx, plan, true, problems); err != nil || len(r.calls) != before+3 {
		t.Fatal("stale retry did not independently retry failed pair")
	}
	if err := Notify(ctx, plan, false, problems); err != nil || len(r.calls) != before+3 {
		t.Fatal("stale run pruned the delivered set")
	}
}

func TestNotifyRetriesOnlyFailedRecipient(t *testing.T) {
	plan, r := noticeTestPlan(t)
	r.failTo = "lead"
	problem := []Problem{{Kind: ProblemSeatAtLimit, Session: "s1", Handle: "lead", CardID: "new", Text: "paused"}}
	if err := Notify(context.Background(), plan, false, problem); err == nil || len(r.calls) != 2 {
		t.Fatalf("first run = %+v %v", r.calls, err)
	}
	r.failTo = ""
	if err := Notify(context.Background(), plan, false, problem); err != nil || len(r.calls) != 3 {
		t.Fatalf("retry run = %+v %v", r.calls, err)
	}
	if got := r.calls[2].argv[5]; got != "lead" {
		t.Fatalf("retry recipient = %s, want only failed lead", got)
	}
}

func TestNotifyBeforeSend(t *testing.T) {
	for _, row := range []struct{ name, sender, recipient, session string }{
		{"unknown sender", "worker", "lead", "s1"},
		{"unknown recipient", "floor", "worker", "s1"},
		{"same handle", "floor", "floor", "s1"},
		{"wrong session", "floor", "lead", "s2"},
	} {
		t.Run(row.name, func(t *testing.T) {
			plan, r := noticeTestPlan(t)
			plan.Sender, plan.Recipients, plan.Session = row.sender, []string{row.recipient}, row.session
			if err := Notify(context.Background(), plan, false, nil); err == nil {
				t.Fatal("invalid route accepted")
			}
			if len(r.calls) != 0 {
				t.Fatal("sent before identity resolution")
			}
		})
	}
}
