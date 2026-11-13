package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/claudeidentity"
	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/harness"
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

// shellQuote quotes s as one word for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// coopHookEvents are the events amq coop install-hooks wires.
var coopHookEvents = []string{"SessionStart", "Stop"}

// coopHookCommand is the shell command one co-op hook runs. The gate runs
// in the harness shell, so a session that does not take part starts no
// amq: a participant has AM_ME (amq coop exec set it) or a binding file in
// store (amq coop join wrote it) named by its Claude session id or by the
// id of the chat a host application runs it for. The gate only keeps other
// sessions from starting amq; amq decides which binding is the session's. On SessionStart
// exec replaces that shell with amq: exit 2 from an older amq there only
// shows a notice. On Stop exit 2 blocks the stop, and an older amq at this
// path would exit 2 on the unknown subcommand, so the shell stays to turn
// that into exit 0 with || true.
//
// The waiter rings with its own exit code, and the shell turns only that
// code into the 2 that wakes the session: a usage error of an older amq
// also exits 2, and as a ring it would start a turn at every turn end.
func coopHookCommand(bin, store, verb string) string {
	bound := func(env string) string { return `[ -e ` + shellQuote(store+"/") + `"${` + env + `-}".json ]` }
	gate := coopHookMarker + `[ -n "${AM_ME-}" ] || ` + bound(envClaudeSession) + ` || ` + bound(envClaudeHostSession) + ` || exit 0; `
	amq := shellQuote(bin) + " --no-update-check coop hook " + verb
	switch verb {
	case "stop":
		return gate + amq + " || true"
	case "wait":
		return gate + amq + "; [ $? -ne " + strconv.Itoa(harness.ClaudeRewakeRingExit) + " ] || exit 2"
	}
	return gate + "exec " + amq
}

// ownsCoopHook returns the test for the co-op commands an entry replaces:
// the waiter's entries (wait) or the other co-op entries. An event chain
// holds one of each.
func ownsCoopHook(wait bool) func(command string) bool {
	return func(command string) bool {
		return strings.HasPrefix(command, coopHookMarker) && strings.Contains(command, " coop hook wait;") == wait
	}
}

// coopWaitTimeout is the waiter's hook timeout in seconds. Claude Code
// ends the hook at its timeout, and the session then has no doorbell until
// its next turn end or start, so it is long: 7 days.
const coopWaitTimeout = 7 * 24 * 60 * 60

func coopHooks(bin, store string) []hookfile.CommandHook {
	return []hookfile.CommandHook{
		{
			Event:   "SessionStart",
			Matcher: coopSessionStartSources,
			Marker:  coopHookMarker,
			Command: coopHookCommand(bin, store, "session-start"),
			Timeout: 2,
			Owns:    ownsCoopHook(false),
		},
		{
			Event:   "Stop",
			Marker:  coopHookMarker,
			Command: coopHookCommand(bin, store, "stop"),
			Timeout: 2,
			Owns:    ownsCoopHook(false),
		},
		coopWaitHook(bin, store, "SessionStart", coopSessionStartSources),
		coopWaitHook(bin, store, "Stop", ""),
	}
}

// coopWaitHook is the waiter's entry in one event chain. A session start
// arms it for a session that waits for its first turn; every turn end arms
// it again.
func coopWaitHook(bin, store, event, matcher string) hookfile.CommandHook {
	return hookfile.CommandHook{
		Event:       event,
		Matcher:     matcher,
		Marker:      coopHookMarker,
		Command:     coopHookCommand(bin, store, "wait"),
		Timeout:     coopWaitTimeout,
		AsyncRewake: true,
		Owns:        ownsCoopHook(true),
	}
}

// codexCoopWaitCommand is the command of the Codex wait hook. The gate
// starts no amq unless the shell has AM_ME or a Codex thread was ever bound
// (amq coop join or coop exec wrote the store's threads directory). The
// shell stays as the parent and ends in || true: exit 2 means something to
// Codex on Stop, and an older amq at this path exits 2 on the unknown flag.
func codexCoopWaitCommand(bin, threads string) string {
	return coopHookMarker + `[ -n "${AM_ME-}" ] || [ -d ` + shellQuote(threads) + ` ] || exit 0; ` +
		shellQuote(bin) + " --no-update-check coop hook --harness codex wait || true"
}

// codexCoopHooks are the Codex wait hook's entries: a session start arms
// it for a chat that waits for its first turn, and every turn end arms it
// again. Codex runs an async hook in the background for the thread.
func codexCoopHooks(bin, threads string) []hookfile.CommandHook {
	hooks := make([]hookfile.CommandHook, 0, len(coopHookEvents))
	for _, event := range coopHookEvents {
		hooks = append(hooks, hookfile.CommandHook{
			Event:   event,
			Marker:  coopHookMarker,
			Command: codexCoopWaitCommand(bin, threads),
			Timeout: coopWaitTimeout,
			Async:   true,
		})
	}
	return hooks
}

