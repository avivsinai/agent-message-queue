package requests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

func fixedClock() time.Time { return time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC) }

func newRecord(id string) *Record {
	return &Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   id,
			CreatorHost: "hostA",
			TargetID:    "t_fake1",
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			InputDigest: Digest([]byte("say hi")),
		},
		Input: &protocol.SubmitInput{Text: "say hi"},
	}
}

// TestRecordLifecycle is the happy path: create, advance through the state
// graph, read back, and compact a completed result into a tombstone that
// still answers result_expired.
func TestRecordLifecycle(t *testing.T) {
	s, err := Open(t.TempDir(), WithClock(fixedClock))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	rec := newRecord("11111111-1111-4111-8111-111111111101")
	if err := s.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rec.RequestRef == "" {
		t.Fatal("create did not assign request_ref")
	}
	rec.Revision, rec.State = 2, protocol.StateDispatching
	if err := s.Update(rec); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
	run := "run_1"
	rec.Revision, rec.State, rec.NativeRun, rec.NativeDispatches = 3, protocol.StateRunning, &run, 1
	if err := s.Update(rec); err != nil {
		t.Fatalf("running: %v", err)
	}
	rec.Revision, rec.State = 4, protocol.StateCompleted
	rec.Result = &protocol.Result{Text: "hi"}
	rec.ObservedAt = "2026-09-01T00:00:00Z"
	if err := s.Update(rec); err != nil {
		t.Fatalf("completed: %v", err)
	}

	got, ok, err := s.Get(Key{"hostA", "t_fake1", rec.RequestID})
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.State != protocol.StateCompleted || got.Result == nil || got.Result.Text != "hi" || got.Revision != 4 {
		t.Fatalf("unexpected record: %+v", got.Snapshot)
	}

	n, err := s.Compact(fixedClock())
	if err != nil || n != 1 {
		t.Fatalf("compact: n=%d err=%v", n, err)
	}
	got, _, err = s.Get(Key{"hostA", "t_fake1", rec.RequestID})
	if err != nil {
		t.Fatalf("get after compact: %v", err)
	}
	if !got.Tombstone || got.Result != nil || got.Code != protocol.CodeResultExpired || got.State != protocol.StateCompleted || got.InputDigest == "" {
		t.Fatalf("compaction lost identity or kept the result: %+v", got)
	}
}

// TestStoreRefusesSecondWriterAndBadTransitions pins the two invariants the
// design relies on: one endpoint per root, and no state graph shortcuts.
func TestStoreRefusesSecondWriterAndBadTransitions(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, WithClock(fixedClock))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := Open(dir); protocol.ExitCode(err) != protocol.ExitActionRequired {
		t.Fatalf("second writer: want action-required refusal, got %v", err)
	}

	rec := newRecord("11111111-1111-4111-8111-111111111102")
	if err := s.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	bad := *rec
	bad.Revision, bad.State = 2, protocol.StateCompleted
	if err := s.Update(&bad); err == nil {
		t.Fatal("received -> completed was accepted")
	}
	bad = *rec
	bad.Revision, bad.State, bad.NativeDispatches = 2, protocol.StateDispatching, 2
	if err := s.Update(&bad); err == nil {
		t.Fatal("two native dispatches were accepted")
	}
	if err := s.Create(rec); protocol.ExitCode(err) != protocol.ExitActionRequired {
		t.Fatalf("duplicate create: want request_conflict, got %v", err)
	}
}

// TestListIsolatesPoisonRecords pins B15: a corrupt record on disk must not
// abort List (and therefore Reconcile). The bad record is isolated, its
// dedup identity is recovered from the path, and every healthy record after
// it is still returned. A poison record also blocks a conflicting re-create
// (Get surfaces it as an error, not as absent).
func TestListIsolatesPoisonRecords(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, WithClock(fixedClock))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Two healthy records, one poison in between, one healthy after.
	good1 := newRecord("11111111-1111-4111-8111-111111111301")
	if err := s.Create(good1); err != nil {
		t.Fatalf("create good1: %v", err)
	}
	poisonKey := Key{CreatorHost: "hostA", TargetID: "t_fake1", RequestID: "11111111-1111-4111-8111-111111111302"}
	poisonPath, err := s.path(poisonKey)
	if err != nil {
		t.Fatalf("poison path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(poisonPath), dirMode); err != nil {
		t.Fatalf("mkdir poison dir: %v", err)
	}
	// Write a file that exists but is not a valid record.
	if err := os.WriteFile(poisonPath, []byte("{not valid json"), fileMode); err != nil {
		t.Fatalf("write poison: %v", err)
	}
	good2 := newRecord("11111111-1111-4111-8111-111111111303")
	if err := s.Create(good2); err != nil {
		t.Fatalf("create good2: %v", err)
	}

	// List must return the two healthy records and NOT abort on the poison one.
	recs, poison, err := s.ListWithPoison()
	if err != nil {
		t.Fatalf("list with poison: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 healthy records, got %d: %+v", len(recs), recs)
	}
	if len(poison) != 1 {
		t.Fatalf("want 1 poison record, got %d: %+v", len(poison), poison)
	}
	// The poison record's dedup identity is recovered from the path.
	p := poison[0]
	if p.Key != poisonKey {
		t.Fatalf("poison key: got %+v want %+v", p.Key, poisonKey)
	}
	if p.Path != poisonPath || p.Error == "" {
		t.Fatalf("poison diagnostic incomplete: path=%q err=%q", p.Path, p.Error)
	}

	// The plain List() returns only healthy records (no abort).
	recs2, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs2) != 2 {
		t.Fatalf("List() want 2 healthy records, got %d", len(recs2))
	}

	// A poison record blocks a conflicting re-create: Get surfaces the error
	// rather than treating the corrupt file as absent.
	_, exists, gerr := s.Get(poisonKey)
	if gerr == nil {
		t.Fatalf("Get on poison record: want error, got exists=%v nil", exists)
	}
	if exists {
		t.Fatalf("Get on poison record reported exists=true")
	}
}

// TestNormalizeRecordClearsStaleInteraction pins the shared state-normalization:
// a terminal record recovered from disk must not carry a pending interaction
// left by a crash between setting and clearing it.
func TestNormalizeRecordClearsStaleInteraction(t *testing.T) {
	rec := &Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   "11111111-1111-4111-8111-111111111401",
			CreatorHost: "hostA",
			TargetID:    "t_fake1",
			Epoch:       "e_1",
			Revision:    4,
			State:       protocol.StateCompleted,
			Interaction: &protocol.Interaction{InteractionID: "ix1", Kind: "question", Options: []string{"a"}},
		},
	}
	normalizeRecord(rec)
	if rec.Interaction != nil {
		t.Fatalf("terminal record kept a stale interaction: %+v", rec.Interaction)
	}
}
