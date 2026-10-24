package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// legacyCoopHookScripts are the co-op hook scripts AMQ no longer ships. A
// project entry that still names one runs a missing file, or an old copy
// that duplicates the hooks amq coop install-hooks installs.
var legacyCoopHookScripts = []string{
	"claude-session-start.sh",
	"amq-stop-hook.sh",
	"claude-amq-user-prompt-submit.py",
}

// legacyCoopHookEntries lists, as "<settings file>: <event>: <command>", the
// hook commands in dir/.claude/settings.json and settings.local.json that run
// a legacy co-op script. A missing file lists none.
func legacyCoopHookEntries(dir string) ([]string, error) {
	var out []string
	for _, name := range []string{"settings.json", "settings.local.json"} {
		path := filepath.Join(dir, ".claude", name)
		raw, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var settings struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		events := make([]string, 0, len(settings.Hooks))
		for event := range settings.Hooks {
			events = append(events, event)
		}
		sort.Strings(events)
		for _, event := range events {
			for _, group := range settings.Hooks[event] {
				for _, hook := range group.Hooks {
					if runsLegacyCoopScript(hook.Command) {
						out = append(out, fmt.Sprintf("%s: %s: %s", path, event, hook.Command))
					}
				}
			}
		}
	}
	return out, nil
}

func runsLegacyCoopScript(command string) bool {
	for _, script := range legacyCoopHookScripts {
		if strings.Contains(command, script) {
			return true
		}
	}
	return false
}

// checkLegacyCoopHooks reports project hook entries under the current
// directory that run the old co-op scripts, which AMQ no longer ships. It is
// read-only and reports nothing when none exist.
func checkLegacyCoopHooks() (doctorCheck, bool) {
	check := doctorCheck{Name: "Co-op project hooks", Status: "warn"}
	entries, err := legacyCoopHookEntries(".")
	if err != nil {
		check.Message = err.Error()
		return check, true
	}
	if len(entries) == 0 {
		return doctorCheck{}, false
	}
	check.Message = fmt.Sprintf("entries run co-op scripts AMQ no longer ships: %s; remedy: remove these entries, then run amq coop install-hooks",
		strings.Join(entries, "; "))
	return check, true
}
