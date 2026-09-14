package core_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression tests for Pro round-2 findings on the endpoint
// (agent-message-queue-611.22.36 packets 4a, 4b, 4c). Each fails against the
// endpoint as it was before the fix.

// TestPublishedRevisionMarkerFailureRetriesMarkerNotDelivery reproduces
// packet 4a: a publish that reached the caller but whose published_revision
// marker did not commit was published AGAIN by the next Reconcile. The
// message was already in the caller's mailbox; if the caller had drained it,
// the second copy was a genuine duplicate. Reconcile now retries only the
// marker.
func TestPublishedRevisionMarkerFailureRetriesMarkerNotDelivery(t *testing.T) {
	store, now := openStore(t)
	var mu sync.Mutex
	var published []protocol.Snapshot
	var crashOnce atomic.Int32
	ep := core.New(core.Config{
		Store: store, Now: now,
		Publish: func(s protocol.Snapshot, _ map[string]string) error {
			mu.Lock()
			published = append(published, s)
			mu.Unlock()
			return nil
		},
		// The marker write "crashes" once, right after a successful publish.
		Crash: func(point string) error {
			if point == core.PointBeforePublished && crashOnce.CompareAndSwap(1, 0) {
				return errors.New("marker write lost")
			}
			return nil
		},
	})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	id := "11111111-1111-4111-8111-11111111114a"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !b14cWait(func() bool { return rt.HasRun(id) }) {
		t.Fatal("never admitted")
	}
	crashOnce.Store(1)
	if !rt.Complete(id, "done") {
		t.Fatal("complete")
	}
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, _, _ := store.Get(k)
	if rec.State != protocol.StateCompleted {
		t.Fatalf("precondition: state=%s", rec.State)
	}
	if rec.PublishedRevision >= rec.Revision {
		t.Fatal("precondition: the marker committed although its write was lost")
	}
	countTerminal := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, s := range published {
			if s.State == protocol.StateCompleted {
				n++
			}
		}
		return n
	}
	if countTerminal() != 1 {
		t.Fatalf("precondition: terminal revision published %d times", countTerminal())
	}

	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rec, _, _ = store.Get(k)
	if rec.PublishedRevision != rec.Revision {
		t.Fatalf("marker not repaired: published=%d revision=%d", rec.PublishedRevision, rec.Revision)
	}
	if n := countTerminal(); n != 1 {
		t.Fatalf("terminal revision delivered %d times (4a — reconcile re-delivered instead of re-marking)", n)
	}
}

