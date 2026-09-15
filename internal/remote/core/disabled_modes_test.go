package core_test

import (
	"strings"
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
		want  string
	}{
		{"busy-queue", protocol.BusyQueue, protocol.DeliverTurn, "busy=queue"},
		{"deliver-steer", protocol.BusyReject, protocol.DeliverSteer, "deliver=steer"},
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
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: refusal %q does not name the disabled mode %q", tc.name, err.Error(), tc.want)
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

// TestSteerCapabilityMaskedInSessionProjection verifies Pro r2 #22
// (agent-message-queue-611.22.36): the D1 gate refuses every deliver=steer
// submit, but the attachment advertises Steer:true. session.inspect and
// session.list must mask Steer out of the advertised capabilities so a client
// that picks operations from the advertised capabilities is not given a false
// signal.
func TestSteerCapabilityMaskedInSessionProjection(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	// session.inspect
	inspectCmd := &protocol.Command{
		Schema:   protocol.SchemaCommand,
		Op:       protocol.OpSessionInspect,
		TargetID: "fake",
	}
	rep, err := ep.Handle(inspectCmd, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("session.inspect: %v", err)
	}
	insp, ok := rep.(protocol.Session)
	if !ok {
		t.Fatalf("session.inspect returned %T, want protocol.Session", rep)
	}
	if insp.Capabilities.Steer {
		t.Fatal("session.inspect advertises Steer:true (D1 gate refuses it — must be masked)")
	}

	// session.list
	listCmd := &protocol.Command{
		Schema: protocol.SchemaCommand,
		Op:     protocol.OpSessionList,
	}
	rep2, err := ep.Handle(listCmd, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("session.list: %v", err)
	}
	sessions, ok := rep2.([]protocol.Session)
	if !ok {
		t.Fatalf("session.list returned %T, want []protocol.Session", rep2)
	}
	if len(sessions) != 1 {
		t.Fatalf("session.list returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].Capabilities.Steer {
		t.Fatal("session.list advertises Steer:true (D1 gate refuses it — must be masked)")
	}
}
