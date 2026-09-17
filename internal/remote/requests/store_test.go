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
	rec.AckDigest = protocol.EvidenceDigest(rec.Result) // settled: ack matches the result
	if err := s.Update(rec); err != nil {
		t.Fatalf("completed: %v", err)
	}
	if err := s.MarkPublished(Key{"hostA", "t_fake1", rec.RequestID}, 4); err != nil {
		t.Fatalf("mark published: %v", err)
	}

	got, ok, err := s.Get(Key{"hostA", "t_fake1", rec.RequestID})
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.State != protocol.StateCompleted || got.Result == nil || got.Result.Text != "hi" || got.Revision != 4 {
		t.Fatalf("unexpected record: %+v", got.Snapshot)
	}

	n, err := s.Compact(fixedClock(), 1000)
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

// TestClosedStoreRejectsEveryMutation pins B13 store half: after Close, every
// mutation refuses with store_closed, so a stale reference cannot write after a
// replacement endpoint has taken ownership. Create, Update, MarkPublished, and
// Compact all go through write() and must refuse. A fresh Open after Close
// re-acquires the lock and can write again.
func TestClosedStoreRejectsEveryMutation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, WithClock(fixedClock))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	rec := newRecord("11111111-1111-4111-8111-111111111601")
	if err := s.Create(rec); err != nil {
		t.Fatalf("create before close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Every mutation must refuse with store_closed, not succeed silently.
	wantCode := protocol.CodeStoreClosed
	if got := protocol.ExitCode(s.Create(newRecord("11111111-1111-4111-8111-111111111602"))); got != protocol.ExitActionRequired {
		t.Fatalf("Create after close: want exit %d, got %d", protocol.ExitActionRequired, got)
	}
	if code := protocol.RefusalCode(s.Create(newRecord("11111111-1111-4111-8111-111111111603"))); code != wantCode {
		t.Fatalf("Create after close: want code %q, got %q", wantCode, code)
	}
	upd := *rec
	upd.Revision = 2
	upd.State = protocol.StateRunning
	if code := protocol.RefusalCode(s.Update(&upd)); code != wantCode {
		t.Fatalf("Update after close: want code %q, got %q", wantCode, code)
	}
	if code := protocol.RefusalCode(s.MarkPublished(keyOf(&upd), 1)); code != wantCode {
		t.Fatalf("MarkPublished after close: want code %q, got %q", wantCode, code)
	}
	if n, err := s.Compact(fixedClock(), 1000); err == nil {
		t.Fatalf("Compact after close: want error, got n=%d", n)
	} else if code := protocol.RefusalCode(err); code != wantCode {
		t.Fatalf("Compact after close: want code %q, got %q", wantCode, code)
	}
	// A fresh Open after Close re-acquires the lock and can write again.
	s2, err := Open(dir, WithClock(fixedClock))
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if err := s2.Create(newRecord("11111111-1111-4111-8111-111111111604")); err != nil {
		t.Fatalf("create after reopen: %v", err)
	}
}

// TestBK4AggregateQuotaRefusesBeforeWrite is the 611.22.19 BK4 happy path for
// the aggregate quota: a store at quota refuses a new record with storage_full
// BEFORE the write lands, leaves the existing record intact, and never evicts
// a dedup tombstone to make space. Reserve fails closed the same way.
func TestBK4AggregateQuotaRefusesBeforeWrite(t *testing.T) {
	// One small record fits; the quota is tight enough that a second cannot.
	// 600 bytes accommodates one ~530-byte record but refuses a second.
	s, err := Open(t.TempDir(), WithClock(fixedClock), WithMaxStoreBytes(600))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	first := newRecord("11111111-1111-4111-8111-111111111701")
	if err := s.Create(first); err != nil {
		t.Fatalf("create first: %v", err)
	}

	// Reserve for a worst-case record must refuse now that the quota is full.
	if err := s.Reserve(protocol.MaxRecordBytes); err == nil {
		t.Fatalf("Reserve at quota: want storage_full, got nil")
	} else if code := protocol.RefusalCode(err); code != protocol.CodeStorageFull {
		t.Fatalf("Reserve at quota: want storage_full, got %q", code)
	}

	// A second record is refused with storage_full and does NOT land.
	second := newRecord("11111111-1111-4111-8111-111111111702")
	if err := s.Create(second); err == nil {
		t.Fatal("create second at quota: want storage_full, got nil")
	} else if code := protocol.RefusalCode(err); code != protocol.CodeStorageFull {
		t.Fatalf("create second at quota: want storage_full, got %q", code)
	}

	// The first record is intact: its dedup identity survives (no eviction).
	got, ok, err := s.Get(Key{first.CreatorHost, first.TargetID, first.RequestID})
	if err != nil || !ok {
		t.Fatalf("first record lost after quota refusal: ok=%v err=%v", ok, err)
	}
	if got.RequestID != first.RequestID {
		t.Fatalf("first record identity changed: %q", got.RequestID)
	}
	// The refused second record left no file.
	if _, ok, err := s.Get(Key{second.CreatorHost, second.TargetID, second.RequestID}); err != nil || ok {
		t.Fatalf("refused second record landed on disk: ok=%v err=%v", ok, err)
	}
}

// TestBK4QuotaDisabledByDefault pins that a store without WithMaxStoreBytes
// enforces no aggregate quota: records accumulate up to the per-record cap
// only. Production sets the quota via serve; tests that shrink it use
// WithMaxStoreBytes explicitly.
func TestBK4QuotaDisabledByDefault(t *testing.T) {
	s, err := Open(t.TempDir(), WithClock(fixedClock))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Reserve never refuses when the quota is disabled.
	if err := s.Reserve(protocol.MaxRecordBytes); err != nil {
		t.Fatalf("Reserve with quota disabled: want nil, got %v", err)
	}
	for i := 0; i < 5; i++ {
		rec := newRecord(fmtID(i))
		if err := s.Create(rec); err != nil {
			t.Fatalf("create %d with quota disabled: %v", i, err)
		}
	}
}

// fmtID builds a valid UUIDv4-ish id distinct per index.
func fmtID(i int) string {
	base := "11111111-1111-4111-8111-11111111170"
	if i < 10 {
		return base + string(rune('0'+i))
	}
	return base + "a"
}
