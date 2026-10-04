package core_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// TestAsyncStorageFailureIsVisibleToTheOwner reproduces agent-message-queue
// -611.50: when the durable write of a completion failed, the endpoint built
// an uncertain projection that only test observers saw, so request.get and
// Wait kept showing running until Reconcile.
func TestAsyncStorageFailureIsVisibleToTheOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	const id = "11111111-1111-4111-8111-111111111150"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	hostDir := filepath.Join(store.Dir(), "requests", "local")
	if err := os.Chmod(hostDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(hostDir, 0o700) })
	if !rt.Complete(id, "done") {
		t.Fatal("no running run to complete")
	}

	ref := protocol.EncodeRef("local", "fake", id)
	get := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}
	reply, err := ep.Handle(get, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s := reply.(protocol.Reply).Snapshot; s.State != protocol.StateUncertain || s.Code != protocol.CodeStorageFull {
		t.Fatalf("get = %s/%s, want uncertain/storage_full", s.State, s.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s, err := ep.Wait(ctx, ref); err != nil || s.State != protocol.StateUncertain {
		t.Fatalf("wait = %s, %v; want uncertain", s.State, err)
	}

	// The disk recovers and Reconcile writes the result: the owner sees it.
	if err := os.Chmod(hostDir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := ep.Reconcile(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	reply, err = ep.Handle(get, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("get after reconcile: %v", err)
	}
	if s := reply.(protocol.Reply).Snapshot; s.State != protocol.StateCompleted {
		t.Fatalf("get after reconcile = %s, want completed", s.State)
	}
}
