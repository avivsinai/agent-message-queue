package metadata

import (
	"encoding/json"
	"os"
	"time"
)

// SessionMeta represents the session.json advisory metadata file.
//
// This file is optional. Sessions created before federation support
// will not have a session.json, and all operations must continue to
// work without it.
type SessionMeta struct {
	Schema  int       `json:"schema"`
	Session string    `json:"session"`
	Topic   string    `json:"topic,omitempty"`
	Branch  string    `json:"branch,omitempty"`
	Claims  []string  `json:"claims,omitempty"`
	Updated time.Time `json:"updated"`
}

// WriteSessionMeta writes session metadata to the given path with 0600 permissions.
func WriteSessionMeta(path string, s SessionMeta) error {
	s.Schema = 1
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// ReadSessionMeta reads session metadata from the given path.
// Returns os.ErrNotExist (via os.IsNotExist) if the file does not exist,
// which callers should treat as a normal condition for legacy sessions.
func ReadSessionMeta(path string) (SessionMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionMeta{}, err
	}
	var s SessionMeta
	if err := json.Unmarshal(data, &s); err != nil {
		return SessionMeta{}, err
	}
	return s, nil
}
