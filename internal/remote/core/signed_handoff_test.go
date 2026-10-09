package core_test

import (
	"sync"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// movingNative is a fake whose native session can change while a request
// waits, as a harness does on /new or a reload.
type movingNative struct {
	*fake.Runtime
	mu sync.Mutex
	id string
}

func (m *movingNative) NativeSessionID() string { m.mu.Lock(); defer m.mu.Unlock(); return m.id }
func (m *movingNative) set(id string)           { m.mu.Lock(); m.id = id; m.mu.Unlock() }

// A signed link request that waited for an offline target is checked again
// right before the handoff: the native session changed meanwhile, so it ends
// session_changed and the harness never receives it. A request signed for
// the session that is attached runs.
func TestSignedRequestStopsWhenTheSessionChangedBeforeHandoff(t *testing.T) {
	rt := &movingNative{Runtime: fake.New("fake", "e_1"), id: "native-1"}
	ep, store, clk := newLinkEndpoint(t, rt)
	src := linkSource(hostA, "fake")
	src.NativeSession, src.Credential = "native-1", "cred-1"

	rt.SetOffline(true)
	waiting := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111d1")
	if out, err := ep.Handle(waiting, src); err != nil || out.(protocol.Reply).Snapshot.State != protocol.StateReceived {
		t.Fatalf("offline submit = %+v, %v; want received", out, err)
	}
	rt.set("native-2")
	rt.SetOffline(false)
	if err := ep.Tick(); err != nil {
		t.Fatal(err)
	}
	rec, _, err := store.Get(requests.Key{CreatorHost: hostA, TargetID: "fake", RequestID: waiting.RequestID})
	if err != nil || rec.State != protocol.StateRejected || rec.Code != protocol.CodeSessionChanged {
		t.Fatalf("deferred request %+v, %v; want rejected session_changed", rec, err)
	}
	if n := rt.Snapshot().Dispatches; n != 0 {
		t.Fatalf("the harness received %d submits, want 0", n)
	}

	src.NativeSession = "native-2"
	out, err := ep.Handle(linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111d2"), src)
	if err != nil || out.(protocol.Reply).Snapshot.State != protocol.StateRunning || rt.Snapshot().Dispatches != 1 {
		t.Fatalf("submit signed for the attached session = %+v, %v; want running", out, err)
	}
}

// The same signed submit sent twice (a resend after a lost answer) runs once:
// the second reply carries the first's record, with no second dispatch.
func TestSignedSubmitReplayRunsOnce(t *testing.T) {
	rt := &movingNative{Runtime: fake.New("fake", "e_1"), id: "native-1"}
	ep, _, clk := newLinkEndpoint(t, rt)
	src := linkSource(hostA, "fake")
	src.NativeSession, src.Credential = "native-1", "cred-1"
	cmd := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111e1")
	first, err := ep.Handle(cmd, src)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ep.Handle(cmd, src)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := first.(protocol.Reply).Snapshot, again.(protocol.Reply).Snapshot; a.RequestRef != b.RequestRef || rt.Snapshot().Dispatches != 1 {
		t.Fatalf("replay = %+v after %+v, %d dispatches; want the same record and 1 dispatch", b, a, rt.Snapshot().Dispatches)
	}
}

// A consent key removed while the request waited for an offline target stops
// it at the handoff: consent_invalid, and the harness never receives it.
func TestRemovedKeyStopsAWaitingSignedRequest(t *testing.T) {
	rt := &movingNative{Runtime: fake.New("fake", "e_1"), id: "native-1"}
	ep, store, clk := newLinkEndpoint(t, rt)
	live := true
	ep.SetConsent(func(string, string) bool { return live })
	src := linkSource(hostA, "fake")
	src.NativeSession, src.Credential = "native-1", "cred-1"
	rt.SetOffline(true)
	cmd := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111e2")
	if _, err := ep.Handle(cmd, src); err != nil {
		t.Fatal(err)
	}
	live = false
	rt.SetOffline(false)
	if err := ep.Tick(); err != nil {
		t.Fatal(err)
	}
	rec, _, err := store.Get(requests.Key{CreatorHost: hostA, TargetID: "fake", RequestID: cmd.RequestID})
	if err != nil || rec.Code != protocol.CodeConsentInvalid || rt.Snapshot().Dispatches != 0 {
		t.Fatalf("record %+v, %v, %d dispatches; want consent_invalid and 0", rec, err, rt.Snapshot().Dispatches)
	}
}
