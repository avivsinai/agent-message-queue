package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRouteExplainSameSession(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	sourceRoot := filepath.Join(baseRoot, "collab")
	ensureRouteAgents(t, sourceRoot, "alice", "bob")
	configureSendTestRoot(t, sourceRoot, "alice", "bob")

	result := runRouteExplainJSONForTest(t,
		"--from-root", sourceRoot,
		"--me", "alice",
		"--to", "bob",
	)

	if !result.Routable {
		t.Fatalf("expected routable route, got error: %s", result.Error)
	}
	expectSamePath(t, result.SourceRoot, sourceRoot)
	expectSamePath(t, result.DeliveryRoot, sourceRoot)
	if result.SourceSession != "collab" {
		t.Errorf("source_session = %q, want collab", result.SourceSession)
	}
	if result.TargetSession != "collab" {
		t.Errorf("target_session = %q, want collab", result.TargetSession)
	}
	wantArgv := []string{"amq", "send", "--root", sourceRoot, "--me", "alice", "--to", "bob"}
	expectStringSlice(t, result.Argv, wantArgv)
}

func TestRouteExplainFromCWD(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "project-a")
	sourceRoot := filepath.Join(projectDir, ".agent-mail")
	ensureRouteAgents(t, sourceRoot, "alice", "bob")
	configureSendTestRoot(t, sourceRoot, "alice", "bob")
	writeRouteAmqrc(t, projectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "project-a",
	})

	outsideDir := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv(envRoot, "")
	t.Setenv(envGlobalRoot, "")
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatalf("chdir outside: %v", err)
	}
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	result := runRouteExplainJSONForTest(t,
		"--from-cwd", projectDir,
		"--me", "alice",
		"--to", "bob",
	)

	if !result.Routable {
		t.Fatalf("expected routable route, got error: %s", result.Error)
	}
	expectSamePath(t, result.SourceRoot, sourceRoot)
	expectSamePath(t, result.DeliveryRoot, sourceRoot)
	if result.SourceProject != "project-a" {
		t.Errorf("source_project = %q, want project-a", result.SourceProject)
	}
}

func runRouteExplainJSONForTest(t *testing.T, args ...string) routeExplainResult {
	t.Helper()

	outArgs := append([]string{"--json"}, args...)
	output, err := captureEnvStdout(t, func() error {
		return runRouteExplain(outArgs)
	})
	if err != nil {
		t.Fatalf("runRouteExplain: %v", err)
	}

	var result routeExplainResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal route explain output: %v, output was: %s", err, output)
	}
	return result
}

func ensureRouteAgents(t *testing.T, root string, agents ...string) {
	t.Helper()

	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs(%q): %v", root, err)
	}
	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%q, %q): %v", root, agent, err)
		}
	}
}

func writeRouteAmqrc(t *testing.T, dir string, value map[string]any) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal amqrc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".amqrc"), data, 0o600); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}
}

func expectStringSlice(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("slice length = %d, want %d; got %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slice[%d] = %q, want %q; got %v", i, got[i], want[i], got)
		}
	}
}

// TestB7ResolveReplyRouteSessionRootSource covers the case where the source
// endpoint is running inside a session root (.agent-mail/s1). A reply to
// handle@qa must resolve to the qa session root under the same base.
func TestB7ResolveReplyRouteSessionRootSource(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	// source session s1
	sourceRoot := filepath.Join(baseRoot, "s1")
	ensureRouteAgents(t, sourceRoot, "alice")
	// destination session qa
	destRoot := filepath.Join(baseRoot, "qa")
	ensureRouteAgents(t, destRoot, "bob")

	// replyTo: bob@qa, no project (same project, different session)
	root, handle, err := ResolveReplyRoute(sourceRoot, "", "bob@qa")
	if err != nil {
		t.Fatalf("ResolveReplyRoute: %v", err)
	}
	if handle != "bob" {
		t.Errorf("handle = %q, want bob", handle)
	}
	expectSamePath(t, root, destRoot)
}

// TestB7ResolveReplyRouteBaseRootSource covers the addendum: an endpoint
// running at the BASE root (.agent-mail, not a session root) replying to a
// caller in session qa of the same project. classifyRoot returns "" for a
// base root; the fix uses sourceRoot as the base directly.
func TestB7ResolveReplyRouteBaseRootSource(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	// base root has agents directly
	ensureRouteAgents(t, baseRoot, "alice")
	// destination session qa
	destRoot := filepath.Join(baseRoot, "qa")
	ensureRouteAgents(t, destRoot, "bob")

	// replyTo: bob@qa, no project. sourceRoot is the BASE root.
	root, handle, err := ResolveReplyRoute(baseRoot, "", "bob@qa")
	if err != nil {
		t.Fatalf("ResolveReplyRoute (base root): %v", err)
	}
	if handle != "bob" {
		t.Errorf("handle = %q, want bob", handle)
	}
	expectSamePath(t, root, destRoot)
}

// TestB7ResolveReplyRouteSessionNotFoundIsRetryable verifies that a missing
// session returns a retryable error (ErrPeerRootUnreachable), not a poison.
func TestB7ResolveReplyRouteSessionNotFoundIsRetryable(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), ".agent-mail")
	sourceRoot := filepath.Join(baseRoot, "s1")
	ensureRouteAgents(t, sourceRoot, "alice")
	// qa session does NOT exist

	_, _, err := ResolveReplyRoute(sourceRoot, "", "bob@qa")
	if err == nil {
		t.Fatal("expected error for missing session, got nil")
	}
	if !errors.Is(err, ErrPeerRootUnreachable) {
		t.Fatalf("missing session error = %v, want ErrPeerRootUnreachable (retryable, not poison)", err)
	}
}
