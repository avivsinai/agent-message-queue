package swarm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
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

func TestRegisterMember_PreservesExistingFields(t *testing.T) {
	home := setupTeamDir(t)

	// Write config with extra fields that our struct doesn't know about
	dir := filepath.Join(home, claudeConfigDir, teamsSubdir, "test-team")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"name":"test-team","lead":"cc-1","members":[{"name":"claude","agent_id":"cc-1","agent_type":"claude-code","extra_member_field":"member_value"}],"extra_field":"should_survive","settings":{"timeout":30}}`
	if err := os.WriteFile(filepath.Join(dir, teamConfigFile), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	// Register a new member (triggers read-modify-write)
	err := RegisterMember("test-team", Member{
		Name: "codex", AgentID: "ext-1", AgentType: AgentTypeCodex,
	})
	if err != nil {
		t.Fatalf("RegisterMember: %v", err)
	}

	// Read back raw JSON and verify unknown fields survived
	data, err := os.ReadFile(filepath.Join(dir, teamConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}

	// Top-level unknown fields preserved
	if m["extra_field"] != "should_survive" {
		t.Errorf("extra_field = %v, want %q", m["extra_field"], "should_survive")
	}
	settings, ok := m["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings not preserved, got %T: %v", m["settings"], m["settings"])
	}
	if settings["timeout"] != float64(30) {
		t.Errorf("settings.timeout = %v, want 30", settings["timeout"])
	}

	// Existing member's unknown fields preserved
	members, ok := m["members"].([]any)
	if !ok || len(members) < 1 {
		t.Fatalf("members not preserved")
	}
	firstMember, ok := members[0].(map[string]any)
	if !ok {
		t.Fatalf("first member not a map")
	}
	if firstMember["extra_member_field"] != "member_value" {
		t.Errorf("extra_member_field = %v, want %q", firstMember["extra_member_field"], "member_value")
	}

	// New member was added
	if len(members) != 2 {
		t.Fatalf("len(members) = %d, want 2", len(members))
	}
	secondMember, ok := members[1].(map[string]any)
	if !ok {
		t.Fatalf("second member not a map")
	}
	if secondMember["name"] != "codex" {
		t.Errorf("second member name = %v, want %q", secondMember["name"], "codex")
	}
}

func TestRegisterMember_Concurrent(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("concurrency locking uses flock (darwin/linux)")
	}

	home := setupTeamDir(t)
	writeTeamJSON(t, home, "test-team", TeamConfig{
		Name:    "test-team",
		Members: []Member{{Name: "claude", AgentID: "cc-1", AgentType: AgentTypeClaudeCode}},
	})

	const n = 25
	start := make(chan struct{})
	errCh := make(chan error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			if err := RegisterMember("test-team", Member{
				Name:      fmt.Sprintf("m%d", i),
				AgentID:   fmt.Sprintf("ext_m%d", i),
				AgentType: AgentTypeExternal,
			}); err != nil {
				errCh <- fmt.Errorf("RegisterMember(%d): %w", i, err)
			}
		}()
	}
	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for concurrent RegisterMember calls")
	}
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	got, err := LoadTeam("test-team")
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if len(got.Members) != n+1 {
		t.Fatalf("len(Members) = %d, want %d", len(got.Members), n+1)
	}
	ids := make(map[string]bool, len(got.Members))
	for _, m := range got.Members {
		ids[m.AgentID] = true
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("ext_m%d", i)
		if !ids[id] {
			t.Errorf("missing agent_id %q after concurrent registration", id)
		}
	}
}

func TestFindMemberLookups(t *testing.T) {
	cfg := TeamConfig{
		Members: []Member{
			{Name: "claude", AgentID: "cc-1", AgentType: AgentTypeClaudeCode},
			{Name: "codex", AgentID: "ext-1", AgentType: AgentTypeCodex},
		},
	}

	m := cfg.FindMember("ext-1")
	if m == nil || m.Name != "codex" {
		t.Fatalf("FindMember(ext-1) = %#v, want codex", m)
	}
	if cfg.FindMember("nonexistent") != nil {
		t.Fatal("FindMember should return nil for nonexistent")
	}

	m = cfg.FindMemberByName("codex")
	if m == nil || m.AgentID != "ext-1" {
		t.Fatalf("FindMemberByName(codex) = %#v, want ext-1", m)
	}
	if cfg.FindMemberByName("nonexistent") != nil {
		t.Fatal("FindMemberByName should return nil for nonexistent")
	}
}
