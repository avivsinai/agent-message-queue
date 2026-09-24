package main

import (
	"context"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"io"
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

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Codex #895 P1 #4: mailbox attach accepted an AM_ROOT outside the pinned
// AM_BASE_ROOT/AM_SESSION.
func TestMailboxAttachRefusesAForeignRoot(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	foreign := filepath.Join(dir, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AM_ROOT", foreign)
	t.Setenv("AM_ME", "agent")
	t.Setenv("AM_BASE_ROOT", dir)
	t.Setenv("AM_SESSION", "authorized-session")
	if code, err := attach([]string{"--self"}, io.Discard, io.Discard); err == nil || code == 0 {
		t.Fatalf("a mismatched session pin was accepted: code=%d err=%v", code, err)
	}
}

// Codex #895 P2 #5: off failed outside AMQ because detach needed --root.
func TestDetachOutsideAMQUsesTheBoundRoot(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_ME", "")
	if err := binding.Write(binding.Binding{Root: dir, Target: "claude:7", NativeSession: "session-7"}); err != nil {
		t.Fatal(err)
	}
	if code, err := detach([]string{"--self"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("detach outside AMQ: code=%d err=%v", code, err)
	}
}
