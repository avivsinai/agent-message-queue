package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

const (
	ProjectConfigSchema = 1
	LocalConfigSchema   = 1
	DefaultSessionName  = "collab"
	LayoutColumns       = "columns"
	LauncherCMux        = "cmux"
	LauncherGhostty     = "ghostty"
	LauncherTMux        = "tmux"
	LauncherCommands    = "commands"
)

func knownLaunchers() []string {
	return []string{LauncherCMux, LauncherGhostty, LauncherTMux, LauncherCommands}
}

type ProjectConfig struct {
	Schema         int                  `json:"schema"`
	DefaultSession string               `json:"default_session"`
	Agents         []ProjectAgentConfig `json:"agents"`
	Layout         LayoutIntent         `json:"layout"`
}

type ProjectAgentConfig struct {
	Handle       string               `json:"handle"`
	Adapter      string               `json:"adapter"`
	Command      []string             `json:"command"`
	Env          map[string]string    `json:"env,omitempty"`
	Cwd          string               `json:"cwd,omitempty"`
	ResumePolicy ResumePolicy         `json:"resume_policy"`
	InitialInput *InitialInputRequest `json:"initial_input,omitempty"`
}

type LayoutIntent struct {
	Type string `json:"type"`
}

// LocalConfig is deliberately preference-only. Execution authority, roster,
// environment, cwd, bypass arguments, and session selection do not belong in
// this in-worktree file, even when the file is already tracked.
type LocalConfig struct {
	Schema             int      `json:"schema"`
	LauncherPreference []string `json:"launcher_preference"`
}

type ConfigAuthorityConflictError struct {
	Path  string
	Field string
}

func (e *ConfigAuthorityConflictError) Error() string {
	return fmt.Sprintf("configuration authority conflict: %s must not define %q", e.Path, e.Field)
}

var canonicalSessionPattern = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]*$`)

func ParseProjectConfig(data []byte) (ProjectConfig, error) {
	var cfg ProjectConfig
	if err := decodeStrictJSON(data, &cfg); err != nil {
		return ProjectConfig{}, fmt.Errorf("decode launch config: %w", err)
	}
	var fields struct {
		DefaultSession json.RawMessage              `json:"default_session"`
		Agents         []map[string]json.RawMessage `json:"agents"`
		Layout         json.RawMessage              `json:"layout"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return ProjectConfig{}, fmt.Errorf("inspect launch config fields: %w", err)
	}
	if fields.DefaultSession == nil {
		cfg.DefaultSession = DefaultSessionName
	}
	if fields.Layout == nil {
		cfg.Layout.Type = LayoutColumns
	}
	for i, agentFields := range fields.Agents {
		if _, ok := agentFields["adapter"]; !ok && len(cfg.Agents[i].Command) > 0 {
			cfg.Agents[i].Adapter = cfg.Agents[i].Command[0]
		}
		if _, ok := agentFields["resume_policy"]; !ok {
			cfg.Agents[i].ResumePolicy = ResumeEnabled
		}
	}
	if err := cfg.Validate(); err != nil {
		return ProjectConfig{}, err
	}
	return cfg, nil
}

func ParseLocalConfig(path string, data []byte) (LocalConfig, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return LocalConfig{}, fmt.Errorf("decode local launch config: %w", err)
	}
	for _, field := range []string{
		"agents", "default_session", "layout", "resume_policy", "command",
		"argv", "env", "cwd", "bypass_args", "trust", "root",
	} {
		if _, ok := fields[field]; ok {
			return LocalConfig{}, &ConfigAuthorityConflictError{Path: path, Field: field}
		}
	}
	var cfg LocalConfig
	if err := decodeStrictJSON(data, &cfg); err != nil {
		return LocalConfig{}, fmt.Errorf("decode local launch config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return LocalConfig{}, err
	}
	return cfg, nil
}

func (cfg ProjectConfig) Validate() error {
	if cfg.Schema != ProjectConfigSchema {
		return fmt.Errorf("unsupported launch config schema %d", cfg.Schema)
	}
	if !canonicalSessionPattern.MatchString(cfg.DefaultSession) || strings.HasPrefix(cfg.DefaultSession, "-") {
		return fmt.Errorf("invalid default session %q", cfg.DefaultSession)
	}
	if len(cfg.Agents) == 0 {
		return fmt.Errorf("launch config requires at least one agent")
	}
	seen := make(map[string]struct{}, len(cfg.Agents))
	for i, agent := range cfg.Agents {
		if !canonicalSessionPattern.MatchString(agent.Handle) || strings.HasPrefix(agent.Handle, "-") {
			return fmt.Errorf("agent %d has invalid handle %q", i, agent.Handle)
		}
		if _, ok := seen[agent.Handle]; ok {
			return fmt.Errorf("duplicate agent handle %q", agent.Handle)
		}
		seen[agent.Handle] = struct{}{}
		if agent.Adapter == "" {
			return fmt.Errorf("agent %q has no adapter", agent.Handle)
		}
		if len(agent.Command) == 0 || agent.Command[0] != agent.Adapter {
			return fmt.Errorf("agent %q command must select its adapter-known executable", agent.Handle)
		}
		for _, arg := range agent.Command {
			if arg == "" || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("agent %q command contains an invalid argument", agent.Handle)
			}
		}
		if agent.InitialInput != nil {
			if agent.InitialInput.Kind != InitialInputArgument && agent.InitialInput.Kind != InitialInputFile {
				return fmt.Errorf("agent %q initial input kind is unsupported", agent.Handle)
			}
			if !validDigest(agent.InitialInput.SHA256) || strings.ContainsRune(agent.InitialInput.Value, 0) {
				return fmt.Errorf("agent %q initial input is invalid", agent.Handle)
			}
		}
		if agent.ResumePolicy != ResumeEnabled && agent.ResumePolicy != ResumeFresh && agent.ResumePolicy != ResumeDisabled {
			return fmt.Errorf("agent %q has invalid resume policy %q", agent.Handle, agent.ResumePolicy)
		}
	}
	if cfg.Layout.Type != LayoutColumns {
		return fmt.Errorf("unsupported layout intent %q", cfg.Layout.Type)
	}
	return nil
}

func (cfg LocalConfig) Validate() error {
	if cfg.Schema != LocalConfigSchema {
		return fmt.Errorf("unsupported local launch config schema %d", cfg.Schema)
	}
	if len(cfg.LauncherPreference) == 0 {
		return fmt.Errorf("local launch config requires a launcher preference")
	}
	seen := make(map[string]struct{}, len(cfg.LauncherPreference))
	for _, launcher := range cfg.LauncherPreference {
		if !slices.Contains(knownLaunchers(), launcher) {
			return fmt.Errorf("unsupported launcher preference %q", launcher)
		}
		if _, ok := seen[launcher]; ok {
			return fmt.Errorf("duplicate launcher preference %q", launcher)
		}
		seen[launcher] = struct{}{}
	}
	if _, ok := seen[LauncherCommands]; !ok {
		return fmt.Errorf("launcher preference must include %q fallback", LauncherCommands)
	}
	return nil
}

func MarshalProjectConfig(cfg ProjectConfig) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return marshalConfig(cfg)
}

func MarshalLocalConfig(cfg LocalConfig) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return marshalConfig(cfg)
}

func marshalConfig(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}
