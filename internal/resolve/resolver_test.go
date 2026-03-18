// internal/resolve/resolver_test.go
package resolve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestProject(t *testing.T, base, name string, sessions map[string][]string) string {
	t.Helper()
	projDir := filepath.Join(base, name)
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	amqrc := map[string]string{"root": ".agent-mail", "project": name}
	data, _ := json.Marshal(amqrc)
	if err := os.WriteFile(filepath.Join(projDir, ".amqrc"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for sess, agents := range sessions {
		for _, agent := range agents {
			dir := filepath.Join(projDir, ".agent-mail", sess, "agents", agent, "inbox", "new")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(projDir, ".agent-mail", sess, "agents", agent, "inbox", "tmp"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(projDir, ".agent-mail", sess, "agents", agent, "inbox", "cur"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	return projDir
}

// --- Local resolution ---

func TestResolveLocal(t *testing.T) {
	base := t.TempDir()
	proj := setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude", "codex"},
	})
	sessionRoot := filepath.Join(proj, ".agent-mail", "collab")

	r := NewResolver(sessionRoot, filepath.Join(proj, ".agent-mail"), proj)
	targets, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Agent != "codex" {
		t.Fatalf("unexpected: %+v", targets)
	}
	if targets[0].Session != "collab" {
		t.Errorf("want session 'collab', got %q", targets[0].Session)
	}
	if targets[0].SessionRoot != sessionRoot {
		t.Errorf("wrong session root: %s", targets[0].SessionRoot)
	}
}

func TestResolveLocal_NotFound(t *testing.T) {
	base := t.TempDir()
	proj := setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude"},
	})
	sessionRoot := filepath.Join(proj, ".agent-mail", "collab")

	r := NewResolver(sessionRoot, filepath.Join(proj, ".agent-mail"), proj)
	_, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent agent")
	}
}

func TestResolveLocal_NoAutoCreate(t *testing.T) {
	// Verify that the resolver does NOT create directories for agents
	// that don't exist. The inbox must pre-exist.
	base := t.TempDir()
	proj := setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude"},
	})
	sessionRoot := filepath.Join(proj, ".agent-mail", "collab")

	r := NewResolver(sessionRoot, filepath.Join(proj, ".agent-mail"), proj)
	_, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "phantom"})
	if err == nil {
		t.Fatal("expected error for agent without pre-existing mailbox")
	}

	// Verify no directories were created
	phantomInbox := filepath.Join(sessionRoot, "agents", "phantom", "inbox")
	if _, statErr := os.Stat(phantomInbox); statErr == nil {
		t.Fatal("resolver must NOT auto-create mailbox directories")
	}
}

// --- Cross-session resolution ---

func TestResolveCrossSession(t *testing.T) {
	base := t.TempDir()
	proj := setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude", "codex"},
		"auth":   {"claude", "codex"},
	})
	sessionRoot := filepath.Join(proj, ".agent-mail", "collab")

	r := NewResolver(sessionRoot, filepath.Join(proj, ".agent-mail"), proj)
	targets, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex", Session: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if targets[0].SessionRoot != filepath.Join(proj, ".agent-mail", "auth") {
		t.Errorf("wrong session root: %s", targets[0].SessionRoot)
	}
	if targets[0].Agent != "codex" {
		t.Errorf("wrong agent: %s", targets[0].Agent)
	}
}

func TestResolveCrossSession_NotFound(t *testing.T) {
	base := t.TempDir()
	proj := setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude"},
	})

	r := NewResolver(
		filepath.Join(proj, ".agent-mail", "collab"),
		filepath.Join(proj, ".agent-mail"),
		proj,
	)
	_, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex", Session: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

// --- Cross-project resolution ---

func TestResolveCrossProject(t *testing.T) {
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude", "codex"},
	})
	infraProj := setupTestProject(t, base, "infra-lib", map[string][]string{
		"collab": {"claude", "codex"},
	})
	_ = infraProj

	appRoot := filepath.Join(base, "my-app", ".agent-mail", "collab")
	r := NewResolver(appRoot, filepath.Join(base, "my-app", ".agent-mail"), filepath.Join(base, "my-app"))
	r.DiscoveryRoots = []string{base}

	targets, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "claude", Project: "infra-lib", Session: "collab"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if targets[0].Project != "infra-lib" {
		t.Errorf("want project 'infra-lib', got %q", targets[0].Project)
	}
	if targets[0].Agent != "claude" {
		t.Errorf("want agent 'claude', got %q", targets[0].Agent)
	}
}

func TestResolveCrossProject_NoSession(t *testing.T) {
	// Cross-project without explicit session: agent must be unique across sessions
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude"},
	})
	setupTestProject(t, base, "infra-lib", map[string][]string{
		"collab": {"claude", "codex"},
	})

	r := NewResolver(
		filepath.Join(base, "my-app", ".agent-mail", "collab"),
		filepath.Join(base, "my-app", ".agent-mail"),
		filepath.Join(base, "my-app"),
	)
	r.DiscoveryRoots = []string{base}

	targets, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex", Project: "infra-lib"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Agent != "codex" {
		t.Fatalf("unexpected: %+v", targets)
	}
}

