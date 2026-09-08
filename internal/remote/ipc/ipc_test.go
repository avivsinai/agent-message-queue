package ipc

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestSubmitStatusWaitOverSocket is the happy path for the local carrier: a
// client submits to the fake runtime, reads status, and waits for the result
// that the runtime later produces. Socket paths must stay short, so the state
// directory lives directly under the system temp root.
func TestSubmitStatusWaitOverSocket(t *testing.T) {
	stateDir, err := os.MkdirTemp("", "amqr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })

	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ep := core.New(core.Config{Store: store})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	server, err := Listen(stateDir, ep)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Serve(ctx) }()

	cmd := &protocol.Command{
		Schema:    protocol.SchemaCommand,
		Op:        protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111111201",
		TargetID:  "fake",
		Epoch:     "e_1",
		NotAfter:  protocol.FormatTime(time.Now().Add(time.Minute)),
		Input:     &protocol.SubmitInput{Text: "say hi"},
	}
	resp, err := Call(stateDir, Request{Command: cmd})
	if err != nil || resp.AsError() != nil {
		t.Fatalf("submit: %v / %v", err, resp.AsError())
	}
	var snap protocol.Snapshot
	if err := json.Unmarshal(resp.Reply, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.State != protocol.StateRunning || snap.CreatorHost != LocalHost {
		t.Fatalf("unexpected submit reply: %+v", snap)
	}

	resp, err = Call(stateDir, Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: snap.RequestRef}})
	if err != nil || resp.AsError() != nil {
		t.Fatalf("status: %v / %v", err, resp.AsError())
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		rt.Complete(cmd.RequestID, "hi")
	}()
	resp, err = Call(stateDir, Request{Wait: &WaitRequest{RequestRef: snap.RequestRef, TimeoutMS: 2000}})
	if err != nil || resp.AsError() != nil || resp.TimedOut {
		t.Fatalf("wait: %v / %v / timed out=%v", err, resp.AsError(), resp.TimedOut)
	}
	if err := json.Unmarshal(resp.Reply, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.State != protocol.StateCompleted || snap.Result == nil || snap.Result.Text != "hi" {
		t.Fatalf("unexpected wait reply: %+v", snap)
	}
	cancel()
	_ = ep.Close()
}
