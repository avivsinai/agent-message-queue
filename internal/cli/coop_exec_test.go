//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/metadata"
)

func TestSplitDashDash(t *testing.T) {
	tests := []struct {
		name       string
		input      []string
		wantBefore []string
		wantAfter  []string
	}{
		{
			name:       "no separator",
			input:      []string{"claude"},
			wantBefore: []string{"claude"},
			wantAfter:  nil,
		},
		{
			name:       "separator with args",
			input:      []string{"--root", "/tmp/q", "codex", "--", "--some-flag", "--other"},
			wantBefore: []string{"--root", "/tmp/q", "codex"},
			wantAfter:  []string{"--some-flag", "--other"},
		},
		{
			name:       "separator at start",
			input:      []string{"--", "claude", "-v"},
			wantBefore: []string{},
			wantAfter:  []string{"claude", "-v"},
		},
		{
			name:       "separator at end",
			input:      []string{"claude", "--"},
			wantBefore: []string{"claude"},
			wantAfter:  []string{},
		},
		{
			name:       "empty input",
			input:      []string{},
			wantBefore: []string{},
			wantAfter:  nil,
		},
		{
			name:       "multiple separators",
			input:      []string{"a", "--", "b", "--", "c"},
			wantBefore: []string{"a"},
			wantAfter:  []string{"b", "--", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := splitDashDash(tt.input)
			if !sliceEq(before, tt.wantBefore) {
				t.Errorf("before = %v, want %v", before, tt.wantBefore)
			}
			if !sliceEq(after, tt.wantAfter) {
				t.Errorf("after = %v, want %v", after, tt.wantAfter)
			}
		})
	}
}

func TestSetEnvVar(t *testing.T) {
	t.Run("append new", func(t *testing.T) {
		env := []string{"PATH=/bin", "HOME=/home"}
		got := setEnvVar(env, "AM_ROOT", "/tmp/q")
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[2] != "AM_ROOT=/tmp/q" {
			t.Fatalf("got[2] = %q, want %q", got[2], "AM_ROOT=/tmp/q")
		}
	})

	t.Run("replace existing", func(t *testing.T) {
		env := []string{"PATH=/bin", "AM_ROOT=/old", "HOME=/home"}
		got := setEnvVar(env, "AM_ROOT", "/new")
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[1] != "AM_ROOT=/new" {
			t.Fatalf("got[1] = %q, want %q", got[1], "AM_ROOT=/new")
		}
	})
}

func TestCoopExecUsageError(t *testing.T) {
	err := runCoopExec([]string{})
	if err == nil {
		t.Fatal("expected error for empty args")
	}
	exitErr, ok := err.(*ExitCodeError)
	if !ok {
		t.Fatalf("expected *ExitCodeError, got %T: %v", err, err)
	}
	if exitErr.Code != ExitUsage {
		t.Fatalf("expected ExitUsage (%d), got %d", ExitUsage, exitErr.Code)
	}
	if !containsStr(err.Error(), "command required") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCoopExecSessionRootMutuallyExclusive(t *testing.T) {
	err := runCoopExec([]string{"--session", "feat", "--root", "/tmp/q", "claude"})
	if err == nil {
		t.Fatal("expected error for --session + --root")
	}
	if !containsStr(err.Error(), "mutually exclusive") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCoopExecSessionInvalidName(t *testing.T) {
	err := runCoopExec([]string{"--session", "Bad/Name", "claude"})
	if err == nil {
		t.Fatal("expected error for invalid session name")
	}
	if !containsStr(err.Error(), "invalid session name") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func sliceEq(a, b []string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && searchStr(s, sub)
}

func searchStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestGenerateUUID(t *testing.T) {
	id, err := generateUUID()
	if err != nil {
		t.Fatal(err)
	}
	// UUID v4 format: xxxxxxxx-xxxx-4xxx-[89ab]xxx-xxxxxxxxxxxx
	if len(id) != 36 {
		t.Fatalf("UUID length = %d, want 36: %q", len(id), id)
	}
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("UUID parts = %d, want 5: %q", len(parts), id)
	}
	// Check version nibble (should be '4')
	if parts[2][0] != '4' {
		t.Fatalf("UUID version nibble = %c, want '4': %q", parts[2][0], id)
	}
	// Check variant nibble (should be 8, 9, a, or b)
	v := parts[3][0]
	if v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Fatalf("UUID variant nibble = %c, want [89ab]: %q", v, id)
	}

	// Ensure uniqueness
	id2, err := generateUUID()
	if err != nil {
		t.Fatal(err)
	}
	if id == id2 {
		t.Fatal("two generated UUIDs should not be equal")
	}
}

func TestDetectGitBranch(t *testing.T) {
	// We're running inside a git repo, so this should return something.
	branch := detectGitBranch()
	if branch == "" {
		t.Skip("not in a git repo")
	}
	// Branch name should not contain newlines.
	if strings.Contains(branch, "\n") {
		t.Fatalf("branch contains newline: %q", branch)
	}
}

func TestWriteSessionMetadata(t *testing.T) {
	dir := t.TempDir()
	// Create the session root so session.json can be written.
	if err := fsq.EnsureRootDirs(dir); err != nil {
		t.Fatal(err)
	}

	writeSessionMetadata(dir, "auth", "Auth rewrite", "auth,security")

	path := fsq.SessionJSON(dir)
	sm, err := metadata.ReadSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if sm.Session != "auth" {
		t.Fatalf("session = %q, want %q", sm.Session, "auth")
	}
	if sm.Topic != "Auth rewrite" {
		t.Fatalf("topic = %q, want %q", sm.Topic, "Auth rewrite")
	}
	if len(sm.Claims) != 2 || sm.Claims[0] != "auth" || sm.Claims[1] != "security" {
		t.Fatalf("claims = %v, want [auth security]", sm.Claims)
	}
	if sm.Schema != 1 {
		t.Fatalf("schema = %d, want 1", sm.Schema)
	}
	// Branch should be non-empty since we're in a git repo.
	if sm.Branch == "" {
		t.Log("warning: branch is empty (might not be in a git repo)")
	}
}

func TestWriteSessionMetadata_EmptyFlags(t *testing.T) {
	dir := t.TempDir()
	if err := fsq.EnsureRootDirs(dir); err != nil {
		t.Fatal(err)
	}

	writeSessionMetadata(dir, "collab", "", "")

	sm, err := metadata.ReadSessionMeta(fsq.SessionJSON(dir))
	if err != nil {
		t.Fatal(err)
	}
	if sm.Session != "collab" {
		t.Fatalf("session = %q, want %q", sm.Session, "collab")
	}
	if sm.Topic != "" {
		t.Fatalf("topic = %q, want empty", sm.Topic)
	}
	if len(sm.Claims) != 0 {
		t.Fatalf("claims = %v, want empty", sm.Claims)
	}
}

func TestWriteAgentMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := fsq.EnsureAgentDirs(dir, "claude"); err != nil {
		t.Fatal(err)
	}

	writeAgentMetadata(dir, "claude", "ops,alerts")

	path := fsq.AgentJSON(dir, "claude")
	am, err := metadata.ReadAgentMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if am.Agent != "claude" {
		t.Fatalf("agent = %q, want %q", am.Agent, "claude")
	}
	if len(am.Channels) != 2 || am.Channels[0] != "ops" || am.Channels[1] != "alerts" {
		t.Fatalf("channels = %v, want [ops alerts]", am.Channels)
	}
	if am.Schema != 1 {
		t.Fatalf("schema = %d, want 1", am.Schema)
	}
}

