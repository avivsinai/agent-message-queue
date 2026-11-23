package board

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

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

func noticeStore(t *testing.T) Store {
	t.Helper()
	root := t.TempDir()
	id, err := fsq.StableTreeIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(root, id, "00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func noticeTestPlan(t *testing.T, store Store) (NotifyPlan, *noticeRunner) {
	t.Helper()
	r := &noticeRunner{}
	return NotifyPlan{Sender: "floor", Recipients: []string{"lead", "alt"}, Session: "s1", Config: DefaultConfig(), Env: Env{Run: r}, Store: store, Roster: []RosterEntry{
		{Session: "s1", Handle: "floor", ValidHandle: true}, {Session: "s1", Handle: "lead", ValidHandle: true}, {Session: "s1", Handle: "alt", ValidHandle: true},
	}}, r
}

func publishNotificationNews(t *testing.T, ctx context.Context, store Store, id string) {
	t.Helper()
	initial, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base := store.BaseRoot
	projectID := initial.ProjectID
	now := time.Now().UTC().Add(-time.Second)
	entry := thread.Entry{ID: id, From: "lead", To: []string{"worker"}, Thread: "work", Subject: "first news", Kind: "todo", Created: now.Add(-time.Hour).Format(time.RFC3339), NewsBy: now.Add(-time.Minute).Format(time.RFC3339)}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	a := Acquisition{ProjectID: projectID, Input: Input{Results: []NamedResult{{Name: "amq-history", Result: Result{Status: StatusOK, ObservedAt: now}}}}, Messages: []Message{MessageFromEntry(base, base, "", entry, evidenceDigest(raw))}, Evidence: [][]byte{raw}}
	if err := store.Commit(ctx, a); err != nil {
		t.Fatal(err)
	}
}

// The happy path publishes and reads real source history before sending.
func TestNotifyReadsUnderItsLock(t *testing.T) {
	store := noticeStore(t)
	plan, r := noticeTestPlan(t, store)
	ctx := context.Background()
	publishNotificationNews(t, ctx, store, "prior-floor-incident")
	if report := Notify(ctx, plan); report.Err != nil || len(r.calls) != 2 {
		t.Fatalf("initial observation: %+v %v", r.calls, report.Err)
	}
	held, ok, err := lock.TryExclusive(filepath.Join(filepath.Dir(store.Dir), "notify.lock"))
	if err != nil || !ok {
		t.Fatalf("hold notifier lock: %v", err)
	}
	answer := make(chan NotifyReport, 1)
	go func() { answer <- Notify(ctx, plan) }()
	// This later run observes the later publication, not facts captured before
	// it queued behind the earlier caller.
	publishNotificationNews(t, ctx, store, "later-floor-incident")
	held()
	report := <-answer
	if report.Err != nil || len(report.Problems) != 2 || len(r.calls) != 4 {
		t.Fatalf("new incident pruned or missed: %+v %+v %v", r.calls, report.Problems, report.Err)
	}
}

func TestNotifyIncidentDeliverySet(t *testing.T) {
	ctx := context.Background()
	store := noticeStore(t)
	plan, r := noticeTestPlan(t, store)
	base := store.BaseRoot
	now := time.Now().UTC().Add(-time.Second)
	entry := thread.Entry{ID: "news", From: "lead", To: []string{"worker"}, Thread: "work", Subject: "first news", Kind: "todo", Created: now.Add(-time.Hour).Format(time.RFC3339), NewsBy: now.Add(-time.Minute).Format(time.RFC3339)}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	acquisition := Acquisition{ProjectID: initial.ProjectID, Input: Input{Results: []NamedResult{{Name: "amq-history", Result: Result{Status: StatusOK, ObservedAt: now}}}}, Messages: []Message{MessageFromEntry(base, base, "", entry, evidenceDigest(raw))}, Evidence: [][]byte{raw}}
	if err := store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	report := Notify(ctx, plan)
	if report.Err != nil {
		t.Fatal(report.Err)
	}
	if len(r.calls) != 2 || len(report.Problems) != 1 || report.Problems[0].Kind != ProblemNewsOverdue {
		t.Fatalf("initial notification: %+v %+v", r.calls, report)
	}
	for _, call := range r.calls {
		if !strings.Contains(strings.Join(call.argv, " "), " --kind status --labels floor --subject Floor: news_overdue --body -") || call.body != report.Problems[0].Text {
			t.Fatalf("actual send: %+v", call)
		}
	}
	if report := Notify(ctx, plan); report.Err != nil || len(r.calls) != 2 {
		t.Fatalf("second run resent = %+v, %v", r.calls, report.Err)
	}
	// A real reply settles the first request; a new request reissues delivery.
	reply := thread.Entry{ID: "reply", From: "worker", To: []string{"lead"}, Thread: "work", Subject: "started", Kind: "status", Created: now.Format(time.RFC3339), Refs: []string{"news"}}
	rawReply, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	acquisition.Messages = append(acquisition.Messages, MessageFromEntry(base, base, "", reply, evidenceDigest(rawReply)))
	acquisition.Evidence = append(acquisition.Evidence, rawReply)
	if err := store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	if report := Notify(ctx, plan); report.Err != nil || len(r.calls) != 2 {
		t.Fatalf("answer did not settle: %+v %v", r.calls, report.Err)
	}
	fresh := thread.Entry{ID: "fresh-news", From: "lead", To: []string{"worker"}, Thread: "another", Subject: "next work", Kind: "todo", Created: now.Format(time.RFC3339), NewsBy: now.Add(-time.Minute).Format(time.RFC3339)}
	rawFresh, err := json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	acquisition.Messages = append(acquisition.Messages, MessageFromEntry(base, base, "", fresh, evidenceDigest(rawFresh)))
	acquisition.Evidence = append(acquisition.Evidence, rawFresh)
	if err := store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	if report := Notify(ctx, plan); report.Err != nil || len(r.calls) != 4 {
		t.Fatalf("recurring incident not sent: %+v %v", r.calls, report.Err)
	}
}

func TestNotifyReadErrorRetainsDeliveredPairs(t *testing.T) {
	ctx := context.Background()
	plan, r := noticeTestPlan(t, noticeStore(t))
	pairs := deliveredSet{Sent: []deliveredEntry{plan.deliveredPair((Problem{Kind: ProblemBoardStale}).Incident(), "lead"), plan.deliveredPair((Problem{Kind: ProblemBoardStale}).Incident(), "alt")}}
	if err := plan.Store.writeDelivered(map[deliveredEntry]bool{pairs.Sent[0]: true, pairs.Sent[1]: true}); err != nil {
		t.Fatal(err)
	}
	state, err := plan.Store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation++
	state.PublishedAt = time.Now().UTC()
	state.Schema = Schema
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["tasks"] = "wrong type"
	data, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Store.writeFile("snapshot.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	report := Notify(ctx, plan)
	if report.Err == nil || len(r.calls) != 0 {
		t.Fatalf("partial state generated notices: %+v %v", r.calls, report.Err)
	}
	delivered, err := plan.Store.readDelivered()
	if err != nil || len(delivered) != 2 {
		t.Fatalf("read error pruned pairs: %+v %v", delivered, err)
	}
}

func TestNotifyAbsentStoreDoesNotBlockInitialization(t *testing.T) {
	ctx := context.Background()
	plan, r := noticeTestPlan(t, noticeStore(t))
	if err := os.RemoveAll(plan.Store.Dir); err != nil {
		t.Fatal(err)
	}
	report := Notify(ctx, plan)
	if report.Err == nil || len(r.calls) != 0 {
		t.Fatalf("uninitialized board notified: %v %+v", report.Err, r.calls)
	}
	if err := plan.Store.Init(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyRetriesOnlyFailedRecipientAndScopesSessions(t *testing.T) {
	plan, r := noticeTestPlan(t, noticeStore(t))
	publishNotificationNews(t, context.Background(), plan.Store, "request-retry")
	r.failTo = "lead"
	if report := Notify(context.Background(), plan); report.Err == nil || len(r.calls) != 2 {
		t.Fatalf("first stale attempt: %+v %v", r.calls, report.Err)
	}
	r.failTo = ""
	other := plan
	other.Session, other.Roster = "s2", []RosterEntry{{Session: "s2", Handle: "floor", ValidHandle: true}, {Session: "s2", Handle: "lead", ValidHandle: true}, {Session: "s2", Handle: "alt", ValidHandle: true}}
	if report := Notify(context.Background(), other); report.Err != nil || len(r.calls) != 4 {
		t.Fatalf("other-root pair was wrongly suppressed: %+v %v", r.calls, report.Err)
	}
	if report := Notify(context.Background(), plan); report.Err != nil || len(r.calls) != 5 || r.calls[4].argv[5] != "lead" {
		t.Fatalf("retry was not independently recorded: %+v %v", r.calls, report.Err)
	}
	delivered, err := plan.Store.readDelivered()
	if err != nil || len(delivered) != 4 {
		t.Fatalf("session entries pruned or missing: %+v %v", delivered, err)
	}
}

func TestNotifyBeforeSend(t *testing.T) {
	for _, row := range []struct{ name, sender, recipient, session string }{
		{"unknown sender", "worker", "lead", "s1"}, {"unknown recipient", "floor", "worker", "s1"}, {"same handle", "floor", "floor", "s1"}, {"wrong session", "floor", "lead", "s2"},
	} {
		t.Run(row.name, func(t *testing.T) {
			plan, r := noticeTestPlan(t, noticeStore(t))
			plan.Sender, plan.Recipients, plan.Session = row.sender, []string{row.recipient}, row.session
			if report := Notify(context.Background(), plan); report.Err == nil || len(r.calls) != 0 {
				t.Fatalf("invalid route delivered before refusal: %+v", r.calls)
			}
		})
	}
}
