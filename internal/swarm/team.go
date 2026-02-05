package swarm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentType constants for team member classification.
const (
	AgentTypeClaudeCode = "claude-code"
	AgentTypeCodex      = "codex"
	AgentTypeExternal   = "external"
)

// Member represents a teammate in a Claude Code Agent Team.
type Member struct {
	Name      string `json:"name"`
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
}

// TeamConfig represents the team configuration stored at
// ~/.claude/teams/{team-name}/config.json.
type TeamConfig struct {
	Name    string   `json:"name"`
	Lead    string   `json:"lead,omitempty"`
	Members []Member `json:"members"`
}

// TeamSummary provides a brief overview of a team for listing.
type TeamSummary struct {
	Name        string `json:"name"`
	MemberCount int    `json:"member_count"`
	ConfigPath  string `json:"config_path"`
}

// DiscoverTeams lists all teams found in ~/.claude/teams/.
func DiscoverTeams() ([]TeamSummary, error) {
	teamsDir := ClaudeTeamsDir()
	entries, err := os.ReadDir(teamsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read teams directory: %w", err)
	}

	var teams []TeamSummary
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		configPath := TeamConfigPath(name)
		cfg, err := LoadTeam(name)
		if err != nil {
			// Include team even if config is unreadable
			teams = append(teams, TeamSummary{
				Name:       name,
				ConfigPath: configPath,
			})
			continue
		}
		teams = append(teams, TeamSummary{
			Name:        name,
			MemberCount: len(cfg.Members),
			ConfigPath:  configPath,
		})
	}
	return teams, nil
}

// LoadTeam reads and parses a team's config.json.
func LoadTeam(name string) (TeamConfig, error) {
	configPath := TeamConfigPath(name)
	data, err := os.ReadFile(configPath)
	if err != nil {
		return TeamConfig{}, fmt.Errorf("read team config: %w", err)
	}
	var cfg TeamConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return TeamConfig{}, fmt.Errorf("parse team config: %w", err)
	}
	if cfg.Name == "" {
		cfg.Name = name
	}
	return cfg, nil
}

// FindMember returns the member with the given agent ID, or nil if not found.
func (tc *TeamConfig) FindMember(agentID string) *Member {
	for i := range tc.Members {
		if tc.Members[i].AgentID == agentID {
			return &tc.Members[i]
		}
	}
	return nil
}

// FindMemberByName returns the member with the given name, or nil if not found.
func (tc *TeamConfig) FindMemberByName(name string) *Member {
	for i := range tc.Members {
		if tc.Members[i].Name == name {
			return &tc.Members[i]
		}
	}
	return nil
}

// RegisterMember adds a new member to the team config and writes it back.
// Returns an error if a member with the same agent_id already exists.
func RegisterMember(teamName string, member Member) error {
	cfg, err := LoadTeam(teamName)
	if err != nil {
		return err
	}

	// Check for duplicate
	if existing := cfg.FindMember(member.AgentID); existing != nil {
		return fmt.Errorf("member with agent_id %q already registered (name=%q)", member.AgentID, existing.Name)
	}

	cfg.Members = append(cfg.Members, member)
	return writeTeamConfig(teamName, cfg)
}

// UnregisterMember removes a member by agent_id from the team config.
func UnregisterMember(teamName, agentID string) error {
	cfg, err := LoadTeam(teamName)
	if err != nil {
		return err
	}

	found := false
	members := make([]Member, 0, len(cfg.Members))
	for _, m := range cfg.Members {
		if m.AgentID == agentID {
			found = true
			continue
		}
		members = append(members, m)
	}
	if !found {
		return fmt.Errorf("member with agent_id %q not found in team %q", agentID, teamName)
	}

	cfg.Members = members
	return writeTeamConfig(teamName, cfg)
}

// NewExternalAgentID generates an agent ID for external (non-Claude-Code) agents.
// Format: ext_{handle}_{timestamp}
func NewExternalAgentID(handle string) string {
	ts := time.Now().UTC().Format("20060102T150405")
	return fmt.Sprintf("ext_%s_%s", handle, ts)
}

func writeTeamConfig(teamName string, cfg TeamConfig) error {
	configPath := TeamConfigPath(teamName)
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create team directory: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal team config: %w", err)
	}
	data = append(data, '\n')

	// Write atomically via temp file
	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write team config: %w", err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename team config: %w", err)
	}
	return nil
}
