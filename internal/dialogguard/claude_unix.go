//go:build darwin || linux

package dialogguard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/hookfile"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// claudeHookTools are the tools whose dialog opens in PreToolUse and closes
// in PostToolUse or PostToolUseFailure.
const claudeHookTools = "AskUserQuestion|ExitPlanMode"

// ClaudeEvents are the events the installer wires, in order: the
// closing events first, so an install that fails part way never leaves an
// open without its close. Only long-standing events: a settings file with an
// event an older Claude Code does not know could cost every other hook in it.
var ClaudeEvents = []string{
	"PostToolUse",
	"PostToolUseFailure",
	"UserPromptSubmit",
	"Stop",
	"SubagentStop",
	"SessionStart",
	"SessionEnd",
	"PreToolUse",
	"PermissionRequest",
}

// claudeEventFloor is the first Claude Code version known to have
// each gated event, per the Claude Code changelog: PostToolUseFailure in
// 2.1.119, and agent_id in SubagentStop input in 2.0.42 (SubagentStop
// closes nothing without it). Before 2.1.101 an unknown event made Claude
// Code ignore the whole settings file, so the installer adds a gated event
// only for a version at or past its floor.
var claudeEventFloor = map[string]ClaudeVersion{
	"PostToolUseFailure": {2, 1, 119},
	"SubagentStop":       {2, 0, 42},
}

// ClaudeVersion is a Claude Code major.minor.patch.
type ClaudeVersion [3]int

func (v ClaudeVersion) less(floor ClaudeVersion) bool {
	for i := range v {
		if v[i] != floor[i] {
			return v[i] < floor[i]
		}
	}
	return false
}

var claudeVersionRe = regexp.MustCompile(`^\s*(\d+)\.(\d+)\.(\d+)`)

// InstalledClaudeVersion reads `claude --version` from the claude on PATH,
// through harness.ProbeVersion: bounded output, and the process group killed
// and reaped at the deadline, so a launcher's child that keeps the pipe open
// cannot stall the installer or doctor. ok is false when it is missing,
// fails, or prints no version.
func InstalledClaudeVersion() (ClaudeVersion, bool) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return ClaudeVersion{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, ok := harness.ProbeVersion(ctx, path)
	if !ok {
		return ClaudeVersion{}, false
	}
	match := claudeVersionRe.FindStringSubmatch(out)
	if match == nil {
		return ClaudeVersion{}, false
	}
	var v ClaudeVersion
	for i := range v {
		if v[i], err = strconv.Atoi(match[i+1]); err != nil {
			return ClaudeVersion{}, false
		}
	}
	return v, true
}

// ClaudeEventsFor is ClaudeEvents without the events the
// given Claude Code version may not know; an unknown version gets none of
// the gated events.
func ClaudeEventsFor(version ClaudeVersion, known bool) []string {
	events := make([]string, 0, len(ClaudeEvents))
	for _, event := range ClaudeEvents {
		if floor, gated := claudeEventFloor[event]; gated && (!known || version.less(floor)) {
			continue
		}
		events = append(events, event)
	}
	return events
}

// HookCommand is the shell command one hook runs: the marker, a
// POSIX sh gate, the single-quoted binary, and a trailing || true. The gate
// repeats what the receiver checks, so a gated call leaves state as the
// receiver would and starts no amq: without AM_ME the receiver does nothing,
// and a close has nothing to remove when the agent directory at AM_ROOT holds
// no marker. The close skips only when the shell inspects exactly the
// directory the receiver opens: AM_ROOT is absolute, has no "." or ".."
// segment (sh resolves those through symlinks, filepath.Clean lexically),
// and ends in a character that TrimSpace keeps; AM_ME holds only handle
// characters. Any doubt (AM_ROOT unset, relative, padded, or naming no agent
// directory) runs the receiver, which resolves the root itself. Claude Code
// reads exit 2 as a blocking error for PreToolUse, UserPromptSubmit and Stop,
// and an older amq at this path would exit 2 on the unknown subcommand.
// harnessFlag is the receiver's flags before verb, each followed by a space:
// claudeDialogHookHostFlag for a Claude Code open, or a --harness flag.
func HookCommand(bin, harnessFlag, verb string) string {
	gate := `[ -z "${AM_ME-}" ] || `
	if verb == "close" {
		gate += `{ [ -z "${AMQ_CLAUDE_NATIVE_LAUNCH-}" ] && ` + NoMarkerTest + `; } || `
	}
	return HookMarker + gate + SingleQuote(bin) +
		" --no-update-check wake dialog " + harnessFlag + verb + " || true"
}

// SingleQuote always single-quotes s for a POSIX shell.
func SingleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// claudeDialogHookHostFlag passes a Claude Code open the pid of the process
// that ran the hook: the $PPID of its sh -c is Claude Code.
const claudeDialogHookHostFlag = `--host-pid "$PPID" `

// claudeDialogHookCommand is HookCommand of the Claude Code hook
// for verb; an open names its host.
func claudeDialogHookCommand(bin, verb string) string {
	flags := ""
	if verb == "open" {
		flags = claudeDialogHookHostFlag
	}
	return HookCommand(bin, flags, verb)
}

// NoMarkerTest succeeds only when AM_ROOT and AM_ME name exactly
// the agent directory the receiver opens and it holds no marker; see
// HookCommand.
const NoMarkerTest = `{ case "${AM_ROOT-}" in */./*|*/../*|*/.|*/..) false ;; /*[[:alnum:]._/-]) ;; *) false ;; esac && ` +
	`case "$AM_ME" in *[![:alnum:]_-]*) false ;; esac && ` +
	`[ -d "$AM_ROOT/agents/$AM_ME" ] && ` +
	`[ ! -e "$AM_ROOT/agents/$AM_ME/` + FileName + `" ] && ` +
	`[ ! -L "$AM_ROOT/agents/$AM_ME/` + FileName + `" ]; }`

