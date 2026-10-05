package main

import (
	"context"
	"errors"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
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
	// A fixed invoking session, not this machine's ambient one (codex #895
	// r2: the first version passed only inside a real Claude session).
	saved := selfIdentity
	t.Cleanup(func() { selfIdentity = saved })
	selfIdentity = func(root, _ string) (string, string, error) {
		if root != dir {
			t.Errorf("detach resolved self under %q; want the bound root %q", root, dir)
		}
		return "claude:7", "session-7", nil
	}
	if code, err := detach([]string{"--self"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("detach outside AMQ: code=%d err=%v", code, err)
	}
	if _, err := binding.Read(); !errors.Is(err, binding.ErrNone) {
		t.Fatalf("binding still present after detach: %v", err)
	}
}

// Bead agent-message-queue-94w (review F13): detach with no scope removed
// every binding; it needs --self, --name or --all.
func TestDetachNeedsAScope(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "binding.json"))
	for _, n := range []string{"one", "two", "three"} {
		b := binding.Binding{Carrier: binding.CarrierMailbox, Root: dir, Handle: n, Name: n}
		if err := binding.WriteNamed(b); err != nil {
			t.Fatal(err)
		}
	}
	if code, err := detach(nil, io.Discard, io.Discard); code != protocol.ExitUsage || err == nil {
		t.Fatalf("detach with no scope: code=%d err=%v; want usage error", code, err)
	}
	if all, _ := binding.List(); len(all) != 3 {
		t.Fatalf("%d bindings after a refused detach; want 3", len(all))
	}
	if code, err := detach([]string{"--name", "two"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("detach --name: code=%d err=%v", code, err)
	}
	if all, _ := binding.List(); len(all) != 2 {
		t.Fatalf("%d bindings after detach --name; want 2", len(all))
	}
	if code, err := detach([]string{"--all"}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("detach --all: code=%d err=%v", code, err)
	}
	if all, _ := binding.List(); len(all) != 0 {
		t.Fatalf("%d bindings after detach --all; want 0", len(all))
	}
}

// Bead agent-message-queue-za4 (review F12): a reply to a Buzz DM warned
// "may not be read" because attach never listed buzz in the root's roster.
func TestAttachListsBuzzInTheRosterOnce(t *testing.T) {
	root := canonicalTempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "meta"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "meta", "config.json")
	if err := os.WriteFile(cfg, []byte(`{"version":1,"created_utc":"2026-01-01T00:00:00Z","agents":["claude"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := listBuzzInRoster(root); err != nil {
			t.Fatal(err)
		}
	}
	got, err := config.LoadConfig(cfg)
	if err != nil || len(got.Agents) != 2 || got.Agents[1] != "buzz" {
		t.Fatalf("agents = %v err=%v; want [claude buzz]", got.Agents, err)
	}
	bare := canonicalTempDir(t)
	if err := listBuzzInRoster(bare); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bare, "meta", "config.json")); err == nil {
		t.Fatal("attach created a config.json in a root that had none")
	}
}

// Pro review of 94w: a whitespace-only native --name picked the default name
// but counted as explicit, so it replaced another session's binding.
func TestNativeBlankNameIsNotExplicit(t *testing.T) {
	name, explicit := nativeBindingName("  ", "claude:7")
	if explicit || name == "" {
		t.Fatalf("name=%q explicit=%v; want the default name, not explicit", name, explicit)
	}
}
