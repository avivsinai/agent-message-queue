package core_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// busyAttachment answers busy while busy is set, the way an adapter does
// for a turn it cannot start yet; nothingSent says it made no native call.
// onBusy, when set, runs before a busy answer, and onCancel before
// CancelExact records the intent the way the Codex adapter does.
type busyAttachment struct {
	*fake.Runtime
	busy        atomic.Bool
	nothingSent bool
	onBusy      func()
	onCancel    func()
}

func (b *busyAttachment) Submit(req core.BoundRequest) (core.Admission, error) {
	if b.busy.Load() {
		if b.onBusy != nil {
			b.onBusy()
		}
		return core.Admission{Code: protocol.CodeBusy, Message: "not ready yet", NothingSent: b.nothingSent}, nil
	}
	return b.Runtime.Submit(req)
}

func (b *busyAttachment) CancelExact(key requests.Key, epoch string) (core.CancelEvidence, error) {
	if b.onCancel != nil {
		b.onCancel()
		return core.CancelEvidence{Disposition: protocol.CancelRequested}, nil
	}
	return b.Runtime.CancelExact(key, epoch)
}

// Review finding (bead agent-message-queue-uue): adapter busy stored as
// terminal rejection. The record kept its dispatch and no tombstone, so the
// sender's identical retry got the stored rejection back with no code and
// recorded a prompt that never ran as dispatched. Each row does one thing to
// the busy tombstone before the adapter frees up, then sends the identical
// retry the sender drainer sends.
func TestAdapterBusyLeavesARetryableTombstone(t *testing.T) {
	local := core.Source{Host: "local"}
	for _, tc := range []struct {
		name    string
		between func(t *testing.T, ep *core.Endpoint, att *busyAttachment, id string, clk *time.Time)
		// want and wantCode are what the final identical retry reports and
		// stores; dispatches counts the native sends.
		want       protocol.State
		wantCode   protocol.Code
		dispatches int
	}{
		{name: "an identical retry runs it", want: protocol.StateRunning, dispatches: 1},
		{
			// Post-release review v0.113.2 finding R1: a retry while the
			// target was offline answered the tombstone with no code, which
			// the drainer and the CLI record as dispatched, and the prompt
			// never ran.
			name: "a retry while the target is offline stays retryable",
			between: func(t *testing.T, ep *core.Endpoint, att *busyAttachment, id string, _ *time.Time) {
				att.SetOffline(true)
				out, err := ep.Handle(submitCmd(id), local)
				if err != nil {
					t.Fatalf("retry while offline: %v", err)
				}
				if r := out.(protocol.Reply); r.Outcome.Code != protocol.CodeBusy || r.Snapshot.State != protocol.StateRejected {
					t.Fatalf("retry while offline = %s %q, want rejected busy so the sender retries", r.Snapshot.State, r.Outcome.Code)
				}
				att.SetOffline(false)
			},
			want: protocol.StateRunning, dispatches: 1,
		},
		{
			// Post-release review v0.113.2 finding R2: a cancel of the busy
			// tombstone answered noop_already_terminal and recorded nothing,
			// so the drainer's next retry ran the cancelled prompt.
			name: "a cancel makes it final",
			between: func(t *testing.T, ep *core.Endpoint, _ *busyAttachment, id string, _ *time.Time) {
				out, err := ep.Handle(cancelCmd(id), local)
				if err != nil {
					t.Fatalf("cancel: %v", err)
				}
				if r := out.(protocol.Reply); r.Outcome.Disposition != protocol.CancelConfirmed || r.Snapshot.State != protocol.StateCancelled {
					t.Fatalf("cancel of a busy tombstone = %s %q, want cancelled, confirmed", r.Snapshot.State, r.Outcome.Disposition)
				}
			},
			want: protocol.StateCancelled, dispatches: 0,
		},
		{
			// Post-release review v0.113.2 finding R2, code-running review
			// of 27011714: a cancel that saw the retry dispatching relocked
			// after the busy answer had stored a fresh tombstone, answered
			// noop_already_terminal, and the next retry ran the prompt.
			name: "a cancel that races a busy retry makes it final",
			between: func(t *testing.T, ep *core.Endpoint, att *busyAttachment, id string, _ *time.Time) {
				entered, release, retried := make(chan struct{}), make(chan struct{}), make(chan struct{})
				att.onBusy = func() { close(entered); <-release }
				att.onCancel = func() { close(release); <-retried }
				go func() {
					_, _ = ep.Handle(submitCmd(id), local)
					close(retried)
				}()
				<-entered
				out, err := ep.Handle(cancelCmd(id), local)
				if err != nil {
					t.Fatalf("cancel: %v", err)
				}
				if r := out.(protocol.Reply); r.Outcome.Disposition != protocol.CancelConfirmed || r.Snapshot.State != protocol.StateCancelled {
					t.Fatalf("cancel racing a busy retry = %s %q, want cancelled, confirmed", r.Snapshot.State, r.Outcome.Disposition)
				}
			},
			want: protocol.StateCancelled, dispatches: 0,
		},
		{
			// Post-release review v0.113.2 finding R4: a busy tombstone was
			// never compacted and kept its full input for good.
			name: "compaction after its window sheds the input",
			between: func(t *testing.T, ep *core.Endpoint, _ *busyAttachment, _ string, clk *time.Time) {
				*clk = clk.Add(time.Hour)
				if err := ep.Reconcile(); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
			},
			want: protocol.StateRejected, wantCode: protocol.CodeExpired, dispatches: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
			now := func() time.Time { return clk }
			store, err := requests.Open(t.TempDir(), requests.WithClock(now))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			att := &busyAttachment{Runtime: fake.New("fake", "e_1"), nothingSent: true}
			att.busy.Store(true)
			ep := core.New(core.Config{Store: store, Now: now, CompactHorizon: time.Minute})
			ep.Register(att)
			t.Cleanup(func() { _ = ep.Close() })
			id := "11111111-1111-4111-8111-11111111b5e1"

			out, err := ep.Handle(submitCmd(id), local)
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			if got := out.(protocol.Reply).Outcome.Code; got != protocol.CodeBusy {
				t.Fatalf("submit while the adapter is busy: outcome %q, want busy", got)
			}
			if tc.between != nil {
				tc.between(t, ep, att, id, &clk)
			}

			att.busy.Store(false)
			out, err = ep.Handle(submitCmd(id), local)
			if err != nil {
				t.Fatalf("identical resubmit: %v", err)
			}
			reply := out.(protocol.Reply)
			if reply.Outcome.Code != tc.wantCode || reply.Snapshot.State != tc.want {
				t.Fatalf("identical resubmit after busy = %s %q, want %s %q", reply.Snapshot.State, reply.Outcome.Code, tc.want, tc.wantCode)
			}
			if got := att.Snapshot().Dispatches; got != tc.dispatches {
				t.Fatalf("native dispatches = %d, want %d", got, tc.dispatches)
			}
			rec, ok, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
			if err != nil || !ok || rec.NativeDispatches != tc.dispatches || rec.State != tc.want {
				t.Fatalf("record = %+v, %v, %v; want %d dispatches, %s", rec, ok, err, tc.dispatches, tc.want)
			}
			if tc.wantCode == protocol.CodeExpired && rec.Input != nil {
				t.Fatalf("busy tombstone past its window kept its input %+v", rec.Input)
			}
		})
	}
}

