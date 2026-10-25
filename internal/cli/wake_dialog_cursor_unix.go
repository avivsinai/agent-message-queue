//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// The Cursor approval guard. Cursor runs no hook when its approval prompt
// opens: it maps Claude's PermissionRequest and Notification to nothing. It
// awaits preToolUse before it decides whether to ask, so the receiver opens
// the dialog marker on every tool call, and the wake holds its doorbell
// from that call to the end of the turn. Only a turn or session event
// closes it: a matching stop, sessionStart or
// sessionEnd. A postToolUse cannot: parallel calls share no prompt token,
// and Cursor reports a rejected call as a postToolUse too. Prompts Cursor
// shows before any tool call (ask-question, mode switch, web search, image
// approval) open no marker.

// cursorDialogHookEvents are the events the installer wires, closes first
// so an install that stops part way never leaves an open without its close.
// Cursor refuses its whole hooks file for an event it does not know.
var cursorDialogHookEvents = []string{
	"stop",
	"sessionStart",
	"sessionEnd",
	"beforeSubmitPrompt", // startup only; an in-turn submission can be queued
	"preToolUse",
}

// cursorDialogCloseMatches runs inside the marker lock. Completed stops
// are awaited by Cursor, so their order is usable when no generation ID is
// supplied. Abort/error stops run in the background and need an exact turn.
func cursorDialogCloseMatches(marker wakeDialogMarker, in wakeDialogHookInput) bool {
	if in.emptyInput {
		return true // explicit operator close
	}
	if marker.SessionID != "" && in.SessionID != "" && marker.SessionID != in.SessionID {
		return false
	}
	if marker.Event == wakeDialogStartupEvent {
		return in.Event == "sessionStart" || in.Event == "beforeSubmitPrompt" || in.Event == "sessionEnd"
	}
	switch in.Event {
	case "sessionStart", "sessionEnd":
		return true
	case "stop":
		if marker.TurnID != "" && in.GenerationID != "" {
			return marker.TurnID == in.GenerationID
		}
		return in.Status == "completed"
	}
	return false
}

// cursorDialogHookFlag puts --harness cursor in every Cursor command. Cursor
// also runs the hooks of ~/.claude/settings.json and drops one of two
// identical commands, so a Cursor entry must not read as the Claude one.
const cursorDialogHookFlag = "--harness cursor "

// cursorDialogHookBinRe reads the binary back from a Cursor command.
var cursorDialogHookBinRe = regexp.MustCompile(`'((?:[^']|'\\'')*)' --no-update-check wake dialog --harness cursor (?:open|close) \|\| true; printf '\{\}'$`)

func cursorDialogHookVerb(event string) string {
	if event == "preToolUse" {
		return "open"
	}
	return "close"
}

// cursorDialogHookCommand is wakeDialogHookCommand with --harness cursor,
// followed by an unconditional printf '{}'. Cursor fails a hook whose stdout
// is not JSON, and {} is valid for every wired event and carries no
// permission, follow-up or environment verdict. It prints on the gated path
// too, where no amq runs.
func cursorDialogHookCommand(bin, verb string) string {
	return wakeDialogHookCommand(bin, cursorDialogHookFlag, verb) + "; printf '{}'"
}

// cursorDialogHooks are the Cursor entries. Cursor runs a command in the
// agent's environment, so the shell gate and the receiver read AM_ME and
// AM_ROOT as for Claude Code; exit 2 from an older amq would deny a tool
// call, so the amq call ends in || true before the printf.
func cursorDialogHooks(bin string) []claude.CommandHook {
	hooks := make([]claude.CommandHook, 0, len(cursorDialogHookEvents))
	for _, event := range cursorDialogHookEvents {
		hooks = append(hooks, claude.CommandHook{
			Event:   event,
			Marker:  wakeDialogHookMarker,
			Command: cursorDialogHookCommand(bin, cursorDialogHookVerb(event)),
			Timeout: 5,
		})
	}
	return hooks
}

