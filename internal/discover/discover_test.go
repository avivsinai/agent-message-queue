// internal/discover/discover_test.go
package discover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func setupProject(t *testing.T, base, name, root string, sessions []string) string {
	t.Helper()
	projDir := filepath.Join(base, name)
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	amqrc := map[string]string{"root": root}
	data, _ := json.Marshal(amqrc)
	if err := os.WriteFile(filepath.Join(projDir, ".amqrc"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	amqRoot := filepath.Join(projDir, root)
	for _, sess := range sessions {
		agentDir := filepath.Join(amqRoot, sess, "agents", "claude", "inbox", "new")
		if err := os.MkdirAll(agentDir, 0o700); err != nil {
			t.Fatal(err)
		}
		agentDir2 := filepath.Join(amqRoot, sess, "agents", "codex", "inbox", "new")
		if err := os.MkdirAll(agentDir2, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return projDir
}

func TestDiscoverCurrentProject(t *testing.T) {
	base := t.TempDir()
	projDir := setupProject(t, base, "my-app", ".agent-mail", []string{"collab", "auth"})

	proj, err := DiscoverProject(projDir)
	if err != nil {
		t.Fatal(err)
	}
	if proj.Slug != "my-app" {
		t.Errorf("slug = %q, want my-app", proj.Slug)
	}
	if len(proj.Sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(proj.Sessions))
	}
}

func TestDiscoverSessions(t *testing.T) {
	base := t.TempDir()
	projDir := setupProject(t, base, "my-app", ".agent-mail", []string{"collab", "auth", "api"})

	proj, _ := DiscoverProject(projDir)
	sessions := proj.Sessions
	if len(sessions) != 3 {
		t.Fatalf("sessions = %d, want 3", len(sessions))
	}
	names := make(map[string]bool)
	for _, s := range sessions {
		names[s.Name] = true
	}
	for _, want := range []string{"collab", "auth", "api"} {
		if !names[want] {
			t.Errorf("missing session %q", want)
		}
	}
}

func TestDiscoverAgentsInSession(t *testing.T) {
	base := t.TempDir()
	projDir := setupProject(t, base, "my-app", ".agent-mail", []string{"collab"})

	proj, _ := DiscoverProject(projDir)
	agents := proj.Sessions[0].Agents
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(agents))
	}
}

func TestScanProjects(t *testing.T) {
	base := t.TempDir()
	setupProject(t, base, "app-a", ".agent-mail", []string{"collab"})
	setupProject(t, base, "app-b", ".agent-mail", []string{"collab"})
	setupProject(t, base, "no-amq", ".agent-mail", nil) // has .amqrc but no sessions

	projects, err := ScanProjects([]string{base}, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Should find app-a and app-b (no-amq has no sessions but still valid)
	if len(projects) < 2 {
		t.Errorf("found %d projects, want >= 2", len(projects))
	}
}

func TestDiscoverProject_NoAmqrc(t *testing.T) {
	dir := t.TempDir()
	_, err := DiscoverProject(dir)
	if err == nil {
		t.Fatal("expected error for directory without .amqrc")
	}
}
