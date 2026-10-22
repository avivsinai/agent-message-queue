package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
	codexHome, err := codexHomeDir()
	if err != nil {
		return doctorCheck{}, false
	}
	raw, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		return doctorCheck{}, false
	}
	var config struct {
		Policy struct {
			IncludeOnly []string          `toml:"include_only"`
			Filters     map[string]string `toml:"filters"`
		} `toml:"shell_environment_policy"`
	}
	if toml.Unmarshal(raw, &config) != nil {
		return doctorCheck{}, false
	}
	patterns := config.Policy.IncludeOnly
	for pattern, filter := range config.Policy.Filters {
		if filter == "include" {
			patterns = append(patterns, pattern)
		}
	}
	if len(patterns) == 0 {
		return doctorCheck{}, false
	}
	var dropped []string
	for _, key := range identityEnvKeys {
		if !codexEnvPatternsMatch(patterns, key) {
			dropped = append(dropped, key)
		}
	}
	check := doctorCheck{Name: "Codex shell environment", Status: "ok", Message: "include_only keeps the AMQ identity variables"}
	if len(dropped) > 0 {
		check.Status = "warn"
		check.Message = fmt.Sprintf("Codex removes %s from tool commands, so amq inside a Codex seat has no AMQ identity; add \"AM_*\" to shell_environment_policy include_only in %s", strings.Join(dropped, ", "), filepath.Join(codexHome, "config.toml"))
	}
	return check, true
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
