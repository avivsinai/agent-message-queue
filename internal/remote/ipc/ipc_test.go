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
	var rep protocol.Reply
	if err := json.Unmarshal(resp.Reply, &rep); err != nil {
		t.Fatal(err)
	}
	snap := rep.Snapshot
	if snap.State != protocol.StateRunning || snap.CreatorHost != LocalHost {
		t.Fatalf("unexpected submit reply: %+v", rep)
	}

	resp, err = Call(stateDir, Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: snap.RequestRef}})
	if err != nil || resp.AsError() != nil {
		t.Fatalf("status: %v / %v", err, resp.AsError())
	}
	_ = rep

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

// TestWaitReportsShutdownAsTimedOutNotFailure reproduces
// agent-message-queue-611.22.27: the wait handler special-cased only
// context.DeadlineExceeded, so when the endpoint shut down under a live wait
// the resulting context.Canceled fell through to errorResponse and the client
// mapped it to exit 1 — "the work failed". Wait never cancels work: the
// request was untouched and still running. An orchestrator told its request
// failed during a routine endpoint restart takes a destructive recovery path.
// Any context error means "not observed yet" (TimedOut, exit 4).
func TestWaitReportsShutdownAsTimedOutNotFailure(t *testing.T) {
	dir, err := os.MkdirTemp("", "amqr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	store, err := requests.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ep := core.New(core.Config{Store: store})
	rt := fake.New("fake", "e_1")
	ep.Register(rt)

	ctx, cancel := context.WithCancel(context.Background())
	srv, err := Listen(dir, ep)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); _ = ep.Close() })

	// Submit a request so there is something live to wait on.
	id := "11111111-1111-4111-8111-1111111119a1"
	submit := &protocol.Command{
		Schema:    protocol.SchemaCommand,
		Op:        protocol.OpRequestSubmit,
		RequestID: id,
		TargetID:  "fake",
		Epoch:     "e_1",
		NotAfter:  protocol.FormatTime(time.Now().Add(time.Minute)),
		Input:     &protocol.SubmitInput{Text: "work"},
	}
	if _, err := Call(dir, Request{Command: submit}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Wait with a generous client timeout, then shut the endpoint down under
	// it: the wait's context is cancelled, not deadline-exceeded.
	type result struct {
		resp *Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := Call(dir, Request{Wait: &WaitRequest{RequestRef: protocol.EncodeRef(LocalHost, "fake", id), TimeoutMS: 30000}})
		done <- result{resp, err}
	}()
	time.Sleep(150 * time.Millisecond) // let the wait register
	cancel()                           // endpoint shutdown under the live wait

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("wait call errored: %v", r.err)
		}
		if r.resp.Error != nil {
			t.Fatalf("shutdown reported as failure (%+v); a context error must never read as 'work failed'", r.resp.Error)
		}
		if !r.resp.TimedOut {
			t.Fatal("shutdown under a live wait must be reported as not-yet-observed (TimedOut), so the caller maps it to exit 4")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return after endpoint shutdown")
	}
}
