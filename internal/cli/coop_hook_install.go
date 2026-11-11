package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/claudeidentity"
	"github.com/avivsinai/agent-message-queue/internal/hookfile"
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
var coopHookEvents = []string{"SessionStart", "Stop"}

// coopHookCommand is the shell command one co-op hook runs. The gate runs
// in the harness shell, so a session that does not take part starts no
// amq: a participant has AM_ME (amq coop exec set it) or a binding file in
// store for its Claude session (amq coop join wrote it). On SessionStart
// exec replaces that shell with amq: exit 2 from an older amq there only
// shows a notice. On Stop exit 2 blocks the stop, and an older amq at this
// path would exit 2 on the unknown subcommand, so the shell stays to turn
// that into exit 0 with || true.
func coopHookCommand(bin, store, verb string) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	gate := coopHookMarker + `[ -n "${AM_ME-}" ] || [ -e ` + quote(store+"/") + `"${` + envClaudeSession + `-}".json ] || exit 0; `
	amq := quote(bin) + " --no-update-check coop hook " + verb
	if verb == "stop" {
		return gate + amq + " || true"
	}
	return gate + "exec " + amq
}

func coopHooks(bin, store string) []hookfile.CommandHook {
	return []hookfile.CommandHook{
		{
			Event:   "SessionStart",
			Matcher: coopSessionStartSources,
			Marker:  coopHookMarker,
			Command: coopHookCommand(bin, store, "session-start"),
			Timeout: 2,
		},
		{
			Event:   "Stop",
			Marker:  coopHookMarker,
			Command: coopHookCommand(bin, store, "stop"),
			Timeout: 2,
		},
	}
}

func runCoopInstallHooks(args []string) error {
	fs := flag.NewFlagSet("coop install-hooks", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "Print the hooks without changing the settings file")
	usage := usageWithFlags(fs, "amq coop install-hooks [--dry-run]",
		"Install the co-op hooks in $CLAUDE_CONFIG_DIR/settings.json (default",
		"~/.claude/settings.json).",
		"",
		"SessionStart (startup, resume, clear, compact) gives the agent its co-op",
		"preamble: handle, session, peers, unread count, and what announces new mail.",
		"Stop blocks a stop once for unread mail that no wake doorbell will announce.",
		"",
		"The hooks run only in sessions that take part: sessions with AM_ME set, which",
		"amq coop exec and amq launch set, and sessions bound with amq coop join.",
		"Other Claude Code sessions start no amq.",
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
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home for the co-op hooks: %w", err)
	}
	hooks := coopHooks(bin, claudeidentity.SessionsDir(home))
	if *dryRun {
		for _, hook := range hooks {
			matcher := hook.Matcher
			if matcher == "" {
				matcher = "(all)"
			}
			if err := writeStdout("%s\t%s\t%s\n", hook.Event, matcher, hook.Command); err != nil {
				return err
			}
		}
		return writeLegacyCoopHookEntries()
	}
	if err := claude.InstallCommandHooks(home, hooks); err != nil {
		return err
	}
	if err := writeStdout("co-op hooks installed in %s\n", claude.SettingsPath(home)); err != nil {
		return err
	}
	return writeLegacyCoopHookEntries()
}

func runCoopUninstallHooks(args []string) error {
	fs := flag.NewFlagSet("coop uninstall-hooks", flag.ContinueOnError)
	usage := usageWithFlags(fs, "amq coop uninstall-hooks",
		"Remove the co-op hooks (AMQ_COOP_HOOK entries) from",
		"$CLAUDE_CONFIG_DIR/settings.json (default ~/.claude/settings.json).",
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
	return writeStdout("co-op hooks removed from %s\n", claude.SettingsPath(home))
}

// writeLegacyCoopHookEntries lists the project hook entries under the
// current directory that still run the old co-op scripts.
func writeLegacyCoopHookEntries() error {
	entries, err := legacyCoopHookEntries(".")
	if err != nil {
		return writeStderr("warning: %v\n", err)
	}
	if len(entries) == 0 {
		return nil
	}
	if err := writeStdout("Remove these project hook entries; they run co-op scripts AMQ no longer ships:\n"); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := writeStdout("  %s\n", entry); err != nil {
			return err
		}
	}
	return nil
}
