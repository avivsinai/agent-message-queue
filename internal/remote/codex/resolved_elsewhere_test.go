package codex

import (
	"runtime"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// 611.42 pre-merge review: the app-server fans an approval out to every
// client and drops every answer after the first with only a log line. When
// the Codex terminal answered first, a later Buzz answer still reported
// success. serverRequest/resolved must clear the approval, so the late
// answer is refused as already resolved.
func TestApprovalAnsweredElsewhereRefusesLateAnswer(t *testing.T) {
	sock, srv := startFakeAppServer(t)
	att, err := Attach(sock, "t1", WithApprovals(true))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = att.Close() })
	<-srv.calls // initialize
	<-srv.calls // thread/resume
	resolved := make(chan core.NativeEvent, 1)
	att.Subscribe(func(ev core.NativeEvent) {
		if ev.Type == core.EventQuestionResolved {
			resolved <- ev
		}
	})

	s := att.Inspect()
	key := requests.Key{CreatorHost: "local", TargetID: s.TargetID, RequestID: "11111111-1111-4111-8111-111111111442"}
	done := make(chan error, 1)
	go func() {
		_, err := att.Submit(core.BoundRequest{Key: key, Epoch: s.Epoch, Input: protocol.SubmitInput{Text: "run tests"}})
		done <- err
	}()
	<-srv.calls // turn/start
	srv.notify(t, "turn/started", `{"threadId":"t1","turn":{"id":"u1"}}`)
	srv.notify(t, "item/started", `{"threadId":"t1","turnId":"u1","item":{"type":"userMessage","id":"i1","clientId":"`+clientIDFor(key)+`","content":[]}}`)
	if err := <-done; err != nil {
		t.Fatalf("submit: %v", err)
	}
	srv.sendServerRequest(t, "7", "item/commandExecution/requestApproval", `{"threadId":"t1","turnId":"u1","itemId":"tool1","command":"go test ./..."}`)
	waitInteraction(t, att, key)

	// The terminal answers first; the app-server tells every client.
	srv.notify(t, "serverRequest/resolved", `{"threadId":"t1","requestId":"7"}`)
	select {
	case ev := <-resolved:
		if ev.Key != key || ev.Remote {
			t.Fatalf("resolution = %+v, want this run resolved outside AMQ", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serverRequest/resolved did not resolve the approval")
	}
	for i := 0; i < 1000 && att.Inspect().PendingInteraction != nil; i++ {
		runtime.Gosched()
	}
	if code, err := att.Respond(key, s.Epoch, "tool1", "accept"); err != nil || code != protocol.CodeAlreadyResolved {
		t.Fatalf("late Respond = (%q, %v), want already_resolved", code, err)
	}
}