// ClaudeBinRe reads the binary back from claudeDialogHookCommand,
// and from its form without the host flag, which doctor reports as stale.
var ClaudeBinRe = regexp.MustCompile(`'((?:[^']|'\\'')*)' --no-update-check wake dialog (?:--host-pid "\$PPID" )?(?:open|close) \|\| true$`)

// ClaudeHookVerb is the receiver verb the installer wires to event.
func ClaudeHookVerb(event string) string {
	if event == "PreToolUse" || event == "PermissionRequest" {
		return "open"
	}
	return "close"
}

// ClaudeHooks are the Claude Code guard hooks for events, run by the amq
// binary bin.
func ClaudeHooks(bin string, events []string) []hookfile.CommandHook {
	hooks := make([]hookfile.CommandHook, 0, len(events))
	for _, event := range events {
		hook := hookfile.CommandHook{
			Event:   event,
			Marker:  HookMarker,
			Command: claudeDialogHookCommand(bin, ClaudeHookVerb(event)),
			Timeout: 5,
		}
		switch event {
		case "PreToolUse", "PostToolUse", "PostToolUseFailure":
			hook.Matcher = claudeHookTools
		case "SessionEnd":
			// SessionEnd hooks share a 1.5 s exit budget that a longer
			// timeout raises for every hook.
			hook.Timeout = 1
		}
		hooks = append(hooks, hook)
	}
	return hooks
}

// ClaudeAt reports the wake dialog guard in the Claude settings
// file settings: its state and, when it is not ok, why and the remedy. It
// gives amq doctor's verdict: the events it requires follow the installed
// Claude Code version, read once per process.
func ClaudeAt(settings string) (state, detail string) {
	version, known := installedClaudeVersionOnce()
	check := CheckClaudeAt(settings, version, known)
	return check.State, check.Message
}

// InstallClaudeAt installs or migrates the wake dialog guard,
// run by the amq binary bin, in the Claude settings file settings, with
// the events the installed Claude Code version knows. It reports whether
// it changed the file.
func InstallClaudeAt(settings, bin string) (bool, error) {
	bin = installedGuardBin(settings, ClaudeEvents, ClaudeBinRe, bin)
	return hookfile.InstallCommandHooksAt(settings, ClaudeHooks(bin, ClaudeEventsFor(installedClaudeVersionOnce())))
}

// installedGuardBin is claude.KeptHookBin of the installed guard entries in
// the hooks file path (Codex never asks for trust again for nothing).
func installedGuardBin(path string, events []string, binRe *regexp.Regexp, bin string) string {
	var all []string
	for _, event := range events {
		commands, err := hookfile.MarkedHookCommandsAt(path, event, HookMarker)
		if err != nil {
			return bin
		}
		all = append(all, commands...)
	}
	return claude.KeptHookBin(all, binRe, bin)
}

// installedClaudeVersionOnce is InstalledClaudeVersion, run once per
// process: one doctor run checks many settings files.
var installedClaudeVersionOnce = sync.OnceValues(InstalledClaudeVersion)

// CheckClaudeAt checks the dialog guard in the settings file at path for a
// known or unknown Claude Code version. An unknown version needs only the
// ungated events, so the check runs without starting claude.
func CheckClaudeAt(path string, version ClaudeVersion, known bool) Check {
	type installed struct{ event, command string }
	var commands []installed
	var missing []string
	supported := make(map[string]bool)
	for _, event := range ClaudeEventsFor(version, known) {
		supported[event] = true
	}
	for _, event := range ClaudeEvents {
		found, err := hookfile.MarkedHookCommandsAt(path, event, HookMarker)
		if err != nil {
			return Check{State: Unreadable, Message: fmt.Sprintf("read Claude settings: %v", err)}
		}
		if len(found) == 0 && supported[event] {
			missing = append(missing, event)
		}
		for _, command := range found {
			commands = append(commands, installed{event, command})
		}
	}
	if len(commands) == 0 {
		return Check{State: Missing, Message: "not installed: a wake doorbell can answer an open Claude Code dialog; remedy: amq wake install-dialog-hook"}
	}
	if len(missing) > 0 {
		return Check{State: Incomplete, Message: fmt.Sprintf("partly installed, missing %s; remedy: amq wake install-dialog-hook", strings.Join(missing, ", "))}
	}
	if disabled, err := claude.HooksDisabledAt(path); err != nil {
		return Check{State: Unreadable, Message: fmt.Sprintf("read Claude settings: %v", err)}
	} else if disabled {
		return Check{State: Disabled, Message: "installed, but the user settings set disableAllHooks, so Claude Code does not run it and the wake rings a plain doorbell"}
	}
	for _, hook := range commands {
		match := ClaudeBinRe.FindStringSubmatch(hook.command)
		if match == nil {
			return Check{State: Incomplete, Message: fmt.Sprintf("stale form on %s; remedy: amq wake install-dialog-hook", hook.event)}
		}
		bin := strings.ReplaceAll(match[1], `'\''`, `'`)
		if _, err := os.Stat(bin); err != nil {
			return Check{State: Incomplete, Message: fmt.Sprintf("the hook runs a missing amq binary %s; remedy: amq wake install-dialog-hook", bin)}
		}
		if hook.command != claudeDialogHookCommand(bin, ClaudeHookVerb(hook.event)) {
			return Check{State: Stale, Message: fmt.Sprintf("stale form on %s; remedy: amq wake install-dialog-hook", hook.event)}
		}
	}
	return Check{State: OK, Message: "installed"}
}
