package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Config is persisted to meta/config.json and captures the initial setup.
type Config struct {
	Version    int      `json:"version"`
	CreatedUTC string   `json:"created_utc"`
	Agents     []string `json:"agents"`
}

func WriteConfig(path string, cfg Config, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config already exists at %s (use --force to overwrite)", path)
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), data, 0o600)
	return err
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// EnsureAgent adds handle to the config's agents list if it is not already
// present, preserving every other agent. It creates the config if it does not
// exist. The write is atomic (fsq.WriteFileAtomic). It is idempotent: a
// repeated call with an already-present handle is a no-op.
//
// This is the registration seam a companion uses to make its mailbox handle
// discoverable to other agents in the root: the handle must be in config.json
// so senders can route to it. amqio.New stays free of configuration side
// effects; the caller (serve, up) registers the handle through this function.
func EnsureAgent(path, handle string) (bool, error) {
	cfg, err := LoadConfig(path)
	switch {
	case err == nil:
		// Config exists: preserve every agent, add handle if missing.
		for _, a := range cfg.Agents {
			if a == handle {
				return false, nil // already present; no-op
			}
		}
		cfg.Agents = append(cfg.Agents, handle)
	case os.IsNotExist(err):
		// No config: create one with this handle.
		cfg = Config{Version: 1, CreatedUTC: nowUTC(), Agents: []string{handle}}
	default:
		return false, fmt.Errorf("load config: %w", err)
	}
	if err := WriteConfig(path, cfg, true); err != nil {
		return false, err
	}
	return true, nil
}

// nowUTC returns the current time in RFC 3339 UTC, for CreatedUTC.
var nowUTC = func() string {
	return time.Now().UTC().Format(time.RFC3339)
}
