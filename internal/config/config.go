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
// exist. The write is atomic and guarded against concurrent registration:
// the config is pinned (opened as a regular file, no symlink), verified
// before the write, and written through the DeliveryRoot — the same seam
// provisionExistingApplyRoster uses (611.22.19 round-2 B2). It is idempotent:
// a repeated call with an already-present handle is a no-op.
//
// This is the registration seam a companion uses to make its mailbox handle
// discoverable to other agents in the root: the handle must be in config.json
// so senders can route to it. amqio.New stays free of configuration side
// effects; the caller (serve) registers the handle through this function.
// rootDir is the AMQ root (the DeliveryRoot base), not the config path.
func EnsureAgent(rootDir, handle string) (bool, error) {
	// B1: validate the handle BEFORE any write so a bad --me does not poison
	// config.json. This is the same validation fsq applies when reading the
	// config; applying it at the door prevents an invalid handle from landing
	// and blocking coop init / setup / doctor.
	if err := fsq.ValidateHandle(handle); err != nil {
		return false, fmt.Errorf("invalid handle %q: %w", handle, err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(rootDir)
	if err != nil {
		return false, fmt.Errorf("snapshot root: %w", err)
	}
	root, err := fsq.OpenDeliveryRoot(rootDir, identity)
	if err != nil {
		return false, fmt.Errorf("open root: %w", err)
	}
	// B2: guarded RMW via an exclusive advisory lock on meta/config.lock.
	// The lock is held for the entire read-modify-write, so two concurrent
	// EnsureAgent calls cannot lose a registration. This is the same
	// flock-based seam DLQ envelope locks use (WithDLQEnvelopeLock), exposed
	// as WithConfigLock for config.json mutations. The pin+verify pattern
	// (OpenMailboxConfigAuthorization) alone is optimistic and cannot prevent
	// last-writer-wins; the lock makes it correct.
	var result struct {
		added bool
		err   error
	}
	err = root.WithConfigLock(func(r *fsq.DeliveryRoot) error {
		result.added, result.err = ensureAgentLocked(r, handle)
		return result.err
	})
	if err != nil {
		return false, err
	}
	return result.added, result.err
}

// ensureAgentLocked does the read-modify-write of config.json under the
// config lock. It reads via the DeliveryRoot (regular-file, no-follow),
// preserves Version/CreatedUTC, adds the handle if missing, and writes
// atomically through the root.
func ensureAgentLocked(root *fsq.DeliveryRoot, handle string) (bool, error) {
	data, err := root.ReadFile("meta/config.json")
	if err != nil {
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("read config: %w", err)
		}
		// No config: create one with this handle.
		cfg := Config{Version: 1, CreatedUTC: nowUTC(), Agents: []string{handle}}
		out, mErr := json.MarshalIndent(cfg, "", "  ")
		if mErr != nil {
			return false, mErr
		}
		if _, wErr := root.WriteFileAtomic("meta", "config.json", append(out, '\n'), 0o600); wErr != nil {
			return false, wErr
		}
		return true, nil
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, fmt.Errorf("parse config: %w", err)
	}
	for _, a := range cfg.Agents {
		if a == handle {
			return false, nil // already present; no-op
		}
	}
	cfg.Agents = append(cfg.Agents, handle)
	out, mErr := json.MarshalIndent(cfg, "", "  ")
	if mErr != nil {
		return false, mErr
	}
	if _, wErr := root.WriteFileAtomic("meta", "config.json", append(out, '\n'), 0o600); wErr != nil {
		return false, wErr
	}
	return true, nil
}

// nowUTC returns the current time in RFC 3339 UTC, for CreatedUTC.
var nowUTC = func() string {
	return time.Now().UTC().Format(time.RFC3339)
}
