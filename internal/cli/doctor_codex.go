package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/pelletier/go-toml/v2"
)

// checkCodexShellEnvironment reports whether the user's Codex configuration
// keeps the AMQ identity in tool commands. coop exec sets it through
// shell_environment_policy.set, which survives every inherit mode and
// exclude, but Codex applies include_only (or filters marked "include")
// after set and drops every variable no pattern matches
// (protocol/src/shell_environment.rs populate_env). The row shows only when
// such a list exists. Only the user config.toml is read; a profile or
// project layer is not.
func checkCodexShellEnvironment() (doctorCheck, bool) {
	patterns, config, err := codexIncludePatterns()
	if err != nil || len(patterns) == 0 {
		return doctorCheck{}, false
	}
	check := doctorCheck{Name: "Codex shell environment", Status: "ok", Message: "include_only keeps the AMQ identity variables"}
	if dropped := codexDroppedIdentity(patterns); len(dropped) > 0 {
		check.Status = "warn"
		check.Message = fmt.Sprintf("Codex removes %s from tool commands, so amq inside a Codex seat has no AMQ identity; add \"AM_*\" to shell_environment_policy include_only in %s", strings.Join(dropped, ", "), config)
	}
	return check, true
}

// refuseCodexIncludePolicy stops a Codex seat whose user config.toml has an
// include policy that drops the identity coop exec gives it. A -c override
// of that policy is refused before this (codexOverrideSetsIdentity), so the
// file alone decides; Codex refuses a file that mixes include_only and
// filters. A config.toml AMQ cannot read refuses nothing; Codex itself
// reports it.
func refuseCodexIncludePolicy(binaryPath string) error {
	if launch.ProviderForExecutable(binaryPath) != launch.CodexProvider {
		return nil
	}
	patterns, config, err := codexIncludePatterns()
	if err != nil || len(patterns) == 0 {
		return nil
	}
	if dropped := codexDroppedIdentity(patterns); len(dropped) > 0 {
		return UsageError("Codex would remove %s from this seat's tool commands (shell_environment_policy include_only or an include filter in %s), so its amq commands would have no AMQ identity; add \"AM_*\" to that list. Codex was not started", strings.Join(dropped, ", "), config)
	}
	return nil
}

// codexIncludePatterns returns the include patterns of the user
// config.toml's shell_environment_policy: include_only, or the filters
// marked "include" (protocol/src/shell_environment.rs populate_env keeps
// only matching variables). config is the config.toml path.
func codexIncludePatterns() (patterns []string, config string, err error) {
	codexHome, err := codexHomeDir()
	if err != nil {
		return nil, "", err
	}
	config = filepath.Join(codexHome, "config.toml")
	var file struct {
		Policy struct {
			IncludeOnly []string          `toml:"include_only"`
			Filters     map[string]string `toml:"filters"`
		} `toml:"shell_environment_policy"`
	}
	raw, err := os.ReadFile(config)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, config, nil
	case err != nil:
		return nil, config, err
	}
	if err := toml.Unmarshal(raw, &file); err != nil {
		return nil, config, err
	}
	patterns = append(patterns, file.Policy.IncludeOnly...)
	for pattern, filter := range file.Policy.Filters {
		if filter == "include" {
			patterns = append(patterns, pattern)
		}
	}
	return patterns, config, nil
}

// codexDroppedIdentity returns the identity variables no pattern keeps.
func codexDroppedIdentity(patterns []string) []string {
	var dropped []string
	for _, key := range identityEnvKeys {
		if !codexEnvPatternsMatch(patterns, key) {
			dropped = append(dropped, key)
		}
	}
	return dropped
}

// codexEnvPatternsMatch matches name the way Codex matches an environment
// pattern: a case-insensitive wildcard where * is any run and ? one
// character.
func codexEnvPatternsMatch(patterns []string, name string) bool {
	for _, pattern := range patterns {
		expr := strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern))
		if matched, err := regexp.MatchString("(?is)^"+expr+"$", name); err == nil && matched {
			return true
		}
	}
	return false
}
