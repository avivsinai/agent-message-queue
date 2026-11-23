package core_test

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// grantedBusyOnce refuses its first Submit as busy with nothing sent, as the
// Claude adapter does for a session in a turn, and admits the next.
type grantedBusyOnce struct {
	*promptRuntime
	mu   sync.Mutex
	done bool
}

func (b *grantedBusyOnce) Submit(req core.BoundRequest) (core.Admission, error) {
	b.mu.Lock()
	first := !b.done
	b.done = true
	b.mu.Unlock()
	if first {
		return core.Admission{Code: protocol.CodeBusy, NothingSent: true}, nil
	}
	return b.promptRuntime.Submit(req)
}

// Merge check of the session grant for Claude Code: a granted request that
// answered busy and was sent again, as the contract tells the server to do,
// got no text. A link's busy tombstone keeps no input, the stub went out,
// the hand-over marked the text given and returned nothing, and the request
// ended failed with "the task may have run".
func TestGrantedBusyResubmitRestoresText(t *testing.T) {
	rt := &grantedBusyOnce{promptRuntime: newPromptRuntime()}
	f := newGrantFixture(t, rt)
	const id = "11111111-1111-4111-8111-111111111a01"
	if rep := f.submit(t, id, true); rep.Outcome.Code != protocol.CodeBusy {
		t.Fatalf("busy submit = %q", rep.Outcome.Code)
	}
	if rec := f.record(t, id); rec.Input != nil {
		t.Fatalf("busy tombstone keeps input %+v, want none", rec.Input)
	}
	f.submit(t, id, true)
	if rec := f.record(t, id); rec.State != protocol.StateRunning || !rec.GrantAtPrompt || rec.Input == nil || rec.Input.Text != grantText {
		t.Fatalf("resubmit = %s mark %v input %+v; want running, dispatched as a stub, with its text", rec.State, rec.GrantAtPrompt, rec.Input)
	}
	if got := f.ep.GrantedPrompt(report(id, "default")); got != grantText {
		t.Fatalf("prompt = %q, want the text", got)
	}
	rt.set(func(r *promptRuntime) { r.digest = sha(grantText) })
	rt.Complete(id, "the answer")
	if rec := f.record(t, id); rec.State != protocol.StateCompleted || rec.Result == nil || rec.Result.Text != "the answer" {
		t.Fatalf("settled = %s result %+v; want completed with the answer", rec.State, rec.Result)
	}
}

// The mark never stands for a text that was not given: a record with no
// stored input gets no mark and no transition.
func TestGrantedWithoutStoredInputIsNotMarkedGiven(t *testing.T) {
	f := newGrantFixture(t, newPromptRuntime())
	const id = "11111111-1111-4111-8111-111111111a02"
	f.submit(t, id, true)
	rec := f.record(t, id)
	rec.Input = nil
	rec.Revision++
	if err := f.store.Update(rec); err != nil {
		t.Fatal(err)
	}
	revision := rec.Revision
	if got := f.ep.GrantedPrompt(report(id, "default")); got != "" {
		t.Fatalf("text without stored input = %q", got)
	}
	if got := f.record(t, id); got.GrantTextGiven || got.Revision != revision || got.State != protocol.StateRunning {
		t.Fatalf("record = mark %v revision %d state %s; want no mark, the same revision, running", got.GrantTextGiven, got.Revision, got.State)
	}
}

// Merge check: a cancel that the adapter did not confirm left the stub's
// record running, and the prompt then gave the text. A stub is the one case
// where the endpoint can still stop the task.
func TestGrantedTaskCancelledBeforeItsTextIsGivenDoesNotStart(t *testing.T) {
	rt := newPromptRuntime()
	f := newGrantFixture(t, rt)
	const id = "11111111-1111-4111-8111-111111111a03"
	f.submit(t, id, true)
	rt.FailNextCancelExact(errors.New("transport unavailable"))
	src := linkSource(hostA, "fake")
	src.NativeSession, src.Credential = "fake", "cred-1"
	_, _ = f.ep.Handle(linkCancel(f.clk, protocol.EncodeRef(hostA, "fake", id), "fake"), src)
	if rec := f.record(t, id); rec.State != protocol.StateRunning || rec.Cancel == nil {
		t.Fatalf("after the cancel = %s cancel %+v; want running with the cancel asked", rec.State, rec.Cancel)
	}
	if got := f.ep.GrantedPrompt(report(id, "default")); got != "" {
		t.Fatalf("prompt after a cancel = %q, want no text", got)
	}
	rec := f.record(t, id)
	if rec.State != protocol.StateCancelled || rec.GrantTextGiven || rec.Cancel.Disposition != protocol.CancelConfirmed {
		t.Fatalf("record = %s mark %v cancel %+v; want cancelled, no mark, confirmed", rec.State, rec.GrantTextGiven, rec.Cancel)
	}
	rt.Complete(id, "an answer to the bare stub")
	if rec := f.record(t, id); rec.State != protocol.StateCancelled || (rec.Result != nil && rec.Result.Text != "") {
		t.Fatalf("after the stub's turn = %s result %+v; want cancelled with no answer text", rec.State, rec.Result)
	}
}

