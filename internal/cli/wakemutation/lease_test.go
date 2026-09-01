//go:build darwin || linux

package wakemutation

import (
	"errors"
	"sync/atomic"
	"testing"
)

func TestLeaseCloseWaitsForInFlightEffect(t *testing.T) {
	lease := New(nil)
	entered := make(chan struct{})
	continueEffect := make(chan struct{})
	effectDone := make(chan error, 1)
	go func() {
		effectDone <- lease.withEffect(func() error {
			close(entered)
			<-continueEffect
			return nil
		})
	}()
	<-entered

	closeDone := make(chan error, 1)
	go func() { closeDone <- lease.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before effect completed: %v", err)
	default:
	}
	close(continueEffect)
	if err := <-effectDone; err != nil {
		t.Fatalf("effect: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := lease.withEffect(func() error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("effect after Close = %v, want ErrClosed", err)
	}
}

func TestLeaseCloseRunsReleaseOnce(t *testing.T) {
	var releases atomic.Int32
	lease := New(func() error {
		releases.Add(1)
		return nil
	})
	if err := lease.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("release calls = %d, want 1", got)
	}
}
