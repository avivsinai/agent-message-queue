package core_test

import (
	"sync/atomic"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// busyAttachment answers busy, sending nothing natively, while busy is set,
// the way an adapter does for a turn it cannot start yet.
type busyAttachment struct {
	*fake.Runtime
	busy atomic.Bool
}

func (b *busyAttachment) Submit(req core.BoundRequest) (core.Admission, error) {
	if b.busy.Load() {
		return core.Admission{Code: protocol.CodeBusy, Message: "not ready yet"}, nil
	}
	return b.Runtime.Submit(req)
}

// Review finding (bead agent-message-queue-uue): adapter busy stored as
// terminal rejection. The record kept its dispatch and no tombstone, so the
// sender's identical retry got the stored rejection back with no code and
// recorded a prompt that never ran as dispatched.
func TestAdapterBusyLeavesARetryableTombstone(t *testing.T) {
	store, now := openStore(t)
	att := &busyAttachment{Runtime: fake.New("fake", "e_1")}
	att.busy.Store(true)
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(att)
	t.Cleanup(func() { _ = ep.Close() })
	local := core.Source{Host: "local"}
	id := "11111111-1111-4111-8111-11111111b5e1"

	out, err := ep.Handle(submitCmd(id), local)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := out.(protocol.Reply).Outcome.Code; got != protocol.CodeBusy {
		t.Fatalf("submit while the adapter is busy: outcome %q, want busy", got)
	}

	att.busy.Store(false)
	out, err = ep.Handle(submitCmd(id), local)
	if err != nil {
		t.Fatalf("identical resubmit: %v", err)
	}
	reply := out.(protocol.Reply)
	if reply.Outcome.Code != "" || reply.Snapshot.State != protocol.StateRunning {
		t.Fatalf("identical resubmit after busy = %s %q, want running", reply.Snapshot.State, reply.Outcome.Code)
	}
	if got := att.Snapshot().Dispatches; got != 1 {
		t.Fatalf("native dispatches = %d, want 1", got)
	}
	rec, ok, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil || !ok || rec.NativeDispatches != 1 || rec.State != protocol.StateRunning {
		t.Fatalf("record = %+v, %v, %v; want one dispatch, running", rec, ok, err)
	}
}
