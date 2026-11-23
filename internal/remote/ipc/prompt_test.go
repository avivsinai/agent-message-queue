package ipc

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// A session hook's prompt report reaches the endpoint's prompt handler whole
// and comes back with its text; an endpoint with no handler refuses it, so
// the hook gives nothing.
func TestPromptReportOverSocket(t *testing.T) {
	stateDir, err := os.MkdirTemp("", "amqp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ep := core.New(core.Config{Store: store})
	t.Cleanup(func() { _ = ep.Close() })
	server, err := Listen(stateDir, ep)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Serve(ctx) }()

	want := PromptRequest{TargetID: "cc-1", Instance: "owner-1", NativeSession: "sess-1", Mode: "default", StubID: "stub-1", WaitMS: 1500}
	if _, err := Prompt(stateDir, want, time.Second); err == nil {
		t.Fatal("an endpoint with no prompt handler answered the report")
	}
	var got PromptRequest
	server.SetPromptHandler(func(r PromptRequest) PromptReply {
		got = r
		return PromptReply{Text: "the task"}
	})
	reply, err := Prompt(stateDir, want, time.Second)
	if err != nil || reply.Text != "the task" {
		t.Fatalf("Prompt = %+v, %v; want the handler's text", reply, err)
	}
	if got != want {
		t.Fatalf("handler got %+v, want %+v", got, want)
	}
}
