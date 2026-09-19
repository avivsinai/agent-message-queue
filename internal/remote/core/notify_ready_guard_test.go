package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
)

// TestNotifySubmitReadyDoubleCallPanics (611.22.54 item 2): a second
// NotifySubmitReady with no intervening Submit would overwrite the first
// channel and orphan any waiter forever. The fake refuses the call.
func TestNotifySubmitReadyDoubleCallPanics(t *testing.T) {
	rt := fake.New("fake", "e_1")
	_ = rt.NotifySubmitReady()
	defer func() {
		if recover() == nil {
			t.Fatal("second NotifySubmitReady without an intervening Submit did not panic")
		}
	}()
	_ = rt.NotifySubmitReady()
}

// TestNotifyLookupReadyDoubleCallPanics pins the same guard on the Lookup
// readiness signal (611.22.54 item 2, Lookup analog).
func TestNotifyLookupReadyDoubleCallPanics(t *testing.T) {
	rt := fake.New("fake", "e_1")
	_ = rt.NotifyLookupReady()
	defer func() {
		if recover() == nil {
			t.Fatal("second NotifyLookupReady without an intervening Lookup did not panic")
		}
	}()
	_ = rt.NotifyLookupReady()
}

// TestNotifySubmitReadyConsumedAfterSubmit pins the happy path: after the
// announced Submit runs, the signal is consumed and a fresh Notify works.
func TestNotifySubmitReadyConsumedAfterSubmit(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	ready := rt.NotifySubmitReady()
	done := make(chan error, 1)
	go func() {
		_, err := ep.Handle(submitCmd("11111111-1111-4111-8111-1111111111d1"), core.Source{Host: "local"})
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("NotifySubmitReady never fired for the announced submit")
	}
	if err := <-done; err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Signal consumed: a second Notify is now legal again.
	_ = rt.NotifySubmitReady()
}