// Review finding (bead agent-message-queue-uue): pi busy after publish. The
// pi extension answers busy after the request file exists, so a busy
// answer without NothingSent stays a terminal rejection: a resubmit returns
// it and never reaches the adapter again.
func TestAdapterBusyAfterANativeCallStaysRejected(t *testing.T) {
	store, now := openStore(t)
	att := &busyAttachment{Runtime: fake.New("fake", "e_1")}
	att.busy.Store(true)
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(att)
	t.Cleanup(func() { _ = ep.Close() })
	local := core.Source{Host: "local"}
	id := "11111111-1111-4111-8111-11111111b5e2"

	if _, err := ep.Handle(submitCmd(id), local); err != nil {
		t.Fatalf("submit: %v", err)
	}
	att.busy.Store(false)
	out, err := ep.Handle(submitCmd(id), local)
	if err != nil {
		t.Fatalf("identical resubmit: %v", err)
	}
	if reply := out.(protocol.Reply); reply.Snapshot.State != protocol.StateRejected || att.Snapshot().Dispatches != 0 {
		t.Fatalf("resubmit after a busy that followed a native call = %s, %d dispatches; want rejected, none", reply.Snapshot.State, att.Snapshot().Dispatches)
	}
	rec, ok, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil || !ok || rec.Tombstone || rec.NativeDispatches != 1 {
		t.Fatalf("record = %+v, %v, %v; want a terminal rejection with its dispatch", rec, ok, err)
	}
}
