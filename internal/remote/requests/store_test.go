package requests

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	if err := s.Reserve(Key{"host", "target", "reserved"}, protocol.MaxRecordBytes); err == nil {
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
	if err := s.Reserve(Key{"host", "target", "reserved"}, protocol.MaxRecordBytes); err != nil {
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

// TestBK4B1RaceCompactVsAck exercises the race the round-1 review confirmed
// (611.22.19 BK4 round-2 B1): CompactOne and MarkAcknowledged mutate s.used
// concurrently. Before the fix, CompactOne ran with no store lock while
// MarkAcknowledged held it; a lost += drifted the quota counter. This test
// is semantic (NEW 2, round-3): it runs N concurrent CompactOne vs
// MarkAcknowledged rounds and asserts used equals a fresh recomputation of
// the on-disk sum. It catches the lost update WITHOUT -race and stays under
// 5s (modeled on requests/lost_update_regression_test.go).
func TestBK4B1RaceCompactVsAck(t *testing.T) {
	s, err := Open(t.TempDir(), WithClock(fixedClock), WithMaxStoreBytes(64*1024*1024))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	const rounds = 20
	for r := 0; r < rounds; r++ {
		compactKey := Key{"hostA", "t_fake1", fmt.Sprintf("11111111-1111-4111-8111-11111111%04d", r*2+10)}
		ackKey := Key{"hostA", "t_fake1", fmt.Sprintf("11111111-1111-4111-8111-11111111%04d", r*2+11)}
		for _, k := range []Key{compactKey, ackKey} {
			rec := newRecord(k.RequestID)
			if err := s.Create(rec); err != nil {
				t.Fatalf("create %s: %v", k.RequestID, err)
			}
			rec.Revision, rec.State = 2, protocol.StateDispatching
			if err := s.Update(rec); err != nil {
				t.Fatalf("dispatching %s: %v", k.RequestID, err)
			}
			rec.Revision, rec.State = 3, protocol.StateCompleted
			rec.Result = &protocol.Result{Text: "done"}
			rec.ObservedAt = "2026-09-01T00:00:00Z"
			rec.AckDigest = protocol.EvidenceDigest(rec.Result)
			if err := s.Update(rec); err != nil {
				t.Fatalf("completed %s: %v", k.RequestID, err)
			}
			if err := s.MarkPublished(k, 3); err != nil {
				t.Fatalf("mark published %s: %v", k.RequestID, err)
			}
		}

		cutoff := fixedClock()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.CompactOne(compactKey, cutoff); err != nil {
				t.Errorf("CompactOne: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := s.MarkAcknowledged(ackKey); err != nil {
				t.Errorf("MarkAcknowledged: %v", err)
			}
		}()
		wg.Wait()
	}

	// used must be stable and correct: after N concurrent rounds, the in-memory
	// counter must equal a fresh recomputation of the on-disk sum. A lost
	// update (CompactOne without s.mu) drifts this counter. This assertion is
	// semantic — it catches the bug WITHOUT -race.
	want, _ := s.sumUsed()
	if got := s.used; got != want {
		t.Fatalf("used drifted after %d rounds: got %d want %d (race lost an update)", rounds, got, want)
	}
}

// TestBK4B2TombstoneAtQuotaDoesNotBreakSweep reproduces the round-1 review
// blocker B2 (611.22.19 BK4 round-2): a terminal record with no result GROWS
// on compaction (tombstone flag + revision bump). Before the fix, CompactOne
// at quota returned storage_full and Reconcile broke the whole sweep. Now
// settlement writes are quota-exempt, so CompactOne succeeds at quota and the
// sweep continues.
func TestBK4B2TombstoneAtQuotaDoesNotBreakSweep(t *testing.T) {
	// Tight quota: a minimal cancelled record (no Input, no Result) is ~505
	// bytes on disk, and compaction GROWS it to ~522 (tombstone flag +
	// revision bump + code). Quota 510 lets the record fit but would refuse
	// the tombstone write — before the fix, CompactOne returned storage_full
	// and Reconcile broke the whole sweep (key-ordered List stops every later
	// compaction too). Now settlement writes are quota-exempt.
	s, err := Open(t.TempDir(), WithClock(fixedClock), WithMaxStoreBytes(510))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	// A terminal record with NO result and NO input (cancelled before
	// dispatch) — compaction adds the tombstone flag and bumps the revision,
	// so the record GROWS.
	rec := &Record{
		Snapshot: protocol.Snapshot{
			Schema:      protocol.SchemaRequest,
			RequestID:   "11111111-1111-4111-8111-111111111720",
			CreatorHost: "hostA",
			TargetID:    "t_fake1",
			Epoch:       "e_1",
			Revision:    1,
			State:       protocol.StateReceived,
			InputDigest: Digest([]byte("x")),
		},
	}
	if err := s.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec.Revision = 2
	rec.State = protocol.StateCancelled
	rec.ObservedAt = "2026-09-01T00:00:00Z"
	if err := s.Update(rec); err != nil {
		t.Fatalf("update to cancelled: %v", err)
	}
	if err := s.MarkPublished(Key{rec.CreatorHost, rec.TargetID, rec.RequestID}, 2); err != nil {
		t.Fatalf("mark published: %v", err)
	}

	cutoff := fixedClock()
	ok, err := s.CompactOne(Key{rec.CreatorHost, rec.TargetID, rec.RequestID}, cutoff)
	if err != nil {
		t.Fatalf("CompactOne at quota: settlement write must be quota-exempt, got %v", err)
	}
	if !ok {
		t.Fatal("CompactOne at quota: want compacted=true, got false")
	}
}

// TestBK4B3ReservationIsReal reproduces the round-1 review blocker B3
// (611.22.19 BK4 round-2): Reserve was a point-in-time check, not a
// reservation. quota = 3*MaxRecordBytes admitted six submits, then refused
// three results — records stuck in running forever. Now Reserve increments a
// reserved total; used+reserved <= quota is the invariant; a result write for
// an admitted record is never quota-refused.
func TestBK4B3ReservationIsReal(t *testing.T) {
	// quota = 4 * MaxRecordBytes: at most 3 worst-case records can be
	// admitted simultaneously (3 reservations + their Create overhead fit,
	// the 4th reservation does not). Before the fix, Reserve was a
	// point-in-time check and all 6 were admitted.
	quota := int64(4 * protocol.MaxRecordBytes)
	s, err := Open(t.TempDir(), WithClock(fixedClock), WithMaxStoreBytes(quota))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	admitted := 0
	refused := 0
	var admittedKeys []Key
	for i := 0; i < 6; i++ {
		k := Key{"hostA", "t_fake1", fmt.Sprintf("11111111-1111-4111-8111-11111111%04d", i+20)}
		// Reserve before Create (as the endpoint does before dispatch). All six
		// reservations are held simultaneously — the invariant is
		// used+reserved <= quota, so only 3 worst-case records fit.
		if err := s.Reserve(k, protocol.MaxRecordBytes); err != nil {
			refused++
			continue
		}
		rec := newRecord(k.RequestID)
		if err := s.Create(rec); err != nil {
			// Create should never refuse for a reserved key (reservation paid).
			t.Fatalf("Create for reserved key %s: %v", k.RequestID, err)
		}
		admitted++
		admittedKeys = append(admittedKeys, k)
	}
	if admitted != 3 {
		t.Fatalf("admitted: want 3 (quota = 3*MaxRecordBytes), got %d (refused %d)", admitted, refused)
	}
	if refused != 3 {
		t.Fatalf("refused: want 3, got %d", refused)
	}

	// Now land results for the admitted records: a result write for an
	// admitted record must NEVER be quota-refused (the reservation paid).
	for _, k := range admittedKeys {
		rec, _, err := s.Get(k)
		if err != nil {
			t.Fatalf("get admitted key %s: %v", k.RequestID, err)
		}
		rec.Revision = 2
		rec.State = protocol.StateDispatching
		if err := s.Update(rec); err != nil {
			t.Fatalf("dispatching Update for admitted key %s: %v", k.RequestID, err)
		}
		rec.Revision = 3
		rec.State = protocol.StateCompleted
		rec.Result = &protocol.Result{Text: string(make([]byte, 100*1024))}
		rec.ObservedAt = "2026-09-01T00:00:00Z"
		rec.AckDigest = protocol.EvidenceDigest(rec.Result)
		if err := s.Update(rec); err != nil {
			t.Fatalf("result Update for admitted key %s refused: %v (reservation must cover it)", k.RequestID, err)
		}
	}
}

// TestBK4B3RestartReseedReservation (round-2 B3 restart variant) proves Open
// reseeds reserved for non-terminal records with no result. After a restart,
// running records hold nothing in memory; without reseeding, fresh submits
// are admitted into their room and their results are refused after the work
// ran. The probe: create a running record, reopen the store at a quota just
// above used, write its result — must succeed.
func TestBK4B3RestartReseedReservation(t *testing.T) {
	dir := t.TempDir()
	// First open: create a running record with no result.
	quota := int64(2 * protocol.MaxRecordBytes)
	s1, err := Open(dir, WithClock(fixedClock), WithMaxStoreBytes(quota))
	if err != nil {
		t.Fatalf("open s1: %v", err)
	}
	k := Key{"hostA", "t_fake1", "11111111-1111-4111-8111-11111111b301"}
	rec := newRecord(k.RequestID)
	if err := s1.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec.Revision, rec.State = 2, protocol.StateDispatching
	if err := s1.Update(rec); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
	rec.Revision, rec.State = 3, protocol.StateRunning
	if err := s1.Update(rec); err != nil {
		t.Fatalf("running: %v", err)
	}
	usedAfterCreate := s1.used
	if err := s1.Close(); err != nil {
		t.Fatalf("close s1: %v", err)
	}

	// Reopen at a quota just above used: without reseeding, the running
	// record's result write would be refused (used + result > quota). With
	// reseeding, the reservation is reseeded and the result write succeeds
	// (the reservation pays for it).
	s2, err := Open(dir, WithClock(fixedClock), WithMaxStoreBytes(usedAfterCreate+int64(MaxRecordBytes)))
	if err != nil {
		t.Fatalf("open s2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	// The running record must have been reseeded.
	if s2.reserved <= 0 {
		t.Fatalf("B3 restart: reserved=%d after reopen, want >0 (reseeding failed)", s2.reserved)
	}

	// Write the result: must succeed (the reservation covers it).
	got, _, err := s2.Get(k)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	got.Revision = 4
	got.State = protocol.StateCompleted
	got.Result = &protocol.Result{Text: string(make([]byte, 100*1024))}
	got.ObservedAt = "2026-09-01T00:00:00Z"
	got.AckDigest = protocol.EvidenceDigest(got.Result)
	if err := s2.Update(got); err != nil {
		t.Fatalf("B3 restart: result write for running record refused after reopen: %v (reservation must cover it)", err)
	}
}