// Review of the hand-over deadline: the last check came before the
// hand-over's reads, so a text was marked given, or a rejection committed,
// after the hook's wait had ended.
func TestGrantedHandoverPastItsDeadlineWritesNothing(t *testing.T) {
	for _, row := range []struct{ name, mode string }{
		{"a passing check", "default"},
		{"a refusing check", "plan"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rt := newPromptRuntime()
			f := newGrantFixture(t, rt)
			const id = "11111111-1111-4111-8111-111111111a04"
			f.submit(t, id, true)
			r := report(id, row.mode)
			rt.set(func(p *promptRuntime) { p.onRefusal = func() { f.clk.advance(2 * r.Wait) } })
			if got := f.ep.GrantedPrompt(r); got != "" {
				t.Fatalf("text after the wait ended = %q", got)
			}
			if rec := f.record(t, id); rec.State != protocol.StateRunning || rec.GrantTextGiven {
				t.Fatalf("record = %s mark %v; want running with no mark", rec.State, rec.GrantTextGiven)
			}
			rt.set(func(p *promptRuntime) { p.onRefusal = nil })
			f.clk.advance(3 * time.Minute)
			if err := f.ep.Reconcile(); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if rec := f.record(t, id); rec.State != protocol.StateRejected || rec.Code != protocol.CodeExpired {
				t.Fatalf("record at its deadline = %s/%s, want rejected expired", rec.State, rec.Code)
			}
		})
	}
}

// Review of the hand-over: the request's own deadline was read before the
// hand-over's reads, so a request that expired during them, inside a hook
// wait that was still open, was marked and its text given.
func TestGrantedHandoverPastTheRequestDeadlineGivesNoText(t *testing.T) {
	rt := newPromptRuntime()
	f := newGrantFixture(t, rt)
	const id = "11111111-1111-4111-8111-111111111a09"
	f.submit(t, id, true)
	r := report(id, "default")
	r.Wait = 10 * time.Minute
	rt.set(func(p *promptRuntime) { p.onRefusal = func() { f.clk.advance(3 * time.Minute) } })
	if got := f.ep.GrantedPrompt(r); got != "" {
		t.Fatalf("text after the request's deadline = %q", got)
	}
	if rec := f.record(t, id); rec.State != protocol.StateRejected || rec.Code != protocol.CodeExpired || rec.GrantTextGiven {
		t.Fatalf("record = %s/%s mark %v; want rejected expired with no mark", rec.State, rec.Code, rec.GrantTextGiven)
	}
}

// Merge check, rows for rules that no test held.
func TestGrantedStubRulesHoldOnlyForStubs(t *testing.T) {
	t.Run("a request not dispatched as a stub is not ended at its deadline", func(t *testing.T) {
		rt := newPromptRuntime()
		f := newGrantFixture(t, rt)
		const signed = "11111111-1111-4111-8111-111111111a05"
		submitReply(t, f.ep, linkSubmit(f.clk, "fake", signed), linkSource(hostA, "fake"))
		plain := fake.New("fake", "e_1")
		g := newGrantFixture(t, plain)
		const granted = "11111111-1111-4111-8111-111111111a06"
		g.submit(t, granted, true)
		for _, fx := range []struct {
			f  *grantFixture
			id string
		}{{f, signed}, {g, granted}} {
			fx.f.clk.advance(3 * time.Minute)
			if err := fx.f.ep.Reconcile(); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if rec := fx.f.record(t, fx.id); rec.State != protocol.StateRunning {
				t.Fatalf("record %s past its deadline = %s/%s, want running", fx.id, rec.State, rec.Code)
			}
		}
	})

	t.Run("a prompt after a refusal ended the record gets no text", func(t *testing.T) {
		f := newGrantFixture(t, newPromptRuntime())
		const id = "11111111-1111-4111-8111-111111111a07"
		f.submit(t, id, true)
		if got := f.ep.GrantedPrompt(report(id, "plan")); got != "" {
			t.Fatalf("text in a mode that does not ask = %q", got)
		}
		if got := f.ep.GrantedPrompt(report(id, "default")); got != "" {
			t.Fatalf("text after the refusal = %q", got)
		}
		if rec := f.record(t, id); rec.State != protocol.StateRejected || rec.GrantTextGiven {
			t.Fatalf("record = %s mark %v; want rejected with no mark", rec.State, rec.GrantTextGiven)
		}
	})

	t.Run("a request admitted after a restart is dispatched as a stub", func(t *testing.T) {
		rt := newPromptRuntime()
		f := &grantFixture{clk: &linkClock{t: time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)}, dir: t.TempDir()}
		f.crash = func(point string) error {
			if point == core.PointAfterReceived {
				return os.ErrDeadlineExceeded
			}
			return nil
		}
		f.open(t, rt)
		const id = "11111111-1111-4111-8111-111111111a08"
		src := linkSource(hostA, "fake")
		src.NativeSession, src.Credential = "fake", "cred-1"
		src.GrantDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000003"
		if _, err := f.ep.Handle(linkSubmit(f.clk, "fake", id), src); err == nil {
			t.Fatal("submit with a crash after the received write reported no error")
		}
		f.crash = nil
		f.open(t, rt)
		if err := f.ep.Reconcile(); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if rec := f.record(t, id); rec.State != protocol.StateRunning || !rec.GrantAtPrompt {
			t.Fatalf("record after the restart = %s mark %v; want running, dispatched as a stub", rec.State, rec.GrantAtPrompt)
		}
		if got := f.ep.GrantedPrompt(report(id, "default")); got != grantText {
			t.Fatalf("prompt = %q, want the text", got)
		}
	})
}
