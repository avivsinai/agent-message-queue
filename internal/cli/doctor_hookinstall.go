package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/hookinstall"
)

// checkHookConfigs scans the default Claude and Codex hook config files for
// AMQ-owned SessionStart hook commands whose installed script path no longer
// exists (the WP-646 leak class: macOS reclaims /var/folders temp dirs and the
// dead command then fails with exit 127 on every session start). It is
// read-only; it never repairs. The remedy is the exact install command that
// rewrites a live hook and prunes the stale ones via the installer's self-heal,
// scoped to the agent(s) actually found stale.
func checkHookConfigs() doctorCheck {
	check := doctorCheck{Name: "Hook configs"}
	claudeConfig, claudeErr := hookinstall.DefaultClaudeConfig()
	codexConfig, codexErr := hookinstall.DefaultCodexConfig()

	type staleAgent struct {
		agent string
		hooks []hookinstall.StaleSessionStartHook
	}
	var perAgent []staleAgent
	var warnings []string

	for _, c := range []struct {
		path, agent string
		pathErr     error
	}{
		{codexConfig, hookinstall.AgentCodex, codexErr},
		{claudeConfig, hookinstall.AgentClaude, claudeErr},
	} {
		if c.pathErr != nil {
			// Could not resolve the default config path at all.
			warnings = append(warnings, fmt.Sprintf("%s config path: %v", c.agent, c.pathErr))
			continue
		}
		if c.path == "" {
			continue
		}
		found, err := hookinstall.StaleSessionStartHooks(c.path, c.agent)
		if err != nil {
			// A missing config file is not an error: AMQ hooks may not be
			// installed. Any other error (corrupt JSON, permission denied) is
			// surfaced as a warning naming the path and the error so it is not
			// silently swallowed.
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("%s config %s: %v", c.agent, c.path, err))
			}
			continue
		}
		if len(found) > 0 {
			perAgent = append(perAgent, staleAgent{agent: c.agent, hooks: found})
		}
	}

	totalStale := 0
	for _, a := range perAgent {
		totalStale += len(a.hooks)
	}
	if totalStale == 0 && len(warnings) == 0 {
		check.Status = "ok"
		check.Message = "no stale AMQ SessionStart hooks"
		return check
	}
	if totalStale == 0 {
		// Only warnings (e.g. a corrupt config file), no stale hooks.
		check.Status = "warn"
		check.Message = strings.Join(warnings, "; ")
		return check
	}

	var paths []string
	var agents []string
	for _, a := range perAgent {
		agents = append(agents, a.agent)
		for _, h := range a.hooks {
			paths = append(paths, h.ScriptPath)
		}
	}
	// The remedy uses the real install-hook subcommand spelling, scoped to the
	// agent(s) actually found stale. Verified via `amq-keepalive install-hook
	// --help`: -agent accepts claude, codex, or both.
	remedyAgent := strings.Join(agents, ",")
	if len(agents) > 1 {
		remedyAgent = hookinstall.AgentBoth
	}
	check.Status = "warn"
	message := fmt.Sprintf(
		"%d stale AMQ SessionStart hook(s) point at missing scripts (%s); remedy: amq-keepalive install-hook --agent %s",
		totalStale,
		strings.Join(uniqueNonEmpty(paths), ", "),
		remedyAgent,
	)
	// Surface config warnings alongside stale findings rather than hiding the
	// stale hooks when one agent's config is corrupt and another has dead hooks.
	if len(warnings) > 0 {
		message += "; " + strings.Join(warnings, "; ")
	}
	check.Message = message
	return check
}

// uniqueNonEmpty returns the deduplicated, non-empty subset of values, preserving
// first-seen order. Used to keep the doctor remedy message compact.
func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
