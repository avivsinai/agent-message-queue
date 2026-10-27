package swarm

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// AgentType constants for team member classification.
const (
	AgentTypeClaudeCode = "claude-code"
	AgentTypeCodex      = "codex"
	AgentTypeExternal   = "external"
)

// Member represents a teammate in a Claude Code Agent Team. Claude Code
// writes camelCase keys; configs written by older AMQ releases use snake_case,
// which UnmarshalJSON still reads.
type Member struct {
	Name      string `json:"name"`
	AgentID   string `json:"agentId"`
	AgentType string `json:"agentType"`
}

// UnmarshalJSON reads the camelCase keys first and falls back to the legacy
// snake_case keys.
func (m *Member) UnmarshalJSON(data []byte) error {
	var aux struct {
		Name            string `json:"name"`
		AgentType       string `json:"agentType"`
		LegacyAgentType string `json:"agent_type"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	id, err := memberAgentID(raw)
	if err != nil {
		return err
	}
	*m = Member{
		Name:      aux.Name,
		AgentID:   id,
		AgentType: firstNonEmpty(aux.AgentType, aux.LegacyAgentType),
	}
	return nil
}

// memberAgentID is the single identity parser for typed loads and raw
// rewrites. It reads the camelCase agentId and falls back to the legacy
// agent_id. A present key whose value is not a string, or an entry whose two
// keys disagree, has no trustworthy identity and is refused rather than
// guessed.
func memberAgentID(m map[string]any) (string, error) {
	ids := make([]string, 2)
	for i, key := range []string{"agentId", "agent_id"} {
		v, present := m[key]
		if !present {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("team member %s is %T, not a string", key, v)
		}
		ids[i] = s
	}
	agentID, legacyAgentID := ids[0], ids[1]
	if agentID != "" && legacyAgentID != "" && agentID != legacyAgentID {
		return "", fmt.Errorf("team member has conflicting agentId %q and agent_id %q", agentID, legacyAgentID)
	}
	return firstNonEmpty(agentID, legacyAgentID), nil
}

// TeamConfig represents the team configuration stored at
// ~/.claude/teams/{team-name}/config.json. Lead is Claude Code's leadAgentId;
// legacy AMQ-written configs store it as lead.
type TeamConfig struct {
	Name    string   `json:"name"`
	Lead    string   `json:"leadAgentId,omitempty"`
	Members []Member `json:"members"`
}

// UnmarshalJSON reads leadAgentId first and falls back to the legacy lead key.
func (tc *TeamConfig) UnmarshalJSON(data []byte) error {
	var aux struct {
		Name       string   `json:"name"`
		Lead       string   `json:"leadAgentId"`
		LegacyLead string   `json:"lead"`
		Members    []Member `json:"members"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*tc = TeamConfig{
		Name:    aux.Name,
		Lead:    firstNonEmpty(aux.Lead, aux.LegacyLead),
		Members: aux.Members,
	}
	return nil
}

func firstNonEmpty(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
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
// Returns an error if a member with the same agent ID already exists.
// Uses raw JSON round-tripping to preserve unknown fields in CC's config, and
// writes the new member with Claude Code's camelCase keys.
func RegisterMember(teamName string, member Member) error {
	// Lock on a stable sidecar file; the config is updated via atomic rename.
	lockPath := TeamConfigPath(teamName) + ".lock"
	return lock.WithExclusiveFileLock(lockPath, func() error {
		raw, err := readTeamConfigRaw(teamName)
		if err != nil {
			return err
		}

		members, _ := raw["members"].([]any)

		// Check for duplicate
		for _, item := range members {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, err := memberAgentID(m)
			if err != nil {
				return err
			}
			if id == member.AgentID {
				name, _ := m["name"].(string)
				return fmt.Errorf("member with agent ID %q already registered (name=%q)", member.AgentID, name)
			}
		}

		// Append new member as raw map
		newMember := map[string]any{
			"name":      member.Name,
			"agentId":   member.AgentID,
			"agentType": member.AgentType,
		}
		raw["members"] = append(members, newMember)

		return atomicWriteJSON(TeamConfigPath(teamName), raw)
	})
}

// UnregisterMember removes a member by agent ID from the team config.
// Uses raw JSON round-tripping to preserve unknown fields in CC's config.
func UnregisterMember(teamName, agentID string) error {
	// Lock on a stable sidecar file; the config is updated via atomic rename.
	lockPath := TeamConfigPath(teamName) + ".lock"
	return lock.WithExclusiveFileLock(lockPath, func() error {
		raw, err := readTeamConfigRaw(teamName)
		if err != nil {
			return err
		}

		members, _ := raw["members"].([]any)

		found := false
		filtered := make([]any, 0, len(members))
		for _, item := range members {
			m, ok := item.(map[string]any)
			if !ok {
				filtered = append(filtered, item)
				continue
			}
			id, err := memberAgentID(m)
			if err != nil {
				return err
			}
			if id == agentID {
				found = true
				continue
			}
			filtered = append(filtered, item)
		}
		if !found {
			return fmt.Errorf("member with agent ID %q not found in team %q", agentID, teamName)
		}

		raw["members"] = filtered
		return atomicWriteJSON(TeamConfigPath(teamName), raw)
	})
}

// NewExternalAgentID generates an agent ID for external (non-Claude-Code) agents.
// Format: ext_{handle}_{timestamp}_{nanoseconds}_pid{pid}_{randhex}
func NewExternalAgentID(handle string) string {
	now := time.Now().UTC()
	ts := now.Format("20060102T150405")

	// Reduce collision risk if multiple agents join concurrently.
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback: still mix in time if the RNG fails for any reason.
		binary.LittleEndian.PutUint32(b[:], uint32(now.UnixNano()))
	}

	return fmt.Sprintf("ext_%s_%s_%09d_pid%d_%s",
		handle,
		ts,
		now.Nanosecond(),
		os.Getpid(),
		hex.EncodeToString(b[:]),
	)
}

func readTeamConfigRaw(teamName string) (map[string]any, error) {
	configPath := TeamConfigPath(teamName)
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read team config: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse team config: %w", err)
	}
	return raw, nil
}
