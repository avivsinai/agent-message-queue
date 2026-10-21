package codex

import (
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression: #758, a delayed turn/start RPC continuation can restore stale
// activeTurn + status=busy AFTER the notLoaded/systemError handler cleared
// them. The restoration guard (state.Terminal() || terminalTurns) does
// not fire because the lost-state notification does not make the run
// terminal. Interleave: turn/start response delivered to waiter → readLoop
// processes notLoaded → handler clears → Submit continuation resumes and
// restores. Fix: a lost-state generation guard around the RPC — capture the
// generation before turn/start, validate at restoration; stale = drop the
// restore (report uncertain, keep the correlation). The raw-status
// allowlist stays; no terminal result is fabricated.
func TestLostStateDuringTurnStartDoesNotRestore(t *testing.T) {
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
	// pump. This is the deterministic version of the interleave:
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
	// Deterministic sync: bounded wait until the read pump has
	// processed the lost-state notification (gen bump is the fingerprint).
	waitLostStateGen(t, att, 1)

	// Release the RPC response: the Submit continuation resumes and, without
	// the generation guard, restores activeTurn="u1" + status=busy — the
	// stale state the handler just cleared.
	doRelease()

	select {
	case err := <-admCh:
		t.Logf("submit returned: %v", err)
	case <-time.After(10 * time.Second):
		// Snapshot under the lock, unlock, THEN fail —
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
		t.Fatalf("activeTurn = %q after lost-state clear + stale continuation restore; want empty", active)
	}
	if st == "busy" {
		t.Fatalf("status = busy restored by stale continuation; want idle/unknown")
	}

	// And a fresh distinct-key request must not be refused busy: capture the returned Admission and explicitly reject
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
			t.Fatalf("fresh distinct-key submit refused busy (stale continuation aftermath): the stale continuation wedged the thread state")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fresh submit after lost-state + stale continuation did not return (wedge)")
	}
}

// Regression: #767, in the turn/start
// continuation, the terminal check must run BEFORE the lostStateGen guard.
// A terminal state (r.state.Terminal() || a.terminalTurns hit) is a
// POSITIVE fact that restores nothing — it only reports what already
// happened — so a notLoaded/systemError processed while the RPC was in
// flight cannot invalidate it. Ordered the other way (the pre-bead shape),
// a lost-state notification in flight downgrades a known-good completed
// outcome to uncertain: the continuation returns a bare error and the
// endpoint re-records evidence for a run that was already proven finished.
//
// Deterministic interleave, same harness as
// TestLostStateDuringTurnStartDoesNotRestore: the run is made terminal BEFORE the turn/start RPC
// is released (the read pump records the completed turn), the lost-state
// notification is processed while the continuation is parked on the RPC
// response, and only then is the response released. The continuation must
// return Admitted=true (the terminal fact), not the lost-state uncertain
// error.
func TestTerminalOutcomeSurvivesLostStateDuringTurnStart(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithConfirmTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111e5q"}
	epoch := s.Epoch

	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	srv.setTurnStartDelay(func() {
		<-release // hold the RPC response until terminal + lost state are applied
	})
	t.Cleanup(doRelease)

	admCh := make(chan core.Admission, 1)
	errCh := make(chan error, 1)
	go func() {
		a, err := att.Submit(core.BoundRequest{Key: key, Epoch: epoch, Input: protocol.SubmitInput{Text: "work"}})
		admCh <- a
		errCh <- err
	}()
	<-srv.calls // turn/start arrives; hook now holds the response

	// 1. Make the run terminal BEFORE the RPC response is released: the read
	// pump records the terminal memo for turn "u1" (the turn/start response's
	// turn id — same harness as the regression above). The run's own state
	// is NOT terminal here: r.turnID is bound only when the continuation
	// resumes, so the turn/completed handler cannot reach it via byTurn —
	// exactly the missed-confirmation shape the terminalTurns memo exists
	// for. The continuation's terminal check consults this memo.
	srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"u1","status":"completed"}}`)
	waitMemoForID(t, att, "u1")

	// 2. While the continuation is still parked, the app-server reports lost
	// state. The read pump bumps lostStateGen.
	srv.notify(t, "thread/status/changed", `{"threadId":"t1","status":{"type":"notLoaded"}}`)
	waitLostStateGen(t, att, 1)

	// 3. Release the RPC response. With the bead's ordering (terminal check
	// first) the continuation returns Admitted=true. With the pre-bead
	// ordering (generation guard first) it returns the lost-state uncertain
	// error — the known-good outcome downgraded.
	doRelease()

	var adm core.Admission
	select {
	case adm = <-admCh:
	case <-time.After(10 * time.Second):
		t.Fatal("submit did not return after RPC release")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("submit returned error %v; the terminal outcome must survive the in-flight lost-state notification (terminal is a positive fact that restores nothing)", err)
	}
	if !adm.Admitted {
		t.Fatalf("admission not Admitted (+v: %#v); a proven-terminal outcome was downgraded to uncertain by the lost-state guard ordering", adm)
	}
}
