package requests

import (
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
