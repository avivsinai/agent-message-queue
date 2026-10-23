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
	patterns, config, err := codexIncludePatterns(nil)
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

// refuseCodexIncludePolicy stops a Codex seat whose include policy would
// drop the identity coop exec gives it: the user config.toml and the seat's
// -c overrides of shell_environment_policy.include_only or .filters. A
// config.toml AMQ cannot read refuses nothing; Codex itself reports it.
func refuseCodexIncludePolicy(binaryPath string, args []string) error {
	if launch.ProviderForExecutable(binaryPath) != launch.CodexProvider {
		return nil
	}
	patterns, config, err := codexIncludePatterns(args)
	if err != nil || len(patterns) == 0 {
		return nil
	}
	if dropped := codexDroppedIdentity(patterns); len(dropped) > 0 {
		return UsageError("Codex would remove %s from this seat's tool commands (shell_environment_policy include_only or an include filter), so its amq commands would have no AMQ identity; add \"AM_*\" to that list in %s or in the -c override. Codex was not started", strings.Join(dropped, ", "), config)
	}
	return nil
}

// codexIncludePatterns returns the include patterns Codex keeps tool
// environment variables by: include_only, or the -c override that replaces
// it, plus every filter marked "include" from config.toml and -c. config is
// the config.toml path.
func codexIncludePatterns(args []string) (patterns []string, config string, err error) {
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
	case err != nil:
		return nil, config, err
	default:
		if err := toml.Unmarshal(raw, &file); err != nil {
			return nil, config, err
		}
	}
	includeOnly, filters := file.Policy.IncludeOnly, file.Policy.Filters
	if filters == nil {
		filters = map[string]string{}
	}
	for _, override := range codexConfigOverrides(args) {
		key, value, _ := strings.Cut(override, "=")
		key = strings.TrimSpace(key)
		var parsed struct {
			V any `toml:"v"`
		}
		if toml.Unmarshal([]byte("v = "+value), &parsed) != nil {
			parsed.V = strings.TrimSpace(value)
		}
		lower := strings.ToLower(key)
		switch {
		case lower == "shell_environment_policy.include_only":
			includeOnly = nil
			if list, ok := parsed.V.([]any); ok {
				for _, item := range list {
					if pattern, ok := item.(string); ok {
						includeOnly = append(includeOnly, pattern)
					}
				}
			}
		case lower == "shell_environment_policy.filters":
			if table, ok := parsed.V.(map[string]any); ok {
				for pattern, filter := range table {
					if text, ok := filter.(string); ok {
						filters[pattern] = text
					}
				}
			}
		case strings.HasPrefix(lower, "shell_environment_policy.filters."):
			pattern := strings.Trim(key[len("shell_environment_policy.filters."):], `"'`)
			if text, ok := parsed.V.(string); ok {
				filters[pattern] = text
			}
		}
	}
	patterns = append(patterns, includeOnly...)
	for pattern, filter := range filters {
		if filter == "include" {
			patterns = append(patterns, pattern)
		}
	}
	return patterns, config, nil
}

// codexConfigOverrides returns the values of the -c and --config options in
// args, before the prompt.
func codexConfigOverrides(args []string) []string {
	var values []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		flag, value, inline := strings.Cut(arg, "=")
		switch {
		case flag == "-c" || flag == "--config":
			if !inline {
				if i+1 >= len(args) {
					return values
				}
				i++
				value = args[i]
			}
		case strings.HasPrefix(arg, "-c") && !strings.HasPrefix(arg, "--"):
			value = arg[2:]
		default:
			if codexValueOptions[flag] && !inline {
				i++
			}
			continue
		}
		values = append(values, value)
	}
	return values
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
