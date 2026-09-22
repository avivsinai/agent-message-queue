package core_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestRefusalReasonRoundTripsToSnapshot reproduces agent-message-queue-611.24:
// admissionCause kept the refusal code and dropped the adapter message, so a
// live submit showed only "rejected code=unsupported". The message stays off
// the snapshot (amq.remote.request/1) and comes back on Outcome.Message.
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
	if reply.Snapshot.State != protocol.StateRejected || reply.Snapshot.Code != protocol.CodeNativeError || reply.Outcome.Message != want {
		t.Fatalf("snapshot/outcome = %s %s %q", reply.Snapshot.State, reply.Snapshot.Code, reply.Outcome.Message)
	}
	raw, err := json.Marshal(reply.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"reason"`) || strings.Contains(string(raw), want) {
		t.Fatalf("snapshot carries the refusal text: %s", raw)
	}
	rec, ok, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil || !ok {
		t.Fatalf("store get ok=%v err=%v", ok, err)
	}
	if rec.RefusalReason != want {
		t.Fatalf("record reason = %q", rec.RefusalReason)
	}
	got, err := ep.Handle(&protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: reply.Snapshot.RequestRef,
	}, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	status, ok := got.(protocol.Reply)
	if !ok || status.Outcome.Message != want {
		t.Fatalf("status outcome = %#v", got)
	}
}
