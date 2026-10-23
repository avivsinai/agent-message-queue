package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

const (
	// coopHookMarker starts every co-op hook command; uninstall and doctor
	// find AMQ's entries by it.
	coopHookMarker = "AMQ_COOP_HOOK=1 "
	// coopSessionStartSources skips fork: a fork keeps its parent's context.
	coopSessionStartSources = "startup|resume|clear|compact"
)

// coopHookEvents are the events amq coop install-hooks wires.
var coopHookEvents = []string{"SessionStart"}

// coopHookCommand is the shell command one co-op hook runs. The AM_ME test
// runs in the harness shell, so a session that does not take part starts
// no amq. exec replaces that shell with amq. There is no || true: on
// SessionStart, exit 2 from an older amq only shows a notice and the session
// goes on.
func coopHookCommand(bin, verb string) string {
	return coopHookMarker + `[ -z "${AM_ME-}" ] || exec ` +
		"'" + strings.ReplaceAll(bin, "'", `'\''`) + "'" +
		" --no-update-check coop hook " + verb
}

func coopHooks(bin string) []claude.CommandHook {
	return []claude.CommandHook{{
		Event:   "SessionStart",
		Matcher: coopSessionStartSources,
		Marker:  coopHookMarker,
		Command: coopHookCommand(bin, "session-start"),
		Timeout: 2,
	}}
}

func runCoopInstallHooks(args []string) error {
	fs := flag.NewFlagSet("coop install-hooks", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "Print the hooks without changing the settings file")
	usage := usageWithFlags(fs, "amq coop install-hooks [--dry-run]",
		"Install the co-op hooks in ~/.claude/settings.json.",
		"",
		"SessionStart (startup, resume, clear, compact) gives the agent its co-op",
		"preamble: handle, session, peers, unread count, and whether a wake rings the",
		"terminal. The hooks run only in sessions with AM_ME set, which amq coop exec",
		"and amq launch set; other Claude Code sessions start no amq.",
		"",
		"Running again keeps the same entries and replaces entries of another amq",
		"binary. Other settings and hooks are not changed. It also lists old project",
		"hook entries that run the co-op scripts; remove them, or both run.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve the amq binary for the co-op hooks: %w", err)
	}
	hooks := coopHooks(bin)
	if *dryRun {
		for _, hook := range hooks {
			if err := writeStdout("%s\t%s\t%s\n", hook.Event, hook.Matcher, hook.Command); err != nil {
				return err
			}
		}
		return writeLegacyCoopHookEntries()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home for the co-op hooks: %w", err)
	}
	if err := claude.InstallCommandHooks(home, hooks); err != nil {
		return err
	}
	if err := writeStdout("co-op hooks installed in %s\n", filepath.Join(home, ".claude", "settings.json")); err != nil {
		return err
	}
	return writeLegacyCoopHookEntries()
}

func runCoopUninstallHooks(args []string) error {
	fs := flag.NewFlagSet("coop uninstall-hooks", flag.ContinueOnError)
	usage := usageWithFlags(fs, "amq coop uninstall-hooks",
		"Remove the co-op hooks (AMQ_COOP_HOOK entries) from ~/.claude/settings.json.",
		"Other settings and hooks are not changed.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home for the co-op hooks: %w", err)
	}
	if err := claude.UninstallCommandHooks(home, coopHookMarker, coopHookEvents); err != nil {
		return err
	}
	return writeStdout("co-op hooks removed from %s\n", filepath.Join(home, ".claude", "settings.json"))
}

// writeLegacyCoopHookEntries lists the project hook entries under the
// current directory that still run the co-op scripts.
func writeLegacyCoopHookEntries() error {
	entries, err := legacyCoopHookEntries(".")
	if err != nil {
		return writeStderr("warning: %v\n", err)
	}
	if len(entries) == 0 {
		return nil
	}
	if err := writeStdout("Remove these project hook entries; they run the co-op scripts a second time:\n"); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := writeStdout("  %s\n", entry); err != nil {
			return err
		}
	}
	return nil
}