// TestCompactionKeepsResultTheCallerHasNotReceived reproduces packet 4b: the
// compaction gate checked only the runtime obligation (OwesAck). A result
// whose AMQ publication never succeeded but whose native acknowledgement was
// memoed was compacted away — the only retained copy of an answer the caller
// had not received.
func TestCompactionKeepsResultTheCallerHasNotReceived(t *testing.T) {
	store, now := openStore(t)
	id := "11111111-1111-4111-8111-11111111114b"
	rec := &requests.Record{
		Snapshot: protocol.Snapshot{
			Schema: protocol.SchemaRequest, RequestID: id, CreatorHost: "amq:codex", TargetID: "fake",
			Epoch: "e_1", Revision: 1, State: protocol.StateReceived, InputDigest: requests.Digest([]byte("hi")),
			ObservedAt: "2026-09-01T00:00:00Z",
		},
		Input:  &protocol.SubmitInput{Text: "hi"},
		Origin: map[string]string{"carrier": "amq", "from": "codex"},
	}
	if err := store.Create(rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	run := "run_1"
	rec.Revision, rec.State = 2, protocol.StateDispatching
	if err := store.Update(rec); err != nil {
		t.Fatalf("dispatching: %v", err)
	}
	rec.Revision, rec.State, rec.NativeRun, rec.NativeDispatches = 3, protocol.StateRunning, &run, 1
	if err := store.Update(rec); err != nil {
		t.Fatalf("running: %v", err)
	}
	rec.Revision, rec.State = 4, protocol.StateCompleted
	rec.Result = &protocol.Result{Text: "the answer"}
	rec.AckDigest = protocol.EvidenceDigest(rec.Result) // runtime released; caller never got it
	rec.ObservedAt = "2026-09-01T00:00:00Z"
	if err := store.Update(rec); err != nil {
		t.Fatalf("completed: %v", err)
	}
	k := requests.Key{CreatorHost: "amq:codex", TargetID: "fake", RequestID: id}

	n, err := store.Compact(now().Add(time.Second), 1000)
	if err != nil || n != 0 {
		t.Fatalf("compact: n=%d err=%v, want 0 (4b — the caller has not received this result)", n, err)
	}
	got, _, _ := store.Get(k)
	if got.Result == nil || got.Result.Text != "the answer" {
		t.Fatalf("result erased before publication: %+v", got.Result)
	}

	// Once published, the same record is compactable.
	if err := store.MarkPublished(k, got.Revision); err != nil {
		t.Fatalf("mark published: %v", err)
	}
	n, err = store.Compact(now().Add(time.Second), 1000)
	if err != nil || n != 1 {
		t.Fatalf("compact after publication: n=%d err=%v, want 1", n, err)
	}
}

// TestLateResultWhoseWriteFailedIsRecovered reproduces packet 4c: a result
// arriving on an already-cancelled record whose write failed was discarded,
// and replayTerminalAck returned before asking the attachment because the
// record had neither a result nor an ack digest. The retained result was
// never fetched or acknowledged. Reconcile now asks the attachment for a
// closed record with a bound run and no result and applies what it holds.
func TestLateResultWhoseWriteFailedIsRecovered(t *testing.T) {
	storeDir := t.TempDir()
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	store, err := requests.Open(storeDir, requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ep := core.New(core.Config{Store: store, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })
	id := "11111111-1111-4111-8111-11111111114c"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !b14cWait(func() bool { return rt.HasRun(id) }) {
		t.Fatal("never admitted")
	}
	if _, err := ep.Handle(cancelCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	k := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, _ := store.Get(k)
	if !ok || rec.State != protocol.StateCancelled || rec.NativeRun == nil || rec.Result != nil {
		t.Fatalf("precondition: state=%s run=%v result=%v", recState(rec, ok), rec.NativeRun != nil, rec.Result)
	}

	// The store is unwritable when the late result arrives: the write fails
	// and onNative discards the update.
	setStoreWritable(t, storeDir, false)
	t.Cleanup(func() { setStoreWritable(t, storeDir, true) })
	if !rt.CompleteCancelled(id, "partial output") {
		t.Fatal("late result not emitted")
	}
	rec, _, _ = store.Get(k)
	if rec.Result != nil {
		t.Fatal("precondition: the late result was persisted although the store was unwritable")
	}
	setStoreWritable(t, storeDir, true)

	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rec, _, _ = store.Get(k)
	if rec.Result == nil || rec.Result.Text != "partial output" {
		t.Fatalf("late result never recovered after storage came back (4c): result=%+v", rec.Result)
	}
	if rec.AckDigest == "" {
		t.Fatal("recovered result was not acknowledged to the runtime")
	}
	if rt.UnacknowledgedResults() != 0 {
		t.Fatalf("runtime still retains %d unacknowledged result(s)", rt.UnacknowledgedResults())
	}
}

// setStoreWritable flips write permission on every directory of a store so a
// record write fails (or succeeds again) wherever the store keeps it.
func setStoreWritable(t *testing.T, dir string, writable bool) {
	t.Helper()
	mode := os.FileMode(0o500)
	if writable {
		mode = 0o700
	}
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if !writable {
		// Deepest first so the walk itself is not blocked next time.
		for i := len(dirs) - 1; i >= 0; i-- {
			if err := os.Chmod(dirs[i], mode); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for _, d := range dirs {
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
	}
}
