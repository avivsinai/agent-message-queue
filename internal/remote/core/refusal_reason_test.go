package core_test

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestRefusalReasonRoundTripsToSnapshot reproduces agent-message-queue-611.24:
// admissionCause kept the refusal code and dropped the adapter message, so a
// live submit showed only "rejected code=unsupported".
func TestRefusalReasonRoundTripsToSnapshot(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	rt.FailNextAdmissionAfterReturn()
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	id := "11111111-1111-4111-8111-111111111124"
	rep, err := ep.Handle(submitCmd(id), core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	reply, ok := rep.(protocol.Reply)
	if !ok {
		t.Fatalf("reply type %T", rep)
	}
	const want = "native admission failed after helper returned"
	if reply.Snapshot.State != protocol.StateRejected || reply.Snapshot.Code != protocol.CodeNativeError || reply.Snapshot.Reason != want {
		t.Fatalf("snapshot = %s %s %q", reply.Snapshot.State, reply.Snapshot.Code, reply.Snapshot.Reason)
	}
	rec, ok, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil || !ok {
		t.Fatalf("store get ok=%v err=%v", ok, err)
	}
	if rec.Reason != want {
		t.Fatalf("record reason = %q", rec.Reason)
	}
}
