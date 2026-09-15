package codex

import (
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression for BEAD e5q (lead dispatch after #767): in the turn/start
// continuation, the terminal check must run BEFORE the lostStateGen guard.
// A terminal state (r.state.Terminal() || a.terminalTurns hit) is a
// POSITIVE fact that restores nothing — it only reports what already
// happened — so a notLoaded/systemError processed while the RPC was in
// flight cannot invalidate it. Ordered the other way (the pre-bead shape),
// a lost-state notification in flight downgrades a known-good completed
// outcome to uncertain: the continuation returns a bare error and the
// endpoint re-records evidence for a run that was already proven finished.
//
// Deterministic interleave, same harness as TestGateR2LostStateDuringTurn
// StartDoesNotRestore: the run is made terminal BEFORE the turn/start RPC
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
	// turn id — same harness as the F758-1 regression). The run's own state
	// is NOT terminal here: r.turnID is bound only when the continuation
	// resumes, so the turn/completed handler cannot reach it via byTurn —
	// exactly the missed-confirmation shape the terminalTurns memo exists
	// for. The continuation's terminal check consults this memo.
	srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"u1","status":"completed"}}`)
	deadline := time.Now().Add(5 * time.Second)
	terminal := false
	for {
		att.mu.Lock()
		terminal = att.terminalTurns["u1"]
		att.mu.Unlock()
		if terminal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run never became terminal from the idle notification; test preconditions not met")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// 2. While the continuation is still parked, the app-server reports lost
	// state. The read pump bumps lostStateGen.
	srv.notify(t, "thread/status/changed", `{"threadId":"t1","status":{"type":"notLoaded"}}`)
	deadline = time.Now().Add(5 * time.Second)
	for {
		att.mu.Lock()
		bumped := att.lostStateGen == uint64(1)
		att.mu.Unlock()
		if bumped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lost-state notification never processed by the read pump")
		}
		time.Sleep(2 * time.Millisecond)
	}

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
		t.Fatalf("submit returned error %v; the terminal outcome must survive the in-flight lost-state notification (bead e5q: terminal is a positive fact that restores nothing)", err)
	}
	if !adm.Admitted {
		t.Fatalf("admission not Admitted (+v: %#v); a proven-terminal outcome was downgraded to uncertain by the lost-state guard ordering (bead e5q)", adm)
	}
}