// cursorHooksPath is the user hooks file Cursor reads: ~/.cursor/hooks.json.
func cursorHooksPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "hooks.json"), nil
}

func installCursorDialogHook(bin string, dryRun bool) error {
	hooks := cursorDialogHooks(bin)
	if dryRun {
		return printDialogHooks(hooks)
	}
	path, err := cursorHooksPath()
	if err != nil {
		return fmt.Errorf("resolve home for the Cursor dialog hook: %w", err)
	}
	if err := claude.InstallCursorCommandHooksAt(path, hooks); err != nil {
		return err
	}
	return writeStdout("dialog hook installed in %s\n"+
		"The guard needs AM_ME and AM_ROOT in the Cursor agent environment (amq coop exec\n"+
		"and amq launch set them). Cursor reads hooks.json when a session starts: restart\n"+
		"running Cursor sessions.\n", path)
}

func uninstallCursorDialogHook() error {
	path, err := cursorHooksPath()
	if err != nil {
		return fmt.Errorf("resolve home for the Cursor dialog hook: %w", err)
	}
	if err := claude.UninstallCursorCommandHooksAt(path, wakeDialogHookMarker, cursorDialogHookEvents); err != nil {
		return err
	}
	return writeStdout("dialog hook removed from %s\n", path)
}

// checkCursorDialogHook reports, read only, why the Cursor approval guard
// would not run when ~/.cursor exists: a hooks file Cursor ignores (no
// positive integer version), entries missing, a missing binary, or an
// older command.
func checkCursorDialogHook() (doctorCheck, bool) {
	path, err := cursorHooksPath()
	if err != nil {
		return doctorCheck{}, false
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
		return doctorCheck{}, false
	}
	check := doctorCheck{Name: "Cursor dialog hook", Status: "warn"}
	remedy := "remedy: amq wake install-dialog-hook --harness cursor"
	type installed struct{ event, command string }
	var commands []installed
	var missing []string
	for _, event := range cursorDialogHookEvents {
		found, err := claude.MarkedCursorHookCommandsAt(path, event, wakeDialogHookMarker)
		if err != nil {
			check.Message = fmt.Sprintf("read %s: %v", path, err)
			return check, true
		}
		if len(found) == 0 {
			missing = append(missing, event)
		}
		for _, command := range found {
			commands = append(commands, installed{event, command})
		}
	}
	switch {
	case len(commands) == 0:
		check.Message = "not installed: a wake doorbell can answer a Cursor approval prompt; " + remedy
		return check, true
	case len(missing) > 0:
		check.Message = fmt.Sprintf("partly installed, missing %s; %s", strings.Join(missing, ", "), remedy)
		return check, true
	}
	if raw, err := os.ReadFile(path); err != nil {
		check.Message = fmt.Sprintf("read %s: %v", path, err)
		return check, true
	} else if err := claude.ValidateCursorHooks(raw); err != nil {
		check.Message = fmt.Sprintf("Cursor ignores %s: %v; %s", path, err, remedy)
		return check, true
	}
	for _, hook := range commands {
		match := cursorDialogHookBinRe.FindStringSubmatch(hook.command)
		if match == nil {
			check.Message = fmt.Sprintf("stale form on %s; %s", hook.event, remedy)
			return check, true
		}
		bin := strings.ReplaceAll(match[1], `'\''`, `'`)
		if _, err := os.Stat(bin); err != nil {
			check.Message = fmt.Sprintf("the hook runs a missing amq binary %s; %s", bin, remedy)
			return check, true
		}
		if hook.command != cursorDialogHookCommand(bin, cursorDialogHookVerb(hook.event)) {
			check.Message = fmt.Sprintf("stale form on %s; %s", hook.event, remedy)
			return check, true
		}
	}
	check.Status = "ok"
	check.Message = "installed"
	return check, true
}
