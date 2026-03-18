package metadata

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionMetadata_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	s := SessionMeta{
		Schema:  1,
		Session: "auth",
		Topic:   "Auth rewrite",
		Branch:  "feat/auth-v2",
		Claims:  []string{"internal/auth/**"},
		Updated: time.Now().UTC().Truncate(time.Second),
	}
	if err := WriteSessionMeta(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Session != "auth" || loaded.Topic != "Auth rewrite" || len(loaded.Claims) != 1 {
		t.Fatalf("unexpected: %+v", loaded)
	}
}

func TestAgentMeta_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")

	a := AgentMeta{
		Schema:   1,
		Agent:    "claude",
		LastSeen: time.Now().UTC().Truncate(time.Second),
		Channels: []string{"events", "triage"},
	}
	if err := WriteAgentMeta(path, a); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadAgentMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agent != "claude" || len(loaded.Channels) != 2 {
		t.Fatalf("unexpected: %+v", loaded)
	}
}

func TestAgentMeta_TouchLastSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")

	a := AgentMeta{Schema: 1, Agent: "claude", LastSeen: time.Now().Add(-time.Hour).UTC()}
	if err := WriteAgentMeta(path, a); err != nil {
		t.Fatal(err)
	}

	if err := TouchLastSeen(path); err != nil {
		t.Fatal(err)
	}
	loaded, _ := ReadAgentMeta(path)
	if time.Since(loaded.LastSeen) > 5*time.Second {
		t.Fatalf("last_seen not updated: %v", loaded.LastSeen)
	}
}

func TestAgentMeta_IsActive(t *testing.T) {
	recent := AgentMeta{LastSeen: time.Now().UTC()}
	if !recent.IsActive(10 * time.Minute) {
		t.Fatal("recent agent should be active")
	}
	stale := AgentMeta{LastSeen: time.Now().Add(-time.Hour).UTC()}
	if stale.IsActive(10 * time.Minute) {
		t.Fatal("stale agent should not be active")
	}
}

func TestReadSessionMeta_Missing(t *testing.T) {
	_, err := ReadSessionMeta("/nonexistent/session.json")
	if !os.IsNotExist(err) {
		t.Fatalf("expected not-exist error, got %v", err)
	}
}
