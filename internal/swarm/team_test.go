package swarm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// claudeNativeTeamConfig has the key set of a Claude Code 2.1.311 team config
// with invented values.
const claudeNativeTeamConfig = `{"name":"test-team","description":"demo","createdAt":1761580800000,` +
	`"leadAgentId":"lead-1","leadSessionId":"00000000-0000-4000-8000-000000000001",` +
	`"members":[{"agentId":"cc-123","agentType":"team-lead","name":"claude",` +
	`"cwd":"/work/demo","joinedAt":1761580800001,"model":"demo-model","backendType":"in-process",` +
	`"tmuxPaneId":"%7","subscriptions":["demo-topic"]}]}`

func writeRawTeamConfig(t *testing.T, home, teamName, raw string) string {
	t.Helper()
	dir := filepath.Join(home, claudeConfigDir, teamsSubdir, teamName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, teamConfigFile)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadTeam(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantType string
	}{
		// field: Claude 2.1.311 team configs use camelCase (2026-10-27)
		{name: "claude native camelCase", raw: claudeNativeTeamConfig, wantType: "team-lead"},
		{
			name:     "legacy AMQ snake_case",
			raw:      `{"name":"test-team","lead":"lead-1","members":[{"name":"claude","agent_id":"cc-123","agent_type":"claude-code"}]}`,
			wantType: AgentTypeClaudeCode,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := setupTeamDir(t)
			writeRawTeamConfig(t, home, "test-team", tt.raw)

			got, err := LoadTeam("test-team")
			if err != nil {
				t.Fatalf("LoadTeam: %v", err)
			}
			if got.Name != "test-team" || got.Lead != "lead-1" {
				t.Errorf("Name, Lead = %q, %q, want test-team, lead-1", got.Name, got.Lead)
			}
			if len(got.Members) != 1 {
				t.Fatalf("len(Members) = %d, want 1", len(got.Members))
			}
			if got.Members[0].AgentID != "cc-123" || got.Members[0].AgentType != tt.wantType {
				t.Errorf("Members[0] = %+v, want AgentID cc-123, AgentType %q", got.Members[0], tt.wantType)
			}
		})
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

// field: Claude 2.1.311 team configs use camelCase (2026-10-27). Without a
// readable agentId, claim/complete/fail/block and bridge cannot resolve the
// agent's id by name, join cannot see duplicates, and leave cannot find a
// native member; join must also keep Claude's fields and write camelCase.
func TestClaudeNativeTeamJoinLeave(t *testing.T) {
	home := setupTeamDir(t)
	path := writeRawTeamConfig(t, home, "test-team", claudeNativeTeamConfig)
	var original map[string]any
	if err := json.Unmarshal([]byte(claudeNativeTeamConfig), &original); err != nil {
		t.Fatal(err)
	}
	readRaw := func() map[string]any {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	if err := RegisterMember("test-team", Member{Name: "claude", AgentID: "cc-123"}); err == nil {
		t.Error("join accepted a duplicate of a native agentId")
	}
	if err := RegisterMember("test-team", Member{Name: "codex", AgentID: "ext-1", AgentType: AgentTypeCodex}); err != nil {
		t.Fatalf("RegisterMember: %v", err)
	}
	joined := readRaw()
	members, _ := joined["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("len(members) = %d, want 2", len(members))
	}
	for k, v := range original {
		if k != "members" && !reflect.DeepEqual(joined[k], v) {
			t.Errorf("join changed %q: got %v, want %v", k, joined[k], v)
		}
	}
	origMembers, _ := original["members"].([]any)
	if !reflect.DeepEqual(members[0], origMembers[0]) {
		t.Errorf("join changed the native member: got %v, want %v", members[0], origMembers[0])
	}
	wantAdded := map[string]any{"name": "codex", "agentId": "ext-1", "agentType": AgentTypeCodex}
	if !reflect.DeepEqual(members[1], wantAdded) {
		t.Errorf("joined member = %v, want %v", members[1], wantAdded)
	}

	if err := UnregisterMember("test-team", "ext-1"); err != nil {
		t.Fatalf("UnregisterMember: %v", err)
	}
	if left := readRaw(); !reflect.DeepEqual(left, original) {
		t.Errorf("leave did not restore the native config:\ngot  %v\nwant %v", left, original)
	}
}

// Review finding on d37580ea: a non-string agentId was ignored, so the legacy
// agent_id became the identity and leave deleted the entry.
func TestUnregisterMember_RefusesNonStringIdentity(t *testing.T) {
	home := setupTeamDir(t)
	raw := `{"name":"test-team","members":[{"name":"odd","agentId":123,"agent_id":"ext-1"}]}`
	path := writeRawTeamConfig(t, home, "test-team", raw)

	if err := UnregisterMember("test-team", "ext-1"); err == nil {
		t.Error("UnregisterMember accepted a non-string agentId")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != raw {
		t.Errorf("config changed after refusal: %s", data)
	}
}