func TestWriteAgentMetadata_NoChannels(t *testing.T) {
	dir := t.TempDir()
	if err := fsq.EnsureAgentDirs(dir, "codex"); err != nil {
		t.Fatal(err)
	}

	writeAgentMetadata(dir, "codex", "")

	am, err := metadata.ReadAgentMeta(fsq.AgentJSON(dir, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if am.Agent != "codex" {
		t.Fatalf("agent = %q, want %q", am.Agent, "codex")
	}
	if len(am.Channels) != 0 {
		t.Fatalf("channels = %v, want empty", am.Channels)
	}
}

func TestEnsureProjectID_GeneratesWhenMissing(t *testing.T) {
	dir := t.TempDir()

	// Write a .amqrc without project_id.
	rc := amqrc{Root: ".agent-mail"}
	data, _ := json.MarshalIndent(rc, "", "  ")
	rcPath := filepath.Join(dir, ".amqrc")
	if err := os.WriteFile(rcPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded := &amqrcResult{Config: rc, Dir: dir}
	ensureProjectID(loaded, dir)

	// Check that project_id was generated.
	if loaded.Config.ProjectID == "" {
		t.Fatal("project_id should have been generated")
	}

	// Check that the file was updated.
	fileData, err := os.ReadFile(rcPath)
	if err != nil {
		t.Fatal(err)
	}
	var updated amqrc
	if err := json.Unmarshal(fileData, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.ProjectID == "" {
		t.Fatal("project_id should be in the file")
	}
	if updated.ProjectID != loaded.Config.ProjectID {
		t.Fatalf("file project_id = %q, want %q", updated.ProjectID, loaded.Config.ProjectID)
	}
	// Root should be preserved.
	if updated.Root != ".agent-mail" {
		t.Fatalf("root = %q, want %q", updated.Root, ".agent-mail")
	}
}

func TestEnsureProjectID_SkipsWhenPresent(t *testing.T) {
	dir := t.TempDir()

	// Write a .amqrc with project_id.
	rc := amqrc{Root: ".agent-mail", ProjectID: "existing-id"}
	data, _ := json.MarshalIndent(rc, "", "  ")
	rcPath := filepath.Join(dir, ".amqrc")
	if err := os.WriteFile(rcPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded := &amqrcResult{Config: rc, Dir: dir}
	ensureProjectID(loaded, dir)

	// Check that it wasn't overwritten.
	if loaded.Config.ProjectID != "existing-id" {
		t.Fatalf("project_id = %q, want %q", loaded.Config.ProjectID, "existing-id")
	}
}

func TestEnsureProjectID_NilLoaded(t *testing.T) {
	// Should not panic.
	ensureProjectID(nil, "")
}

func TestCoopExecNewFlagsParse(t *testing.T) {
	// Verify that the new flags are recognized without error
	// by passing them alongside the mutually-exclusive check
	// (which triggers before LookPath).
	err := runCoopExec([]string{
		"--session", "feat", "--root", "/tmp/q",
		"--topic", "My Topic", "--claim", "a,b",
		"--channel", "ops,alerts",
		"claude",
	})
	if err == nil {
		t.Fatal("expected error for --session + --root")
	}
	// The error should be about mutually exclusive, not unknown flag.
	if !containsStr(err.Error(), "mutually exclusive") {
		t.Fatalf("unexpected error (should be mutually exclusive, not unknown flag): %v", err)
	}
}
