package codex

import (
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression for agent-message-queue-611.22.39-r2 (F758-1, yoetz round on
// PR #758): a delayed turn/start RPC continuation can restore stale
// activeTurn + status=busy AFTER the notLoaded/systemError handler cleared
// them. The B3 restoration guard (state.Terminal() || terminalTurns) does
// not fire because the lost-state notification does not make the run
// terminal. Interleave: turn/start response delivered to waiter → readLoop
// processes notLoaded → handler clears → Submit continuation resumes and
// restores. Fix: a lost-state generation guard around the RPC — capture the
// generation before turn/start, validate at restoration; stale = drop the
// restore (report uncertain, keep the correlation). The raw-status
// allowlist stays; no terminal result is fabricated.
func TestGateR2LostStateDuringTurnStartDoesNotRestore(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithConfirmTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111r21"}
	epoch := s.Epoch

	// Gate the Submit continuation: hold the turn/start response in the fake
	// server until we have processed the lost-state notification on the read
	// pump. This is the deterministic version of the yoetz interleave:
	// RPC response written → readLoop processes notLoaded (handler clears)
	// → Submit continuation resumes and would restore.
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	srv.setTurnStartDelay(func() {
		<-release // hold the RPC response until the lost state is applied
	})
	t.Cleanup(doRelease)

	admCh := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key, Epoch: epoch, Input: protocol.SubmitInput{Text: "work"}})
		admCh <- err
	}()
	<-srv.calls // turn/start arrives; hook now holds the response

	// While the continuation is parked, the app-server reports lost state.
	// The read pump processes this NOW, before the continuation resumes.
	srv.notify(t, "thread/status/changed", `{"threadId":"t1","status":{"type":"notLoaded"}}`)
	// Deterministic sync (7xl rule): bounded wait until the read pump has
	// processed the lost-state notification (gen bump is the fingerprint).
	deadline := time.Now().Add(5 * time.Second)
	for {
		att.mu.Lock()
		cleared := att.lostStateGen == uint64(1)
		att.mu.Unlock()
		if cleared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lost-state notification never processed by the read pump")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Release the RPC response: the Submit continuation resumes and, without
	// the generation guard, restores activeTurn="u1" + status=busy — the
	// stale state the handler just cleared.
	doRelease()

	select {
	case err := <-admCh:
		t.Logf("submit returned: %v", err)
	case <-time.After(10 * time.Second):
		// t2 (yoetz r2 note): snapshot under the lock, unlock, THEN fail —
		// t.Fatalf while holding att.mu poisons any later assertion in this
		// test (and goroutine dumps) with a deadlocked lookup.
		att.mu.Lock()
		snap := struct {
			gen  uint64
			turn string
			st   string
			runs int
		}{att.lostStateGen, att.activeTurn, att.status, len(att.runs)}
		att.mu.Unlock()
		t.Fatalf("submit did not return (gen=%d activeTurn=%q status=%q runs=%d)", snap.gen, snap.turn, snap.st, snap.runs)
	}

	// The stale restore must not have happened.
	att.mu.Lock()
	active, st := att.activeTurn, att.status
	att.mu.Unlock()
	if active != "" {
		t.Fatalf("activeTurn = %q after lost-state clear + stale continuation restore (F758-1); want empty", active)
	}
	if st == "busy" {
		t.Fatalf("status = busy restored by stale continuation (F758-1); want idle/unknown")
	}

	// And a fresh distinct-key request must not be refused busy (t1, yoetz
	// r2 note): capture the returned Admission and explicitly reject
	// CodeBusy — accepting any return would let a busy-refusal pass
	// silently, so "not refused busy" would not be independently proven.
	var adm2 core.Admission
	key2 := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "22222222-2222-4222-8222-222222222r21"}
	done := make(chan error, 1)
	go func() {
		a, err := att.Submit(core.BoundRequest{Key: key2, Epoch: epoch, Input: protocol.SubmitInput{Text: "again"}})
		adm2 = a
		done <- err
	}()
	select {
	case <-done:
		if adm2.Code == protocol.CodeBusy {
			t.Fatalf("fresh distinct-key submit refused busy (F758-1 aftermath): the stale continuation wedged the thread state")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fresh submit after lost-state + stale continuation did not return (wedge)")
	}
}
