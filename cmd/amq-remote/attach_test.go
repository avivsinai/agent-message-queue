package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Bead agent-message-queue-611.31: attach registers the invoking session in a
// running endpoint without a restart, and persists it for the next start.
func TestLiveRegisterAttachesAndPersistsTheTarget(t *testing.T) {
	root, err := os.MkdirTemp("", "ar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	stateDir := filepath.Join(root, stateDirName)
	store, err := requests.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	srv, err := ipc.Listen(stateDir, ep)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetRegistrar(liveRegistrar(root, stateDir, ep))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; _ = ep.Close(); _ = store.Close() })

	session, err := registerTarget(stateDir, ipc.RegisterRequest{Kind: "fake", Target: "fake", Epoch: "e_1"})
	if err != nil || session.TargetID != "fake" {
		t.Fatalf("register: %+v %v", session, err)
	}
	if native, err := nativeSessionOf(stateDir, "fake"); err != nil || native == "" {
		t.Fatalf("native: %q %v", native, err)
	}
	f, err := manifest.Load(manifest.DefaultPath(stateDir))
	if err != nil || len(f.Adapters) != 1 || f.Adapters[0].Target != "fake" {
		t.Fatalf("manifest: %+v %v", f.Adapters, err)
	}
}

// Codex #885 P1 #2: registering a target name the manifest already gives to
// another session kept the other session silently.
func TestPersistAdapterRefusesAnotherSessionsTarget(t *testing.T) {
	stateDir := t.TempDir()
	if err := persistAdapter(stateDir, manifest.Adapter{Kind: "fake", Target: "fake", Epoch: "e_1"}); err != nil {
		t.Fatal(err)
	}
	if err := persistAdapter(stateDir, manifest.Adapter{Kind: "fake", Target: "fake", Epoch: "e_2"}); err == nil {
		t.Fatal("a conflicting entry for the same target was accepted")
	}
}
