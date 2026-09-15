package codex

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression for agent-message-queue-611.22.39 (gate 2026-09-15, union
// 42d923a4): threadStatus maps notLoaded/systemError to "unknown", and the
// thread/status/changed handler cleared a.activeTurn only when the mapped
// status was exactly "idle". A thread that reports notLoaded (Codex process
// restart, transcript not yet loaded) never sends turn/completed for the
// turn that was active before the restart, and the terminal memo is written
// only by turn/completed and lookupHistory — so the stale activeTurn wedged
// Submit busy for the life of the process. Fix: notLoaded/systemError mean
// the app-server itself has lost track of any previously-observed turn; the
// handler must clear the stale activeTurn (and mark the run state uncertain
// via the status) so the thread is usable again.
func TestGate3NotLoadedLeavesActiveTurnWedged(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithConfirmTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111g39"}
	epoch := s.Epoch

	// Start a turn so the attachment holds an activeTurn + busy status.
	srv.setTurnStartDelay(func() {
		srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"turnA"}}`)
	})
	admCh := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key, Epoch: epoch, Input: protocol.SubmitInput{Text: "say PONG"}})
		admCh <- err
	}()
	select {
	case <-admCh:
	case <-time.After(10 * time.Second):
		t.Fatal("first submit did not return")
	}
	att.mu.Lock()
	if att.activeTurn == "" {
		att.mu.Unlock()
		t.Fatal("setup: activeTurn empty, want a wedged turn")
	}
	wedgedTurn := att.activeTurn
	att.mu.Unlock()
	_ = wedgedTurn

	// The app-server restarts under us: it reports notLoaded. No
	// turn/completed will ever arrive for turnA (the process that ran it is
	// gone and its transcript is not loaded yet).
	srv.notify(t, "thread/status/changed", `{"threadId":"t1","status":{"type":"notLoaded"}}`)
	waitFor := func(cond func() bool, msg string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal(msg)
	}

	// The stale activeTurn must be dropped: the status source that observed
	// it is gone. Otherwise Submit refuses busy forever.
	waitFor(func() bool {
		att.mu.Lock()
		defer att.mu.Unlock()
		return att.activeTurn == ""
	}, "activeTurn still set after notLoaded (611.22.39)")

	// And the thread must accept a FRESH submit instead of refusing busy.
	// Verifier round-1 TEST FIX 1: the resubmit must use a DISTINCT key —
	// reusing `key` short-circuits on a.runs[req.Key] and never reaches the
	// busy predicate, making the assertion vacuous.
	key2 := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "22222222-2222-4222-8222-222222222g39"}
	done := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key2, Epoch: epoch, Input: protocol.SubmitInput{Text: "say PONG again"}})
		done <- err
	}()
	<-srv.calls // turn/start for key2
	// Confirm key2's run with its own userMessage item, as the real
	// app-server would — otherwise Submit times out uncertain.
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"turnB"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"turnB","item":{"type":"userMessage","id":"iB","clientId":"`+clientIDFor(key2)+`","content":[]}}`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resubmit after notLoaded: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resubmit did not return (wedge)")
	}
}

// Verifier round-1 TEST FIX 2 (negative half, agent-message-queue-611.22.39):
// an UNRECOGNISED status type must NOT clear a live activeTurn — the recut
// gates on the raw status string exactly because threadStatus maps unknown
// statuses to "unknown", and a future status meaning "still running" must
// not make Submit issue turn/start into a live turn.
func TestGate3UnrecognisedStatusKeepsActiveTurn(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithConfirmTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "33333333-3333-4333-8333-333333333g39"}
	epoch := s.Epoch

	srv.setTurnStartDelay(func() {
		srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"turnLIVE"}}`)
	})
	admCh := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key, Epoch: epoch, Input: protocol.SubmitInput{Text: "work"}})
		admCh <- err
	}()
	select {
	case <-admCh:
	case <-time.After(10 * time.Second):
		t.Fatal("submit did not return")
	}
	att.mu.Lock()
	// turn/started announced turnLIVE; the turn/start RPC response names
	// the fake's default u1 — either way a live turn is bound.
	liveTurn := att.activeTurn
	att.mu.Unlock()
	if liveTurn == "" {
		t.Fatal("setup: activeTurn empty, want a live turn")
	}

	// A status type this client does not know — today meaning unknown, but
	// plausibly "still running" in a future Codex release — must not clear
	// the live turn.
	srv.notify(t, "thread/status/changed", `{"threadId":"t1","status":{"type":"definitelyRunning"}}`)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		att.mu.Lock()
		cleared := att.activeTurn == ""
		att.mu.Unlock()
		if cleared {
			t.Fatal("unrecognised status cleared a live activeTurn (611.22.39 recut)")
		}
		time.Sleep(10 * time.Millisecond)
	}
	att.mu.Lock()
	if att.activeTurn != liveTurn {
		att.mu.Unlock()
		t.Fatalf("activeTurn = %q, want %q untouched", att.activeTurn, liveTurn)
	}
	att.mu.Unlock()
}