func TestResolveCrossProject_AmbiguousAgent(t *testing.T) {
	// Agent exists in multiple sessions of the target project: error
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude"},
	})
	setupTestProject(t, base, "infra-lib", map[string][]string{
		"collab": {"codex"},
		"auth":   {"codex"},
	})

	r := NewResolver(
		filepath.Join(base, "my-app", ".agent-mail", "collab"),
		filepath.Join(base, "my-app", ".agent-mail"),
		filepath.Join(base, "my-app"),
	)
	r.DiscoveryRoots = []string{base}

	_, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex", Project: "infra-lib"})
	if err == nil {
		t.Fatal("expected error for ambiguous agent across sessions")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention ambiguity: %s", err)
	}
}

func TestResolveCrossProject_ProjectNotFound(t *testing.T) {
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude"},
	})

	r := NewResolver(
		filepath.Join(base, "my-app", ".agent-mail", "collab"),
		filepath.Join(base, "my-app", ".agent-mail"),
		filepath.Join(base, "my-app"),
	)
	r.DiscoveryRoots = []string{base}

	_, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "claude", Project: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent project")
	}
}

// --- Ambiguous agent@name resolution ---

func TestResolveAmbiguous_SessionWins(t *testing.T) {
	// agent@name where name matches a local session but not a project
	base := t.TempDir()
	proj := setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude"},
		"auth":   {"claude", "codex"},
	})

	r := NewResolver(
		filepath.Join(proj, ".agent-mail", "collab"),
		filepath.Join(proj, ".agent-mail"),
		proj,
	)
	r.DiscoveryRoots = []string{base}

	// codex@auth: "auth" is a local session
	targets, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex", Session: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Session != "auth" {
		t.Fatalf("want session auth, got: %+v", targets)
	}
}

func TestResolveAmbiguous_ProjectFallback(t *testing.T) {
	// agent@name where name does not match a local session but matches a project
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude"},
	})
	setupTestProject(t, base, "infra-lib", map[string][]string{
		"collab": {"claude", "codex"},
	})

	r := NewResolver(
		filepath.Join(base, "my-app", ".agent-mail", "collab"),
		filepath.Join(base, "my-app", ".agent-mail"),
		filepath.Join(base, "my-app"),
	)
	r.DiscoveryRoots = []string{base}

	// codex@infra-lib: no local session "infra-lib", but project "infra-lib" exists
	targets, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex", Session: "infra-lib"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Project != "infra-lib" {
		t.Fatalf("want project infra-lib, got: %+v", targets)
	}
}

func TestResolveAmbiguous_BothMatch_Error(t *testing.T) {
	// agent@name where name matches both a local session AND a project slug: error
	base := t.TempDir()
	proj := setupTestProject(t, base, "my-app", map[string][]string{
		"collab":    {"claude"},
		"infra-lib": {"codex"}, // session named "infra-lib"
	})
	_ = proj
	setupTestProject(t, base, "infra-lib", map[string][]string{
		"collab": {"codex"}, // project also named "infra-lib"
	})

	r := NewResolver(
		filepath.Join(base, "my-app", ".agent-mail", "collab"),
		filepath.Join(base, "my-app", ".agent-mail"),
		filepath.Join(base, "my-app"),
	)
	r.DiscoveryRoots = []string{base}

	_, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex", Session: "infra-lib"})
	if err == nil {
		t.Fatal("expected error for ambiguous agent@name matching both session and project")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("error should mention ambiguity: %s", err)
	}
	// Error should suggest disambiguation syntax
	if !strings.Contains(err.Error(), "session/") || !strings.Contains(err.Error(), "project/") {
		t.Errorf("error should suggest disambiguation syntax: %s", err)
	}
}

func TestResolveAmbiguous_NeitherMatch(t *testing.T) {
	base := t.TempDir()
	proj := setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude"},
	})

	r := NewResolver(
		filepath.Join(proj, ".agent-mail", "collab"),
		filepath.Join(proj, ".agent-mail"),
		proj,
	)
	r.DiscoveryRoots = []string{base}

	_, err := r.Resolve(Endpoint{Kind: KindAgent, Agent: "codex", Session: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent session/project")
	}
}

// --- Channel resolution ---

func TestResolveChannel_All(t *testing.T) {
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude", "codex"},
		"auth":   {"claude", "codex"},
	})

	r := NewResolver(
		filepath.Join(base, "my-app", ".agent-mail", "collab"),
		filepath.Join(base, "my-app", ".agent-mail"),
		filepath.Join(base, "my-app"),
	)
	targets, err := r.Resolve(Endpoint{Kind: KindChannel, Channel: "all"})
	if err != nil {
		t.Fatal(err)
	}
	// #all should fan out to all agents in all sessions = 4
	if len(targets) != 4 {
		t.Fatalf("want 4 targets for #all, got %d", len(targets))
	}
}

