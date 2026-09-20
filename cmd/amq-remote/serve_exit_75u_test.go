package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestServeCloseDrainIncompleteSurfacesNonzeroExit (bead 75u) proves the
// serve exit contract the composition promises: when ep.Close() cannot
// discharge the bounded drain (a handler still in flight at the drain
// timeout), serve returns that error and finish maps it to a nonzero exit
// with the error body named.
//
// RED if serve's close-error propagation (`if cerr := ep.Close(); cerr !=
// nil && err == nil`) is dropped, or if finish() swallowed the error.
//
// The test mirrors serve()'s composition exactly (store, endpoint, carrier,
// ipc.Listen, Serve(ctx), then Close) because serve() itself blocks on
// signal.NotifyContext and cannot be interrupted from a test. A blocked
// fake Submit (HoldAdmission, never released) keeps the Handle in flight
// past the 100ms DrainTimeout, so Close must report the in-flight handler
// and the mapped exit code must be ExitError (1).

type blockingFake struct {
	*fake.Runtime
}

func (b *blockingFake) Submit(req core.BoundRequest) (core.Admission, error) {
	// Hold admission until ReleaseAdmission; the test never releases it, so
	// the handler goroutine stays in flight across Close's drain bound.
	b.HoldAdmission()
	return b.Runtime.Submit(req)
}

func TestServeCloseDrainIncompleteSurfacesNonzeroExit(t *testing.T) {
	root, err := os.MkdirTemp("", "amq75u")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "extensions", "remote")

	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{
		Store:        store,
		Publish:      func(protocol.Snapshot, map[string]string) error { return nil },
		DrainTimeout: 100 * time.Millisecond,
	})
	bf := &blockingFake{Runtime: fake.New("fake", "e_1")}
	ep.Register(bf)
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	server, err := ipc.Listen(stateDir, ep)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(server.Path()) })
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()

	// Drive a real submit through the endpoint's Handle over the IPC socket
	// so the submit runs on a server goroutine and stays in flight on the
	// held admission gate.
	cmd := &protocol.Command{
		Schema:    protocol.SchemaCommand,
		Op:        protocol.OpRequestSubmit,
		RequestID: "11111111-1111-4111-8111-111111117501",
		TargetID:  "fake",
		Epoch:     "e_1",
		NotAfter:  time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339),
		Input:     &protocol.SubmitInput{Text: "75u drain probe"},
	}
	go func() {
		_, _ = ep.Handle(cmd, core.Source{Host: ipc.LocalHost})
	}()
	// Give the handler a beat to reach the held gate.
	time.Sleep(50 * time.Millisecond)

	var errBuf bytes.Buffer
	// Stop serve exactly as the signal path does, then Close.
	cancel()
	<-serveDone
	cerr := ep.Close()
	if cerr == nil {
		t.Fatal("expected Close to report the still-in-flight handler after the drain timeout; got nil")
	}
	if !bytes.Contains([]byte(cerr.Error()), []byte("still in flight")) {
		t.Fatalf("expected drain-incomplete naming in-flight handlers, got: %v", cerr)
	}
	// The exit contract: finish(w, nil, asJSON, code=0, err) must return
	// a nonzero exit (ExitError), not 0, and the error body must name the
	// drain failure.
	mapped := finish(&errBuf, nil, true, 0, cerr)
	if mapped != protocol.ExitError {
		t.Fatalf("finish mapped Close drain-incomplete to exit %d, want %d", mapped, protocol.ExitError)
	}
	if !bytes.Contains(errBuf.Bytes(), []byte("still in flight")) {
		t.Fatalf("expected error output to name the drain failure, got: %s", errBuf.String())
	}
}
