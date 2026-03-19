package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
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