func TestResolveChannel_Session(t *testing.T) {
	base := t.TempDir()
	setupTestProject(t, base, "my-app", map[string][]string{
		"collab": {"claude", "codex"},
		"auth":   {"claude", "codex"},
	})

	r := NewResolver(
		filepath.Join(base, "my-app", ".agent-mail", "collab"),
		filepath.Join(base, "my-app", ".agent-mail"),
		filepath.Join(base, "my-app"),
	)
	targets, err := r.Resolve(Endpoint{Kind: KindChannel, Channel: "session", Session: "auth"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("want 2 targets for #session/auth, got %d", len(targets))
	}
	for _, tgt := range targets {
		if tgt.Session != "auth" {
			t.Errorf("expected session auth, got %q", tgt.Session)
		}
	}
}

func TestResolveChannel_Dedup(t *testing.T) {
	// If the same agent inbox appears via different discovery paths,
	// it should only appear once in targets.
	base := t.TempDir()
	projDir := filepath.Join(base, "my-app")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	amqrc := map[string]string{"root": ".agent-mail", "project": "my-app"}
	data, _ := json.Marshal(amqrc)
	if err := os.WriteFile(filepath.Join(projDir, ".amqrc"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Create a single agent inbox
	agentInbox := filepath.Join(projDir, ".agent-mail", "collab", "agents", "claude", "inbox")
	for _, sub := range []string{"new", "tmp", "cur"} {
		if err := os.MkdirAll(filepath.Join(agentInbox, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// Create a symlink so the same session appears under two names
	sessDir := filepath.Join(projDir, ".agent-mail", "collab")
	linkDir := filepath.Join(projDir, ".agent-mail", "alias")
	if err := os.Symlink(sessDir, linkDir); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	r := NewResolver(
		filepath.Join(projDir, ".agent-mail", "collab"),
		filepath.Join(projDir, ".agent-mail"),
		projDir,
	)

	targets, err := r.Resolve(Endpoint{Kind: KindChannel, Channel: "all"})
	if err != nil {
		t.Fatal(err)
	}
	// Should be 1 (deduped), not 2
	if len(targets) != 1 {
		t.Fatalf("want 1 deduped target, got %d: %+v", len(targets), targets)
	}
}

func TestResolveChannel_Empty(t *testing.T) {
	base := t.TempDir()
	projDir := filepath.Join(base, "empty-proj")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	amqrc := map[string]string{"root": ".agent-mail", "project": "empty-proj"}
	data, _ := json.Marshal(amqrc)
	if err := os.WriteFile(filepath.Join(projDir, ".amqrc"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Create the base root but with no sessions
	if err := os.MkdirAll(filepath.Join(projDir, ".agent-mail"), 0o700); err != nil {
		t.Fatal(err)
	}

	r := NewResolver(
		filepath.Join(projDir, ".agent-mail", "collab"),
		filepath.Join(projDir, ".agent-mail"),
		projDir,
	)

	_, err := r.Resolve(Endpoint{Kind: KindChannel, Channel: "all"})
	if err == nil {
		t.Fatal("expected error for channel with zero targets")
	}
	if !strings.Contains(err.Error(), "zero targets") {
		t.Errorf("error should mention zero targets: %s", err)
	}
}

// --- Mailbox verification ---

func TestVerifyMailbox(t *testing.T) {
	dir := t.TempDir()

	// Non-existent path
	if err := verifyMailbox(filepath.Join(dir, "nope")); err == nil {
		t.Error("expected error for non-existent path")
	}

	// File instead of directory
	filePath := filepath.Join(dir, "file")
	if err := os.WriteFile(filePath, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyMailbox(filePath); err == nil {
		t.Error("expected error for file path")
	}

	// Valid directory
	dirPath := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyMailbox(dirPath); err != nil {
		t.Errorf("valid directory should pass: %v", err)
	}
}

func TestVerifySameOwner(t *testing.T) {
	base := t.TempDir()
	dirA := filepath.Join(base, "a")
	dirB := filepath.Join(base, "b")
	if err := os.MkdirAll(dirA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o700); err != nil {
		t.Fatal(err)
	}

	// Same owner (both created by current user in test)
	if err := verifySameOwner(dirA, dirB); err != nil {
		t.Errorf("same owner should pass: %v", err)
	}

	// Non-existent path should error
	if err := verifySameOwner(dirA, filepath.Join(base, "nonexistent")); err == nil {
		t.Error("expected error for non-existent path")
	}
}

// --- Target helpers ---

func TestTarget_InboxPath(t *testing.T) {
	tgt := Target{
		Agent:       "codex",
		Session:     "collab",
		SessionRoot: "/tmp/test/.agent-mail/collab",
	}
	want := "/tmp/test/.agent-mail/collab/agents/codex/inbox"
	if got := tgt.InboxPath(); got != want {
		t.Errorf("InboxPath() = %q, want %q", got, want)
	}
}

// --- Unknown endpoint kind ---

func TestResolve_UnknownKind(t *testing.T) {
	r := NewResolver("/tmp/test", "/tmp/test", "/tmp/test")
	_, err := r.Resolve(Endpoint{Kind: "bogus"})
	if err == nil {
		t.Fatal("expected error for unknown endpoint kind")
	}
}
