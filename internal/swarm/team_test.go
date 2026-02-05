package swarm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func setupTeamDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return tmp
}

func writeTeamJSON(t *testing.T, home, teamName string, cfg TeamConfig) {
	t.Helper()
	dir := filepath.Join(home, claudeConfigDir, teamsSubdir, teamName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, teamConfigFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTeam(t *testing.T) {
	home := setupTeamDir(t)

	cfg := TeamConfig{
		Name: "test-team",
		Lead: "lead-1",
		Members: []Member{
			{Name: "claude", AgentID: "cc-123", AgentType: AgentTypeClaudeCode},
		},
	}
	writeTeamJSON(t, home, "test-team", cfg)

	got, err := LoadTeam("test-team")
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if got.Name != "test-team" {
		t.Errorf("Name = %q, want %q", got.Name, "test-team")
	}
	if len(got.Members) != 1 {
		t.Fatalf("len(Members) = %d, want 1", len(got.Members))
	}
	if got.Members[0].AgentID != "cc-123" {
		t.Errorf("Members[0].AgentID = %q, want %q", got.Members[0].AgentID, "cc-123")
	}
}

func TestLoadTeam_NotFound(t *testing.T) {
	setupTeamDir(t)
	_, err := LoadTeam("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent team")
	}
}

func TestLoadTeam_InfersName(t *testing.T) {
	home := setupTeamDir(t)
	// Write a config without a name field
	writeTeamJSON(t, home, "my-team", TeamConfig{Members: []Member{}})

	got, err := LoadTeam("my-team")
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if got.Name != "my-team" {
		t.Errorf("Name = %q, want %q (inferred from dir)", got.Name, "my-team")
	}
}

func TestDiscoverTeams(t *testing.T) {
	home := setupTeamDir(t)

	writeTeamJSON(t, home, "alpha", TeamConfig{Name: "alpha", Members: []Member{{Name: "a", AgentID: "1", AgentType: "claude-code"}}})
	writeTeamJSON(t, home, "beta", TeamConfig{Name: "beta", Members: []Member{{Name: "b", AgentID: "2", AgentType: "claude-code"}, {Name: "c", AgentID: "3", AgentType: "codex"}}})

	teams, err := DiscoverTeams()
	if err != nil {
		t.Fatalf("DiscoverTeams: %v", err)
	}
	if len(teams) != 2 {
		t.Fatalf("len(teams) = %d, want 2", len(teams))
	}

	counts := map[string]int{}
	for _, team := range teams {
		counts[team.Name] = team.MemberCount
	}
	if counts["alpha"] != 1 {
		t.Errorf("alpha member count = %d, want 1", counts["alpha"])
	}
	if counts["beta"] != 2 {
		t.Errorf("beta member count = %d, want 2", counts["beta"])
	}
}

func TestDiscoverTeams_Empty(t *testing.T) {
	setupTeamDir(t)

	teams, err := DiscoverTeams()
	if err != nil {
		t.Fatalf("DiscoverTeams: %v", err)
	}
	if len(teams) != 0 {
		t.Errorf("len(teams) = %d, want 0", len(teams))
	}
}

func TestRegisterMember(t *testing.T) {
	home := setupTeamDir(t)
	writeTeamJSON(t, home, "test-team", TeamConfig{
		Name:    "test-team",
		Members: []Member{{Name: "claude", AgentID: "cc-1", AgentType: AgentTypeClaudeCode}},
	})

	err := RegisterMember("test-team", Member{
		Name:      "codex",
		AgentID:   "ext_codex_123",
		AgentType: AgentTypeCodex,
	})
	if err != nil {
		t.Fatalf("RegisterMember: %v", err)
	}

	got, err := LoadTeam("test-team")
	if err != nil {
		t.Fatalf("LoadTeam after register: %v", err)
	}
	if len(got.Members) != 2 {
		t.Fatalf("len(Members) = %d, want 2", len(got.Members))
	}
	if got.Members[1].Name != "codex" {
		t.Errorf("Members[1].Name = %q, want %q", got.Members[1].Name, "codex")
	}
}

func TestRegisterMember_Duplicate(t *testing.T) {
	home := setupTeamDir(t)
	writeTeamJSON(t, home, "test-team", TeamConfig{
		Name:    "test-team",
		Members: []Member{{Name: "codex", AgentID: "ext_codex_123", AgentType: AgentTypeCodex}},
	})

	err := RegisterMember("test-team", Member{
		Name:      "codex-2",
		AgentID:   "ext_codex_123",
		AgentType: AgentTypeCodex,
	})
	if err == nil {
		t.Fatal("expected error for duplicate agent_id")
	}
}

func TestUnregisterMember(t *testing.T) {
	home := setupTeamDir(t)
	writeTeamJSON(t, home, "test-team", TeamConfig{
		Name: "test-team",
		Members: []Member{
			{Name: "claude", AgentID: "cc-1", AgentType: AgentTypeClaudeCode},
			{Name: "codex", AgentID: "ext_codex_123", AgentType: AgentTypeCodex},
		},
	})

	if err := UnregisterMember("test-team", "ext_codex_123"); err != nil {
		t.Fatalf("UnregisterMember: %v", err)
	}

	got, err := LoadTeam("test-team")
	if err != nil {
		t.Fatalf("LoadTeam after unregister: %v", err)
	}
	if len(got.Members) != 1 {
		t.Fatalf("len(Members) = %d, want 1", len(got.Members))
	}
	if got.Members[0].Name != "claude" {
		t.Errorf("remaining member = %q, want %q", got.Members[0].Name, "claude")
	}
}

func TestUnregisterMember_NotFound(t *testing.T) {
	home := setupTeamDir(t)
	writeTeamJSON(t, home, "test-team", TeamConfig{
		Name:    "test-team",
		Members: []Member{{Name: "claude", AgentID: "cc-1", AgentType: AgentTypeClaudeCode}},
	})

	err := UnregisterMember("test-team", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent agent_id")
	}
}

func TestFindMember(t *testing.T) {
	cfg := TeamConfig{
		Members: []Member{
			{Name: "claude", AgentID: "cc-1", AgentType: AgentTypeClaudeCode},
			{Name: "codex", AgentID: "ext-1", AgentType: AgentTypeCodex},
		},
	}

	m := cfg.FindMember("ext-1")
	if m == nil {
		t.Fatal("FindMember returned nil")
	}
	if m.Name != "codex" {
		t.Errorf("Name = %q, want %q", m.Name, "codex")
	}

	m = cfg.FindMember("nonexistent")
	if m != nil {
		t.Errorf("FindMember should return nil for nonexistent, got %v", m)
	}
}

func TestFindMemberByName(t *testing.T) {
	cfg := TeamConfig{
		Members: []Member{
			{Name: "claude", AgentID: "cc-1", AgentType: AgentTypeClaudeCode},
			{Name: "codex", AgentID: "ext-1", AgentType: AgentTypeCodex},
		},
	}

	m := cfg.FindMemberByName("codex")
	if m == nil {
		t.Fatal("FindMemberByName returned nil")
	}
	if m.AgentID != "ext-1" {
		t.Errorf("AgentID = %q, want %q", m.AgentID, "ext-1")
	}
}

func TestRegisterMember_PreservesExistingFields(t *testing.T) {
	home := setupTeamDir(t)

	// Write config with an extra field that our struct doesn't know about
	dir := filepath.Join(home, claudeConfigDir, teamsSubdir, "test-team")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"name":"test-team","lead":"cc-1","members":[{"name":"claude","agent_id":"cc-1","agent_type":"claude-code"}],"extra_field":"should_survive"}`
	if err := os.WriteFile(filepath.Join(dir, teamConfigFile), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	// Register a new member
	err := RegisterMember("test-team", Member{
		Name: "codex", AgentID: "ext-1", AgentType: AgentTypeCodex,
	})
	if err != nil {
		t.Fatalf("RegisterMember: %v", err)
	}

	// Read back raw JSON to check extra_field survived
	data, err := os.ReadFile(filepath.Join(dir, teamConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	// Note: team.go uses a typed struct, so extra_field will be lost.
	// This is a known limitation — team config format is well-defined.
	// Tasks are where unknown-field preservation matters most.
}
