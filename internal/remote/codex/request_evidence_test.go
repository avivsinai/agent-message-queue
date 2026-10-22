package codex

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression for agent-message-queue-611.68 (deep review finding 5): a TUI
// turn started and completed while our turn/start response was in flight, the
// busy-join returned that turn's id, and Submit reported admitted from the
// terminal-turn memo although no userMessage carried our clientId.
func TestForeignTurnCompletionDoesNotAdmit(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithConfirmTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-1111111150b0"}

	release := make(chan struct{})
	srv.setTurnStartDelay(func() { <-release })
	type res struct {
		adm core.Admission
		err error
	}
	ch := make(chan res, 1)
	go func() {
		a, e := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "ours"}})
		ch <- res{a, e}
	}()
	<-srv.calls // turn/start, response held
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"m1","text":"typed in TUI"}}`)
	srv.notify(t, "turn/completed", `{"threadId":"t1","turn":{"id":"u1","status":"completed"}}`)
	waitMemoForID(t, att, "u1")
	close(release)
	r := <-ch
	if r.adm.Admitted || r.err == nil {
		t.Fatalf("foreign turn u1 reported as our admission: admission=%+v err=%v", r.adm, r.err)
	}
	att.mu.Lock()
	active := att.activeTurn
	att.mu.Unlock()
	if active != "" {
		t.Fatalf("finished foreign turn restored as active: %q", active)
	}
}

// Regression for agent-message-queue-611.68 (deep review finding 11, seen
// with go test -race): lookupHistory read the run's text after releasing
// a.mu while the read pump wrote it under a.mu.
func TestLookupHistoryReadsRunUnderLock(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithConfirmTimeout(20*time.Millisecond))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	go func() {
		for range srv.calls {
		}
	}()
	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-1111111111b0"}
	// Submit times out unconfirmed; the run stays with turn id u1.
	_, _ = att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "ours"}})
	cid := clientIDFor(key)
	srv.setThreadReadHandler(func() string {
		return `{"thread":{"turns":[{"id":"u1","status":"inProgress","items":[{"type":"userMessage","clientId":"` + cid + `"},{"type":"agentMessage","text":"history text"}]}]}}`
	})
	// Unconfirmed past confirmTimeout: Lookup goes to history and binds u1.
	if _, err := att.Lookup(key, s.Epoch); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			srv.notify(t, "item/completed", `{"threadId":"t1","turnId":"u1","item":{"type":"agentMessage","id":"a1","text":"pump text"}}`)
			time.Sleep(200 * time.Microsecond)
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := att.lookupHistory(key, s.Epoch); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	<-done
}

// Regression for agent-message-queue-611.68 (deep review finding 12): Submit
// sent turn/start for a request whose not_after had already passed.
func TestSubmitPastNotAfterSendsNothing(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithConfirmTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-1111111112b0"}
	past := protocol.FormatTime(time.Now().Add(-5 * time.Second))
	adm, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, NotAfter: past, Input: protocol.SubmitInput{Text: "late prompt"}})
	// The fake server queues each frame before it replies, so a frame Submit
	// sent is already in srv.calls here.
	select {
	case m := <-srv.calls:
		t.Fatalf("prompt sent after not_after=%s: method=%s", past, m.Method)
	default:
	}
	if err != nil || adm.Code != protocol.CodeExpired {
		t.Fatalf("want an expired refusal, got admission=%+v err=%v", adm, err)
	}
}
