package metadata

import (
	"encoding/json"
	"os"
	"time"
)

// AgentMeta represents the agent.json advisory metadata file.
//
// This file is optional. Agents without an agent.json still receive
// messages normally. All fields are advisory and informational.
//
// Channel membership (the Channels field) is ADVISORY metadata only,
// not an authorization mechanism. Any agent can send to any other agent
// regardless of channel membership. Channels are used for fan-out
// discovery: when sending to #channel, the resolver checks agent.json
// files to find which agents have subscribed to that channel name.
// There is no enforcement — an agent omitted from a channel can still
// receive direct messages, and channel membership can be stale.
type AgentMeta struct {
	Schema   int       `json:"schema"`
	Agent    string    `json:"agent"`
	LastSeen time.Time `json:"last_seen"`
	Channels []string  `json:"channels,omitempty"`
}

// WriteAgentMeta writes agent metadata to the given path with 0600 permissions.
func WriteAgentMeta(path string, a AgentMeta) error {
	a.Schema = 1
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// ReadAgentMeta reads agent metadata from the given path.
// Returns os.ErrNotExist (via os.IsNotExist) if the file does not exist,
// which callers should treat as a normal condition for legacy agents.
func ReadAgentMeta(path string) (AgentMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentMeta{}, err
	}
	var a AgentMeta
	if err := json.Unmarshal(data, &a); err != nil {
		return AgentMeta{}, err
	}
	return a, nil
}

// TouchLastSeen updates the last_seen field to now.
func TouchLastSeen(path string) error {
	a, err := ReadAgentMeta(path)
	if err != nil {
		return err
	}
	a.LastSeen = time.Now().UTC()
	return WriteAgentMeta(path, a)
}

// IsActive returns true if the agent was seen within the given TTL.
func (a AgentMeta) IsActive(ttl time.Duration) bool {
	return time.Since(a.LastSeen) < ttl
}
