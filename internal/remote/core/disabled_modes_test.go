package core_test

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestDisabledQueueAndSteerModesAreRefused pins the D1 gate (bead 611.22.10 /
// 611.22.23): busy=queue and deliver=steer are disabled in v1 until their
// ownership+evidence models exist. A submit carrying either is refused
// unsupported (exit 6 via ExitForCode) BEFORE any durable write — no record
// is created — and the identical submit with reject/turn succeeds.
func TestDisabledQueueAndSteerModesAreRefused(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	for _, tc := range []struct {
		name  string
		busy  protocol.Busy
		deliv protocol.Deliver
	}{
		{"busy-queue", protocol.BusyQueue, protocol.DeliverTurn},
		{"deliver-steer", protocol.BusyReject, protocol.DeliverSteer},
	} {
		id := "11111111-1111-4111-8111-1111111111f1"
		cmd := submitCmd(id)
		cmd.Input.Busy = tc.busy
		cmd.Input.Deliver = tc.deliv
		_, err := ep.Handle(cmd, core.Source{Host: "local"})
		refusal := protocol.RefusalCode(err)
		if refusal != protocol.CodeUnsupported {
			t.Fatalf("%s: err = %v, want unsupported refusal", tc.name, err)
		}
		if code := protocol.ExitForCode(refusal); code != 6 {
			t.Fatalf("%s: exit code = %d, want 6 (action required)", tc.name, code)
		}
		if _, ok, _ := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}); ok {
			t.Fatalf("%s: refused submit created a durable record", tc.name)
		}
	}

	// The plain reject/turn submit still works.
	id := "11111111-1111-4111-8111-1111111111f1"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("reject/turn submit: %v", err)
	}
	rec, ok, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil || !ok {
		t.Fatalf("reject/turn record missing: ok=%v err=%v", ok, err)
	}
	if rec.State != protocol.StateDispatching && rec.State != protocol.StateRunning {
		t.Fatalf("reject/turn submit did not dispatch: state=%s", rec.State)
	}
}
