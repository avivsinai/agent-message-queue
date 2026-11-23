package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// linkShare is a signed link source for one binding: the record it creates
// names the binding, so the run-start transition can read that binding's
// dialog directory.
func linkShare(binding string) core.Source {
	return core.Source{
		Host: "link-x", Origin: map[string]string{"carrier": core.CarrierLink, core.OriginSink: "link-x"},
		Shared: []string{"fake"}, NativeSession: "fake", Credential: "k1", Binding: binding,
	}
}

// R1: entering running takes the baseline of dialogs open in the binding's
// dialog directory, written once with the running transition — a later
// reconcile never rewrites it, an unlistable directory stores no list, and
// a record without a binding never gets one.
func TestRunStartStoresDialogsBeforeOnce(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	ep.SetConsent(func(string, string) bool { return true })
	calls := 0
	listing := []string{"d-1", "d-2"}
	ep.SetDialogsOpen(func(string) ([]string, error) {
		calls++
		return listing, nil
	})

	id := "11111111-1111-4111-8111-1111111111d1"
	if _, err := ep.Handle(submitCmd(id), linkShare("sess-1")); err != nil {
		t.Fatal(err)
	}
	key := requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(key)
	if err != nil || !ok {
		t.Fatalf("record after submit: ok=%v err=%v", ok, err)
	}
	if rec.Binding != "sess-1" {
		t.Fatalf("record binding = %q; want sess-1", rec.Binding)
	}
	if len(rec.DialogsBefore) != 2 || rec.DialogsBefore[0] != "d-1" {
		t.Fatalf("dialogs_before = %v; want the baseline at running", rec.DialogsBefore)
	}
	if calls != 1 {
		t.Fatalf("listing ran %d times; want once", calls)
	}

	// A later observation does not rewrite the baseline.
	listing = []string{"d-late"}
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ = store.Get(key); len(rec.DialogsBefore) != 2 || rec.DialogsBefore[0] != "d-1" {
		t.Fatalf("reconcile rewrote the baseline: %v", rec.DialogsBefore)
	}
	if calls != 1 {
		t.Fatalf("listing ran %d times after reconcile; want once", calls)
	}

	// An unlistable directory stores no list; the record publishes nothing.
	id2 := "11111111-1111-4111-8111-1111111111d2"
	ep2 := core.New(core.Config{Store: store, Now: now})
	ep2.Register(fake.New("fake", "e_1"))
	ep2.SetConsent(func(string, string) bool { return true })
	ep2.SetDialogsOpen(func(string) ([]string, error) { return nil, errUnlistable })
	if _, err := ep2.Handle(submitCmd(id2), linkShare("sess-2")); err != nil {
		t.Fatal(err)
	}
	if rec2, _, _ := store.Get(requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id2}); rec2.DialogsBefore != nil {
		t.Fatalf("unlistable directory stored %v; want no list", rec2.DialogsBefore)
	}

	// No callback, no list: startup reconciliation before the wiring keeps
	// the record publishing nothing.
	id3 := "11111111-1111-4111-8111-1111111111d3"
	if _, err := ep.Handle(submitCmd(id3), core.Source{Host: "local", NativeSession: "fake", Origin: map[string]string{"carrier": "buzz"}}); err != nil {
		t.Fatal(err)
	}
	if rec3, _, _ := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id3}); rec3.DialogsBefore != nil {
		t.Fatalf("a record without a binding stored %v", rec3.DialogsBefore)
	}
}

var errUnlistable = &unlistableError{}

type unlistableError struct{}

func (*unlistableError) Error() string { return "permission denied" }

// A missing dialog directory is the empty list: a real baseline that
// publishes nothing of the run, not the no-list refusal.
func TestRunStartStoresAnEmptyBaselineForAMissingDirectory(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	ep.SetConsent(func(string, string) bool { return true })
	ep.SetDialogsOpen(func(string) ([]string, error) { return []string{}, nil })
	id := "11111111-1111-4111-8111-1111111111d4"
	if _, err := ep.Handle(submitCmd(id), linkShare("sess-3")); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := store.Get(requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id})
	if err != nil || !ok {
		t.Fatalf("record after submit: ok=%v err=%v", ok, err)
	}
	if rec.DialogsBefore == nil || len(rec.DialogsBefore) != 0 {
		t.Fatalf("dialogs_before = %v; want an empty baseline", rec.DialogsBefore)
	}
	_ = time.Now
}

// A run that comes back from uncertain through recovery never manufactures a
// baseline: the capture belongs to the original live admission, and a run
// whose first listing failed publishes nothing even after it is found
// running again.
func TestRecoveryIntoRunningKeepsNoBaseline(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	ep.SetConsent(func(string, string) bool { return true })
	calls := 0
	ep.SetDialogsOpen(func(string) ([]string, error) {
		calls++
		if calls == 1 {
			return nil, errUnlistable
		}
		return []string{"d-1"}, nil // the callback works again by recovery time
	})
	id := "11111111-1111-4111-8111-1111111111d5"
	if _, err := ep.Handle(submitCmd(id), linkShare("sess-4")); err != nil {
		t.Fatal(err)
	}
	key := requests.Key{CreatorHost: "link-x", TargetID: "fake", RequestID: id}
	rec, _, err := store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != protocol.StateRunning || rec.DialogsBefore != nil {
		t.Fatalf("after submit: state %s dialogs %v", rec.State, rec.DialogsBefore)
	}
	// The run is lost: the record goes uncertain while the run is still
	// bound natively.
	rec.Revision, rec.State, rec.Code = rec.Revision+1, protocol.StateUncertain, protocol.CodeAttachmentLost
	if err := store.Update(rec); err != nil {
		t.Fatal(err)
	}
	// Recovery finds the run admitted and returns the record to running.
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
	rec, _, err = store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != protocol.StateRunning {
		t.Fatalf("recovery left the record at %s; want running", rec.State)
	}
	if calls != 1 || rec.DialogsBefore != nil {
		t.Fatalf("recovery manufactured a baseline (calls %d, dialogs %v); want the failed first listing kept", calls, rec.DialogsBefore)
	}
}