// codexCoopHooksPath is the user hooks file Codex reads:
// $CODEX_HOME/hooks.json.
func codexCoopHooksPath() (string, error) {
	codexHome, err := codexHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(absPath(filepath.Clean(codexHome)), "hooks.json"), nil
}

// codexCoopHookTrustNote is printed after a Codex install: Codex skips a
// new or changed hook until the person approves it, and a chat opened
// before that does not run it.
const codexCoopHookTrustNote = "Codex runs the new hooks only after you approve them: open Settings > Hooks in the\n" +
	"Codex app (/hooks in the CLI) and approve the AMQ_COOP_HOOK entries, then start a\n" +
	"new chat and run amq coop join in it.\n"

// coopHookHarness reads --harness of install-hooks and uninstall-hooks.
func coopHookHarness(name string) (codex bool, err error) {
	switch name {
	case "claude":
		return false, nil
	case "codex":
		return true, nil
	}
	return false, UsageError("--harness must be claude or codex")
}

func runCoopInstallHooks(args []string) error {
	fs := flag.NewFlagSet("coop install-hooks", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "Print the hooks without changing the settings file")
	harnessName := fs.String("harness", "claude", "claude, or codex for the wait hook of a joined Codex chat")
	usage := usageWithFlags(fs, "amq coop install-hooks [--dry-run] [--harness claude|codex]",
		"Install the co-op hooks in $CLAUDE_CONFIG_DIR/settings.json (default",
		"~/.claude/settings.json), or with --harness codex the wait hook in",
		"$CODEX_HOME/hooks.json.",
		"",
		"SessionStart (startup, resume, clear, compact) gives the agent its co-op",
		"preamble: handle, session, peers, unread count, and what announces new mail.",
		"Stop blocks a stop once for unread mail that no wake doorbell will announce.",
		"The wait hook, on SessionStart and Stop, is the doorbell of a session that no",
		"wake rings, such as a session of the Claude Code desktop app: it runs in the",
		"background (asyncRewake) and wakes the session when mail arrives. It stops",
		"after 7 days without a turn; the next turn starts it again.",
		"",
		"The hooks run only in sessions that take part: sessions with AM_ME set, which",
		"amq coop exec and amq launch set, and sessions bound with amq coop join.",
		"Other Claude Code sessions start no amq.",
		"",
		"With --harness codex, the wait hook runs on SessionStart and Stop as an async",
		"Codex hook and is the doorbell of a chat bound with amq coop join: it queues",
		"the doorbell to the chat's thread when mail arrives. Codex runs it only after",
		"you approve it, in a chat opened after that. It stops after 7 days without",
		"a turn, or when Codex unloads the chat; the next turn starts it again.",
		"",
		"Running again keeps the same entries and replaces entries of another amq",
		"binary. Other settings and hooks are not changed. It also lists old project",
		"hook entries that run the co-op scripts; remove them, or both run.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	codex, err := coopHookHarness(*harnessName)
	if err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve the amq binary for the co-op hooks: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home for the co-op hooks: %w", err)
	}
	if codex {
		return installCodexCoopHooks(codexCoopHooks(bin, codexidentity.ThreadsDir(home)), *dryRun)
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

// installCodexCoopHooks writes the Codex wait hook's entries after the
// person's own, so the trust keys of their hooks do not move.
func installCodexCoopHooks(hooks []hookfile.CommandHook, dryRun bool) error {
	if dryRun {
		for _, hook := range hooks {
			if err := writeStdout("%s\t(all)\ttimeout=%ds async\t%s\n", hook.Event, hook.Timeout, hook.Command); err != nil {
				return err
			}
		}
		return nil
	}
	path, err := codexCoopHooksPath()
	if err != nil {
		return err
	}
	if _, err := hookfile.InstallCommandHooksAt(path, hooks); err != nil {
		return err
	}
	return writeStdout("co-op wait hook installed in %s\n%s", path, codexCoopHookTrustNote)
}

func runCoopUninstallHooks(args []string) error {
	fs := flag.NewFlagSet("coop uninstall-hooks", flag.ContinueOnError)
	harnessName := fs.String("harness", "claude", "claude, or codex")
	usage := usageWithFlags(fs, "amq coop uninstall-hooks [--harness claude|codex]",
		"Remove the co-op hooks (AMQ_COOP_HOOK entries) from",
		"$CLAUDE_CONFIG_DIR/settings.json (default ~/.claude/settings.json), or with",
		"--harness codex from $CODEX_HOME/hooks.json.",
		"Other settings and hooks are not changed.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	codex, err := coopHookHarness(*harnessName)
	if err != nil {
		return err
	}
	if codex {
		path, err := codexCoopHooksPath()
		if err != nil {
			return err
		}
		if err := hookfile.UninstallCommandHooksAt(path, coopHookMarker, coopHookEvents); err != nil {
			return err
		}
		return writeStdout("co-op wait hook removed from %s\n", path)
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
