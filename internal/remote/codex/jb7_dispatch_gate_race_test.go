package codex

import (
	"sync"
	"testing"
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
	writerDone := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(writerDone)
		fn := func(sr ServerRequest) {}
		// 611.22.33 test-bar: a FIXED NUMBER of arm/clear cycles, not a
		// wall-clock burn. The old 150ms sleep made the race coverage a
		// function of machine speed and added a flat 150ms to every run;
		// now the burn is exactly 20k cycles and ends when the work ends.
		// 20k unsynchronized write/read pairs saturate the race detector's
		// shadow memory many times over — on the reverted plain-var shape
		// this fires DATA RACE warnings deterministically (witnessed 4
		// warnings on the reverted shape with the cycle-count burn).
		for i := 0; i < 20000; i++ {
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

	// The burn ends when the writer finishes its fixed cycle count — no
	// wall-clock wait, no premature or stretched coverage.
	<-writerDone
	close(stop)
	wg.Wait()

	// Leave the gate exactly as production expects it: nil.
	setTestDispatchGate(nil)
	if gate := testDispatchGate.Load(); gate != nil {
		t.Fatal("testDispatchGate not cleared after concurrent test; production would park the read pump")
	}
}
