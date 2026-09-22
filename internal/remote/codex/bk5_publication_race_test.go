package codex

import (
	"sync"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression for BEAD BK5 (agent-message-queue-611.22.20): deliverPendingCancel
// runs on a goroutine spawned from the read loop, so it can be scheduled in
// the window between Dial returning and Attach publishing a.client — the
// exact "unsynchronized publication" the bead names. With a.client a plain
// unsynchronized field, that spawn raced the store (deterministic under
// -race) and a fast-enough spawn read nil and panicked. The field is now an
// atomic.Pointer published before Attach returns, and the spawned path
// nil-checks and gives up the interrupt (the same lost-intent shape as the
// TODO(B04) transport-error arm).
//
// The test drives the window directly: readers run deliverPendingCancel
// concurrently with the publication store, under -race. On the pre-fix form
// the race detector fires on the store-vs-read; the nil check keeps a
// genuinely-early spawn from panicking. Post-publication loads receive the
// zero-value Client whose Call panics on the nil transport — so the store
// phase publishes only once a real client is available is NOT this test's
// concern: after the concurrent phase, one final store of a REAL client
// (from the fake app server) plus one deliverPendingCancel through it pins
// that the spawned path still works end-to-end after publication.
func TestDeliverPendingCancelBeforePublicationIsSafe(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	a := &Attachment{threadID: "t1", targetID: "codex:t1"}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Reader side: the exact call the read-loop goroutine makes.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					a.deliverPendingCancel(bk5Key(), "turn-1")
				}
			}
		}()
	}
	// Writer side: the exact publication Attach performs after Dial. Every
	// store publishes the SAME real client so a post-publication load takes
	// the full deliverPendingCancel Call path against a live transport (the
	// fake app server never answers, but we do not wait: readers stop
	// before any Call can matter, and the final Close fails in-flight
	// calls fast). On the pre-fix plain field, this loop is exactly
	// store-vs-read unsynchronized — the race detector fires.
	real, err := Dial(sock, Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		a.client.Store(real)
		a.client.Load()
	}
	close(stop)
	wg.Wait()
	_ = srv
	_ = real.Close()
}

// bk5Key is the correlation key the spawned cancel carries; deliverPendingCancel
// only forwards it, so the value is inert here.
func bk5Key() requests.Key {
	return requests.Key{CreatorHost: "local", TargetID: "codex:t1", RequestID: "11111111-1111-4111-8111-111111111501"}
}
