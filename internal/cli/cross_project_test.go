package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseInlineRecipient(t *testing.T) {
	tests := []struct {
		input   string
		handle  string
		project string
		session string
		ok      bool
	}{
		{"codex", "codex", "", "", false},
		{"codex@infra-lib", "codex", "infra-lib", "", true},
		{"codex@infra-lib:collab", "codex", "infra-lib", "collab", true},
		{"claude@proj-a:auth", "claude", "proj-a", "auth", true},
		{"agent@", "agent@", "", "", false}, // empty qualifier
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			handle, project, session, ok := parseInlineRecipient(tt.input)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if handle != tt.handle {
				t.Errorf("handle = %q, want %q", handle, tt.handle)
			}
			if project != tt.project {
				t.Errorf("project = %q, want %q", project, tt.project)
			}
			if session != tt.session {
				t.Errorf("session = %q, want %q", session, tt.session)
			}
		})
	}
}

func TestResolvePeer(t *testing.T) {
	// Create a temp project with .amqrc containing peers.
	projectDir := t.TempDir()
	peerRoot := filepath.Join(t.TempDir(), ".agent-mail")
	if err := os.MkdirAll(peerRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	rc := map[string]any{
		"root":    ".agent-mail",
		"project": "proj-a",
		"peers": map[string]string{
			"proj-b": peerRoot,
		},
	}
	rcData, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}

	// chdir to project so findAndLoadAmqrc finds it.
	oldDir, _ := os.Getwd()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldDir) }()
	resetAmqrcCache()
	defer resetAmqrcCache()

	// Resolve peer.
	resolved, err := resolvePeer("", "proj-b")
	if err != nil {
		t.Fatalf("resolvePeer: %v", err)
	}
	if resolved != peerRoot {
		t.Errorf("resolved = %q, want %q", resolved, peerRoot)
	}

	// Unknown peer.
	_, err = resolvePeer("", "nonexistent")
	if err == nil {
		t.Error("expected error for unknown peer")
	}
}

func TestResolveProject(t *testing.T) {
	// Explicit project name in .amqrc.
	projectDir := t.TempDir()
	rc := map[string]any{
		"root":    ".agent-mail",
		"project": "my-custom-name",
	}
	rcData, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}

	oldDir, _ := os.Getwd()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldDir) }()
	resetAmqrcCache()
	defer resetAmqrcCache()

	name := resolveProject("")
	if name != "my-custom-name" {
		t.Errorf("resolveProject = %q, want %q", name, "my-custom-name")
	}
}

func TestResolvePeerFromRoot(t *testing.T) {
	// Fix 3: resolvePeer should work when cwd is outside project tree,
	// by falling back to searching from the root path.
	projectDir := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	peerRoot := filepath.Join(t.TempDir(), "peer-project", ".agent-mail")
	if err := os.MkdirAll(peerRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	rc := map[string]any{
		"root":    ".agent-mail",
		"project": "my-project",
		"peers": map[string]string{
			"peer-project": peerRoot,
		},
	}
	rcData, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}

	// chdir to some unrelated directory (simulate running from outside project).
	outsideDir := t.TempDir()
	oldDir, _ := os.Getwd()
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldDir) }()
	resetAmqrcCache()
	defer resetAmqrcCache()

	// Pass root that is under the project directory.
	root := filepath.Join(projectDir, ".agent-mail")
	resolved, err := resolvePeer(root, "peer-project")
	if err != nil {
		t.Fatalf("resolvePeer from root: %v", err)
	}
	if resolved != peerRoot {
		t.Errorf("resolved = %q, want %q", resolved, peerRoot)
	}
}

func TestFindAmqrcForRootSessionDetection(t *testing.T) {
	// Verify that findAmqrcForRoot can locate .amqrc from a session root
	// when cwd is outside the project tree. This is the key fix for the
	// single-session cross-project edge case.
	projectDir := filepath.Join(t.TempDir(), "my-project")
	sessionRoot := filepath.Join(projectDir, ".agent-mail", "collab")
	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	rc := map[string]any{
		"root":    ".agent-mail",
		"project": "my-project",
	}
	rcData, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}

	// chdir outside the project tree.
	outsideDir := t.TempDir()
	oldDir, _ := os.Getwd()
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldDir) }()
	resetAmqrcCache()
	defer resetAmqrcCache()

	// findAmqrcForRoot should find .amqrc by walking up from the session root.
	result, err := findAmqrcForRoot(sessionRoot)
	if err != nil {
		t.Fatalf("findAmqrcForRoot: %v", err)
	}
	if result.Config.Root != ".agent-mail" {
		t.Errorf("root = %q, want %q", result.Config.Root, ".agent-mail")
	}
	if result.Config.Project != "my-project" {
		t.Errorf("project = %q, want %q", result.Config.Project, "my-project")
	}

	// Verify session detection: absRoot should be under absBase.
	absBase := filepath.Join(result.Dir, result.Config.Root)
	absRoot, _ := filepath.Abs(sessionRoot)
	absBaseAbs, _ := filepath.Abs(absBase)
	if absRoot == absBaseAbs {
		t.Error("session root should not equal base root")
	}
	if !strings.HasPrefix(absRoot, absBaseAbs+string(filepath.Separator)) {
		t.Errorf("session root %q should be under base %q", absRoot, absBaseAbs)
	}
}

func TestResolveProjectFallbackToBasename(t *testing.T) {
	// No project field in .amqrc — should fall back to directory basename.
	projectDir := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rc := map[string]any{
		"root": ".agent-mail",
	}
	rcData, _ := json.Marshal(rc)
	if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatal(err)
	}

	oldDir, _ := os.Getwd()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldDir) }()
	resetAmqrcCache()
	defer resetAmqrcCache()

	name := resolveProject("")
	if name != "my-project" {
		t.Errorf("resolveProject = %q, want %q", name, "my-project")
	}
}
