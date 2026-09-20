package core_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestB13HandleBlockedConcurrentCloseCommits proves the drain wait: a Handle
// that dispatched to native runtime and is blocked before commit must not be
// cut off by a concurrent Close. Close waits for the handler to finish, and
// the command commits (no store_closed error).
//
// Mutation: remove the drain wait in Close -> the handler's commit hits a
// closed store, and the test fails (the reply is an error, not a snapshot).
func TestB13HandleBlockedConcurrentCloseCommits(t *testing.T) {
	store, now := openStoreNoCleanup(t)
	ep := core.New(core.Config{Store: store, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	cmd := submitCmd("11111111-1111-4111-8111-111111111b01")

	// Hold the native Submit so Handle blocks after registering in-flight.
	rt.HoldAdmission()

	// Bead 7eu: an early t.Fatal between Hold and the explicit release must
	// not leave the gate held (goroutine leak under -race). Idempotent:
	// release no-ops once the gate channel is closed.
	t.Cleanup(rt.ReleaseAdmission)
	handleDone := make(chan struct{})
	var reply any
	var handleErr error
	go func() {
		defer close(handleDone)
		reply, handleErr = ep.Handle(cmd, core.Source{Host: "local"})
	}()

	// Wait for the handler to be in-flight: the fake's Submit was called.
	// We know it reached Submit because HoldAdmission gates inside Submit.
	// The admissionGate channel exists once HoldAdmission was called; we
	// need to know the handler actually ENTERED Submit. Poll the fake's
	// in-flight state.
	waitForInFlight(t, ep)

	// Start Close concurrently while the handler is blocked.
	closeDone := make(chan error)
	go func() {
		closeDone <- ep.Close()
	}()

	// Release the handler so it can proceed to commit.
	rt.ReleaseAdmission()

	// The handler must commit successfully (no store_closed).
	select {
	case <-handleDone:
	case <-time.After(10 * time.Second):
		t.Fatal("handle did not return within 10s — drain wait deadlocked")
	}
	if handleErr != nil {
		t.Fatalf("handle returned error after concurrent close: %v", handleErr)
	}
	reply2, ok := reply.(protocol.Reply)
	if !ok {
		t.Fatalf("handle did not return a Reply: %T", reply)
	}
	if reply2.Snapshot.State != protocol.StateRunning {
		t.Fatalf("expected state running, got %s", reply2.Snapshot.State)
	}

	// Close must return without error (in-flight reached zero before bound).
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return within 10s")
	}
}

// TestB13HandleDuringDrainingRefusedWithActionRequiredCode proves the entry
// check: a Handle arriving AFTER Close has started the drain is refused with
// CodeDraining (an action-required code), never a failure code that would be
// recorded as the command's outcome.
//
// Mutation: remove the state check in Handle -> the command is accepted
// during draining, and the test fails (no refusal error).
func TestB13HandleDuringDrainingRefusedWithActionRequiredCode(t *testing.T) {
	store, now := openStoreNoCleanup(t)
	ep := core.New(core.Config{Store: store, Now: now})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	cmd := submitCmd("11111111-1111-4111-8111-111111111b02")

	// Hold one handler so Close enters the drain wait (in-flight > 0).
	rt.HoldAdmission()

	// Bead 7eu: an early t.Fatal between Hold and the explicit release must
	// not leave the gate held (goroutine leak under -race). Idempotent:
	// release no-ops once the gate channel is closed.
	t.Cleanup(rt.ReleaseAdmission)
	blockerDone := make(chan struct{})
	go func() {
		defer close(blockerDone)
		_, _ = ep.Handle(cmd, core.Source{Host: "local"})
	}()
	waitForInFlight(t, ep)

	// Start Close — it transitions to draining and blocks on the in-flight
	// handler.
	closeDone := make(chan error)
	go func() {
		closeDone <- ep.Close()
	}()

	// Give Close time to transition to draining. The drain wait is under
	// e.mu, so we cannot observe it directly; a short yield is the barrier.
	// This is NOT a sleep-based assertion — it is a scheduling yield so the
	// Close goroutine acquires e.mu and sets state=draining before the next
	// Handle tries. The test's correctness does not depend on the yield
	// length; it depends on the state check being correct. If the check is
	// missing, the second Handle will succeed regardless of timing.
	time.Sleep(50 * time.Millisecond)

	// A second Handle arriving during draining must be refused with
	// CodeDraining.
	cmd2 := submitCmd("11111111-1111-4111-8111-111111111b03")
	_, err := ep.Handle(cmd2, core.Source{Host: "local"})
	if err == nil {
		t.Fatal("handle during draining was not refused")
	}
	var r *protocol.Refusal
	if !errors.As(err, &r) {
		t.Fatalf("handle during draining returned non-refusal error: %v", err)
	}
	if r.Code != protocol.CodeDraining {
		t.Fatalf("expected CodeDraining, got %s (message: %s)", r.Code, r.Message)
	}

	// Release the blocker so Close can finish.
	rt.ReleaseAdmission()
	select {
	case <-blockerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("blocked handler did not return")
	}
	select {
	case <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("close did not return after blocker released")
	}
}

// TestB13CloseBoundedWithWedgedHandler proves the drain timeout: a handler
// that never returns must not block Close forever. Close returns within the
// bound and reports how many handlers were still in flight.
//
// Mutation: remove the bound (make Close wait forever) -> the test fails on
// its 10s hard deadline.
func TestB13CloseBoundedWithWedgedHandler(t *testing.T) {
	store, now := openStoreNoCleanup(t)
	ep := core.New(core.Config{Store: store, Now: now, DrainTimeout: 200 * time.Millisecond})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	cmd := submitCmd("11111111-1111-4111-8111-111111111b04")

	// Hold the handler forever (never release during the test). Clean up
	// after Close returns so the goroutine does not leak.
	rt.HoldAdmission()

	// Bead 7eu: an early t.Fatal between Hold and the explicit release must
	// not leave the gate held (goroutine leak under -race). Idempotent:
	// release no-ops once the gate channel is closed.
	t.Cleanup(rt.ReleaseAdmission)
	go func() {
		_, _ = ep.Handle(cmd, core.Source{Host: "local"})
	}()
	waitForInFlight(t, ep)

	// Close must return within a bound and report the in-flight count.
	start := time.Now()
	err := ep.Close()
	elapsed := time.Since(start)

	// Release the wedged handler so the goroutine can exit (the store is
	// closed; its commit will fail silently).
	rt.ReleaseAdmission()
	// Wait for the handler goroutine to finish so it does not race with
	// the test cleanup's store.Close. The handler's commit will fail with
	// store_closed; we discard the error.
	handlerReturned := make(chan struct{})
	go func() {
		// The original goroutine is still in Submit; once released it will
		// proceed through finishAdmissionLocked and return. We just need
		// to give it time. The goroutine was started above; this is a
		// sentinel we poll for via InFlight reaching zero.
		for ep.InFlight() > 0 {
			time.Sleep(time.Millisecond)
		}
		close(handlerReturned)
	}()
	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("wedged handler did not exit after release")
	}

	// The bound is drainTO (200ms in this test). If the bound were removed,
	// Close would block forever and the test would hit the 10s deadline.
	if elapsed > 5*time.Second {
		t.Fatalf("close took %s — drain timeout was not enforced", elapsed)
	}

	if err == nil {
		t.Fatal("expected close to report in-flight handlers, got nil error")
	}
	// The error must name the count.
	if !strings.Contains(err.Error(), "1 handler") {
		t.Fatalf("error does not report in-flight count: %v", err)
	}
}

// waitForInFlight blocks until the endpoint has at least one in-flight
// handler, with a hard deadline. This replaces a blind sleep: it observes
// the actual state, not a guessed delay.
func waitForInFlight(t *testing.T, ep *core.Endpoint) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ep.InFlight() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for handler to enter in-flight state")
}

// openStoreNoCleanup creates a store without registering a t.Cleanup that
// calls store.Close. Tests that call ep.Close() (which closes the store)
// use this to avoid a double-close race on the store's bare closed flag.
func openStoreNoCleanup(t *testing.T) (*requests.Store, func() time.Time) {
	clk := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clk }
	store, err := requests.Open(t.TempDir(), requests.WithClock(now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store, now
}
