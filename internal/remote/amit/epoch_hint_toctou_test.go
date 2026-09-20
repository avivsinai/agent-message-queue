package amit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSubmitEpochHintValidatedByGate pins review 816-r3's P1: the §4 epoch
// gate and the published epoch_hint must be ONE critical section. A
// concurrent receipt pin (or a refused(generation) unpin) inside the window
// between the gate check and the publish must not change the hint: a request
// the gate validated against generation G may never carry a different (or
// omitted) generation, because the extension treats an empty hint as "no
// check" — defeating stale-epoch protection on the very request whose
// generation was just proven stale.
func TestSubmitEpochHintValidatedByGate(t *testing.T) {
	key := testKey("toctou")
	ref := clientRef(key)
	other := testKey("regen")
	dir := newExtDir(t)

	// Hold the first liveness call open while another goroutine re-pins the
	// generation from a receipt — the exact interleaving the P1 window
	// allowed after the 9a restructure (a concurrent reconcile Lookup
	// observing a session regeneration receipt while a fresh Submit sits
	// inside its lock-free liveness gate).
	gate := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	liveHook := func(now time.Time) livenessState {
		// First liveness call: release the racer, then hold this window
		// open until the racer's re-pin has landed.
		once.Do(func() {
			close(gate) // tell the racer to re-pin now
			<-release   // hold the window open until the pin landed
		})
		return livenessState{live: true}
	}

	stampLiveness(t, dir, fixedNow)
	a, err := New("amit", "agent1", bridgeDir{dir: dir, live: liveHook})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.now = func() time.Time { return fixedNow }
	a.submitWait = 50 * time.Millisecond

	// Pin gen-1 for OUR key so the gate has a pinned generation to validate
	// against, WITHOUT confirming our run (the receipt for our ref does not
	// exist; only the other session's does). lateBind pins via the other
	// receipt below.
	writeReceipt(t, dir, clientRef(other), "gen-1", fixedNow)
	if _, err := a.Lookup(other, "gen-1"); err != nil {
		t.Fatalf("lookup other: %v", err)
	}
	a.mu.Lock()
	pinned := a.epoch
	a.mu.Unlock()
	if pinned != "gen-1" {
		t.Fatalf("epoch = %q, want gen-1 (pinned by the other key's receipt)", pinned)
	}

	// The racer: while Submit sits in its lock-free liveness gate, a
	// gen-2 receipt lands (session regenerated) and a reconcile-style
	// Lookup of a THIRD, never-seen key re-pins a.epoch to gen-2 (a
	// confirmed run's receipt is not re-read by consume, so the racer
	// must arrive through a fresh unconfirmed observation).
	third := testKey("third")
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-gate
		writeReceipt(t, dir, clientRef(third), "gen-2", fixedNow)
		if _, err := a.Lookup(third, "gen-1"); err != nil {
			t.Errorf("lookup third: %v", err)
		}
		close(release)
	}()

	// The bridge delivers while the racer holds the window open: our
	// receipt lands mid-poll, so Submit ends admitted (post-fix; the
	// receipt poll's apply re-confirms under the lock).
	bridgeDone := make(chan struct{})
	go func() {
		defer close(bridgeDone)
		<-release
		writeReceipt(t, dir, ref, "gen-1", fixedNow)
	}()

	// Submit bound to gen-1: the gate validates against a.epoch == "gen-1",
	// then the liveness read opens the window, the racer re-pins to gen-2,
	// and the pre-fix code re-read a.epoch AFTER the window — publishing
	// a hint the gate never validated.
	req := submitReq(key, "hello")
	req.Epoch = "gen-1"
	adm, serr := a.Submit(req)
	<-done
	<-bridgeDone

	if serr != nil || !adm.Admitted {
		t.Fatalf("Submit = %+v, %v; want admitted (bridge wrote the receipt mid-poll)", adm, serr)
	}

	// The published request file must carry the hint the gate validated.
	data, rerr := os.ReadFile(filepath.Join(dir, "requests", refSanitize(ref)+".json"))
	if rerr != nil {
		t.Fatalf("read published request: %v", rerr)
	}
	if want := `"epoch_hint":"gen-1"`; !contains(string(data), want) {
		t.Fatalf("TOCTOU: request published with an epoch_hint the gate never validated;\n%s\nwant %s", data, want)
	}
}

// TestSubmitEpochHintNotOmittedByUnpin pins the second half of the P1: a
// concurrent refused(generation) unpin (a.epoch -> "") inside the window
// must not drop the hint — the extension defines an empty epoch_hint as
// "first contact, no check", which would skip the generation check on a
// request whose generation was just proven stale.
func TestSubmitEpochHintNotOmittedByUnpin(t *testing.T) {
	key := testKey("unpin")
	ref := clientRef(key)
	other := testKey("unpin-other")
	dir := newExtDir(t)

	gate := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	liveHook := func(now time.Time) livenessState {
		once.Do(func() {
			close(gate)
			<-release
		})
		return livenessState{live: true}
	}
	a, err := New("amit", "agent1", bridgeDir{dir: dir, live: liveHook})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.now = func() time.Time { return fixedNow }
	a.submitWait = 50 * time.Millisecond

	// Pin gen-1 via a DIFFERENT key's receipt so OUR run stays unconfirmed
	// and Submit takes the fresh path through the liveness gate.
	writeReceipt(t, dir, clientRef(other), "gen-1", fixedNow)
	stampLiveness(t, dir, fixedNow)
	if _, err := a.Lookup(other, "gen-1"); err != nil {
		t.Fatalf("lookup: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		<-gate
		// Force the refused(generation) unpin path (review 816-r3 P1, the
		// gen-1 -> "" direction): a refused event whose reason is
		// "generation" drops a.epoch back to the empty sentinel.
		a.mu.Lock()
		if r, ok := a.runs[other]; ok {
			r.refused = refusalCodeFor("generation")
			a.epoch = ""
		}
		a.mu.Unlock()
		close(release)
	}()

	// The bridge delivers while the racer holds the window open: our
	// receipt lands mid-poll, so Submit ends admitted.
	bridgeDone := make(chan struct{})
	go func() {
		defer close(bridgeDone)
		<-release
		writeReceipt(t, dir, ref, "gen-1", fixedNow)
	}()

	req := submitReq(key, "hello")
	req.Epoch = "gen-1"
	adm, serr := a.Submit(req)
	<-done
	<-bridgeDone
	if serr != nil || !adm.Admitted {
		t.Fatalf("Submit = %+v, %v; want admitted", adm, serr)
	}
	data, rerr := os.ReadFile(filepath.Join(dir, "requests", refSanitize(ref)+".json"))
	if rerr != nil {
		t.Fatalf("read published request: %v", rerr)
	}
	if want := `"epoch_hint":"gen-1"`; !contains(string(data), want) {
		t.Fatalf("TOCTOU: unpin dropped the hint the gate validated (empty = no check);\n%s\nwant %s", data, want)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
