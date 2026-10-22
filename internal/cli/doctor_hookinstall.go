package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/hookinstall"
)

// checkHookConfigs scans the default Claude and Codex hook config files for
// AMQ-owned SessionStart hooks that need a rewrite: commands whose installed
// script path no longer exists (the WP-646 leak class: macOS reclaims
// /var/folders temp dirs and the dead command then fails with exit 127 on
// every session start), and live hooks in an old form or installed twice. It
// is read-only; it never repairs. The remedy is the exact install command,
// which rewrites a live hook, prunes stale ones, and leaves one current entry,
// scoped to the agent(s) actually found.
func checkHookConfigs() doctorCheck {
	check := doctorCheck{Name: "Hook configs"}
	claudeConfig, claudeErr := hookinstall.DefaultClaudeConfig()
	codexConfig, codexErr := hookinstall.DefaultCodexConfig()

	var stale []hookinstall.StaleSessionStartHook
	var outdated []hookinstall.OutdatedSessionStartHook
	var agents []string
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
		if err == nil {
			var old []hookinstall.OutdatedSessionStartHook
			old, err = hookinstall.OutdatedSessionStartHooks(c.path, c.agent)
			outdated = append(outdated, old...)
			if len(found) > 0 || len(old) > 0 {
				agents = append(agents, c.agent)
			}
		}
		if err != nil {
			// A missing config file is not an error: AMQ hooks may not be
			// installed. Any other error (corrupt JSON, permission denied) is
			// surfaced as a warning naming the path, the error, and a remedy so it
			// is not silently swallowed.
			if !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf("%s config %s: %v; %s", c.agent, c.path, err, hookConfigErrorRemedy(c.path, c.agent, err)))
			}
			continue
		}
		stale = append(stale, found...)
	}

	if len(agents) == 0 && len(warnings) == 0 {
		check.Status = "ok"
		check.Message = "no stale or outdated AMQ SessionStart hooks"
		return check
	}
	check.Status = "warn"
	if len(agents) == 0 {
		// Only warnings (e.g. a corrupt config file), no hook findings.
		check.Message = strings.Join(warnings, "; ")
		return check
	}

	var findings []string
	if len(stale) > 0 {
		var paths []string
		for _, h := range stale {
			paths = append(paths, h.ScriptPath)
		}
		findings = append(findings, fmt.Sprintf("%d stale AMQ SessionStart hook(s) point at missing scripts (%s)",
			len(stale), strings.Join(uniqueNonEmpty(paths), ", ")))
	}
	if len(outdated) > 0 {
		var items []string
		for _, h := range outdated {
			items = append(items, fmt.Sprintf("%s %s: %s", h.Agent, h.ScriptPath, h.Reason))
		}
		findings = append(findings, fmt.Sprintf("%d AMQ SessionStart hook(s) need a rewrite (%s)",
			len(outdated), strings.Join(items, ", ")))
	}
	// The remedy uses the real install-hook subcommand spelling, scoped to the
	// agent(s) actually found. Verified via `amq-keepalive install-hook
	// --help`: -agent accepts claude, codex, or both.
	remedyAgent := strings.Join(agents, ",")
	if len(agents) > 1 {
		remedyAgent = hookinstall.AgentBoth
	}
	message := strings.Join(findings, "; ") + "; remedy: amq-keepalive install-hook --agent " + remedyAgent
	if slices.Contains(agents, hookinstall.AgentCodex) {
		message += " (" + hookinstall.CodexRetrustNote + ")"
	}
	// Surface config warnings alongside the findings rather than hiding them
	// when one agent's config is corrupt and another needs a rewrite.
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

// hookConfigErrorRemedy returns a concrete remedy for a hook config read error.
// A JSON parse error points at restoring the newest backup or fixing the JSON;
// a permission error points at chmod. Other errors get a generic re-install
// remedy.
func hookConfigErrorRemedy(path, agent string, err error) string {
	msg := err.Error()
	if errors.Is(err, os.ErrPermission) || strings.Contains(msg, "permission denied") {
		return fmt.Sprintf("chmod u+rw %s", path)
	}
	// loadJSONObject wraps JSON decode failures with a "parse <path>:" prefix;
	// the underlying json error mentions syntax/invalid/unexpected characters.
	if strings.Contains(msg, "parse ") || strings.Contains(msg, "invalid") ||
		strings.Contains(msg, "unexpected") || strings.Contains(msg, "JSON") ||
		strings.Contains(msg, "syntax") {
		return fmt.Sprintf("restore from the newest %s.bak-* backup or fix the JSON by hand, then rerun amq-keepalive install-hook --agent %s", path, agent)
	}
	return fmt.Sprintf("rerun amq-keepalive install-hook --agent %s", agent)
}
