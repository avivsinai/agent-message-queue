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
// preserves Version/CreatedUTC AND every unmodelled key (611.22.55: the
// Config struct models version/created_utc/agents only; re-marshalling the
// bare struct dropped any other key an operator hand-added or a newer
// writer emitted - probe names default_agent, project, wake, extensions,
// routing). The read decodes the file into a raw key map, the known fields
// are overlaid, agents is mutated, and the full map is re-marshalled
// (keys sorted by encoding/json - deterministic). Unknown keys are
// preserved SEMANTICALLY: their values round-trip intact as opaque raw
// JSON, but the whole file is re-serialized (sorted keys, 2-space
// indentation). Since 611.22.57 the same preservation is shared by the
// EnsureAgent/setup/apply writers (amq setup and launch apply overlay
// through the exported MarshalPreservingUnknowns; a forced initialization
// still intentionally replaces the whole file). Creates the
// config when absent.
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
	out, mErr := marshalConfigPreservingUnknowns(data, cfg)
	if mErr != nil {
		return false, mErr
	}
	if _, wErr := root.WriteFileAtomic("meta", "config.json", append(out, '\n'), 0o600); wErr != nil {
		return false, wErr
	}
	return true, nil
}

// MarshalPreservingUnknowns re-marshals cfg together with every key present
// in the original raw document but not modelled by Config (611.22.55,
// extended to all config.json writers in 611.22.57). Known keys always take
// the struct's values; unknown keys are re-emitted as their original raw
// JSON. Keys come out sorted (map marshalling), which is deterministic
// across writers — including a nil/empty original, so a fresh write and a
// re-read rewrite are byte-stable (breaking that stability made a matching
// `amq setup` rerun report a roster update).
func MarshalPreservingUnknowns(original []byte, cfg Config) ([]byte, error) {
	return marshalConfigPreservingUnknowns(original, cfg)
}

// marshalConfigPreservingUnknowns re-marshals cfg together with every key
// present in the original raw document but not modelled by Config
// (611.22.55). Known keys always take the struct's values; unknown keys are
// re-emitted as their original raw JSON. Keys come out sorted (map
// marshalling), which is deterministic across writers.
func marshalConfigPreservingUnknowns(original []byte, cfg Config) ([]byte, error) {
	var raw map[string]json.RawMessage
	if len(original) > 0 {
		if err := json.Unmarshal(original, &raw); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	// A literal JSON `null` unmarshals into a nil map with no error; treat
	// it like an empty document instead of panicking on the overlay
	// (review-821-r1 P1-a; a panic here would kill amq-remote serve, whose
	// caller degrades registration errors to warnings by design).
	if raw == nil {
		raw = make(map[string]json.RawMessage)
	}
	known, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var modelled map[string]json.RawMessage
	if err := json.Unmarshal(known, &modelled); err != nil {
		return nil, err
	}
	for k, v := range modelled {
		raw[k] = v
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nowUTC returns the current time in RFC 3339 UTC, for CreatedUTC.
var nowUTC = func() string {
	return time.Now().UTC().Format(time.RFC3339)
}
