package codex

import (
	"sync"
	"testing"
	"time"
)

// Regression for BEAD jb7 (lead dispatch post-#767): testDispatchGate is
// package-level state written by tests and read by the read pump on every
// server request. As a plain package var it was synchronisation-free —
// safe only while no parallel test touched it. It is now an
// atomic.Pointer[func(ServerRequest)] with setTestDispatchGate as the
// single write path; this test runs a concurrent writer (arm/clear cycles)
// against concurrent readers (the same read the pump performs) under -race.
// On the pre-bead plain-var form this fires the race detector
// deterministically; with the atomic pointer it is clean.
func TestDispatchGateConcurrentAccessIsSynchronised(t *testing.T) {
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: arm/clear cycles — the exact shape test setup + Cleanup do.
	wg.Add(1)
	go func() {
		defer wg.Done()
		fn := func(sr ServerRequest) {}
		for {
			select {
			case <-stop:
				return
			default:
			}
			prev := setTestDispatchGate(fn)
			setTestDispatchGate(prev)
		}
	}()

	// Readers: load-and-call-nil-check, the exact shape
	// dispatchServerRequest performs on every server request.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if gate := testDispatchGate.Load(); gate != nil {
					_ = gate
				}
			}
		}()
	}

	// Bounded burn: long enough for the race detector to see the unsynchronized
	// pair many times over on the reverted shape.
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Leave the gate exactly as production expects it: nil.
	setTestDispatchGate(nil)
	if gate := testDispatchGate.Load(); gate != nil {
		t.Fatal("testDispatchGate not cleared after concurrent test; production would park the read pump")
	}
}
