package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const maxEnqueueConfigSize = 64 * 1024

// EnqueueConfig is the audited Bot-side bridge enqueue configuration.
type EnqueueConfig struct {
	Root               string   `json:"root"`
	SourceHost         string   `json:"source_host"`
	SourceHandle       string   `json:"source_handle"`
	AllowedDestAliases []string `json:"allowed_dest_aliases"`
}

// LoadEnqueueConfig reads and validates a private mode-0600 enqueue config file.
func LoadEnqueueConfig(path string) (EnqueueConfig, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return EnqueueConfig{}, fmt.Errorf("stat enqueue config: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return EnqueueConfig{}, fmt.Errorf("enqueue config %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return EnqueueConfig{}, fmt.Errorf("enqueue config %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return EnqueueConfig{}, fmt.Errorf("enqueue config %s mode is %o, want 0600", path, got)
	}

	file, _, err := fsq.OpenRegularNoFollow(path)
	if err != nil {
		return EnqueueConfig{}, fmt.Errorf("open enqueue config: %w", err)
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxEnqueueConfigSize+1))
	if err != nil {
		return EnqueueConfig{}, fmt.Errorf("read enqueue config: %w", err)
	}
	if len(data) > maxEnqueueConfigSize {
		return EnqueueConfig{}, fmt.Errorf("enqueue config exceeds %d bytes", maxEnqueueConfigSize)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg EnqueueConfig
	if err := dec.Decode(&cfg); err != nil {
		return EnqueueConfig{}, fmt.Errorf("parse enqueue config: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return EnqueueConfig{}, fmt.Errorf("enqueue config has trailing JSON")
		}
		return EnqueueConfig{}, fmt.Errorf("parse enqueue config trailer: %w", err)
	}
	if err := ValidateEnqueueConfig(cfg); err != nil {
		return EnqueueConfig{}, err
	}
	return normalizeEnqueueConfig(cfg)
}

// ValidateEnqueueConfig checks the decoded enqueue configuration.
func ValidateEnqueueConfig(cfg EnqueueConfig) error {
	if strings.TrimSpace(cfg.Root) == "" {
		return fmt.Errorf("enqueue config root is required")
	}
	if err := fsq.ValidateHandle(cfg.SourceHost); err != nil {
		return fmt.Errorf("enqueue config source_host: %w", err)
	}
	if err := fsq.ValidateHandle(cfg.SourceHandle); err != nil {
		return fmt.Errorf("enqueue config source_handle: %w", err)
	}
	if len(cfg.AllowedDestAliases) == 0 {
		return fmt.Errorf("enqueue config allowed_dest_aliases must not be empty")
	}
	seen := make(map[string]struct{}, len(cfg.AllowedDestAliases))
	for _, alias := range cfg.AllowedDestAliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			return fmt.Errorf("enqueue config allowed_dest_aliases contains a blank entry")
		}
		if _, _, err := ParseAlias(alias); err != nil {
			return fmt.Errorf("enqueue config allowed_dest_aliases: %w", err)
		}
		if _, ok := seen[alias]; ok {
			return fmt.Errorf("enqueue config allowed_dest_aliases contains duplicate %q", alias)
		}
		seen[alias] = struct{}{}
	}
	return nil
}

func normalizeEnqueueConfig(cfg EnqueueConfig) (EnqueueConfig, error) {
	root, err := filepath.Abs(strings.TrimSpace(cfg.Root))
	if err != nil {
		return EnqueueConfig{}, fmt.Errorf("resolve enqueue config root: %w", err)
	}
	cfg.Root = root
	return cfg, nil
}

// AllowedDestSet returns the configured destination aliases as a lookup set.
func (cfg EnqueueConfig) AllowedDestSet() map[string]struct{} {
	set := make(map[string]struct{}, len(cfg.AllowedDestAliases))
	for _, alias := range cfg.AllowedDestAliases {
		set[strings.TrimSpace(alias)] = struct{}{}
	}
	return set
}
