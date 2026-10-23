//go:build darwin || linux

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"golang.org/x/sys/unix"
)

const (
	// wakeDialogHookMarker starts every hook command the installer writes;
	// uninstall and doctor find AMQ's entries by it.
	wakeDialogHookMarker = "AMQ_DIALOG_HOOK=1 "
	// wakeDialogHookTools are the tools whose dialog opens in PreToolUse and
	// closes in PostToolUse.
	wakeDialogHookTools = "AskUserQuestion|ExitPlanMode"
	// ExitPlanMode carries the whole plan in its hook input.
	maxWakeDialogHookInputBytes = 8 << 20
)

// wakeDialogHookEvents are the events the installer wires, in order: the
// closing events first, so an install that fails part way never leaves an
// open without its close. Only long-standing events: a settings file with an
// event an older Claude Code does not know could cost every other hook in it.
var wakeDialogHookEvents = []string{
	"PostToolUse",
	"UserPromptSubmit",
	"Stop",
	"SessionStart",
	"SessionEnd",
	"PreToolUse",
	"PermissionRequest",
}

// wakeDialogHookInput is the part of Claude Code's hook input the receiver
// reads.
type wakeDialogHookInput struct {
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	Tool      string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
	Cwd       string `json:"cwd"`
	// TurnID is the Codex turn (wake_dialog_codex_unix.go).
	TurnID string `json:"turn_id"`
}

// wakeDialogMarker is the .wake.dialog content. The wake uses only the
// file's presence and mtime; tool_use_id scopes a PostToolUse close and the
// rest is for diagnosis.
type wakeDialogMarker struct {
	Schema    int    `json:"schema"`
	Event     string `json:"event,omitempty"`
	Harness   string `json:"harness,omitempty"`
	Tool      string `json:"tool,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	// TurnID is the Codex turn that asked, for diagnosis.
	TurnID    string `json:"turn_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	OpenedAt  string `json:"opened_at"`
}

// wakeDialogSourceInDir is the running wake's dialog source. It reads only
// through the canonical agent directory; a detached directory cannot show
// the hook's marker, so it reads as unknown, never as closed.
func wakeDialogSourceInDir(agentDir *wakeAgentDir) func(time.Time) (wakeDialogState, error) {
	return func(now time.Time) (wakeDialogState, error) {
		if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
			return wakeDialogUnknown, err
		}
		var info os.FileInfo
		var exists bool
		var readErr error
		if err := agentDir.withFD(func(dirfd int) error {
			_, info, exists, readErr = readWakeRepairMetadataAt(
				dirfd,
				wakeDialogFileName,
				"wake dialog marker",
				filepath.Join(agentDir.path, wakeDialogFileName),
				maxWakeMetadataFileBytes,
			)
			return nil
		}); err != nil {
			return wakeDialogUnknown, err
		}
		var modTime time.Time
		if info != nil {
			modTime = info.ModTime()
		}
		return classifyWakeDialogMarker(modTime, exists, readErr, now)
	}
}

// runWakeDialog is the hook receiver. Errors after the arguments parse
// return nil: a hook must never block Claude Code, and a missing identity or
// mailbox only leaves today's behavior. It prints nothing on stdout, so it
// never decides a PreToolUse or PermissionRequest.
func runWakeDialog(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("wake dialog", flag.ContinueOnError)
	meFlag := fs.String("me", defaultMe(), "Agent handle (or AM_ME)")
	rootFlag := fs.String("root", "", "Root directory for the queue (default: AM_ROOT, else .amqrc discovery from the hook's cwd)")
	harness := fs.String("harness", "claude", "Hook input of claude or codex")
	usage := usageWithFlags(fs, "amq wake dialog [--me <agent>] [--root <path>] [--harness claude|codex] open|close",
		"Claude Code hook receiver for the wake dialog guard; amq wake install-dialog-hook",
		"installs it. Reads the hook input JSON on stdin.",
		"",
		"open writes <root>/agents/<me>/.wake.dialog; the wake writes no terminal input while",
		"it is present. close removes it; a close with a tool_use_id removes only the marker",
		"of that tool call, and a close with unreadable input keeps it. A close with empty",
		"stdin removes it. With --harness codex only a turn or session event removes it;",
		"without AM_* the seat is the one amq coop exec recorded for the hook's session_id.",
		"Always exits 0 once the arguments parse and prints nothing.")
	if handled, err := parseFlagsAllowPositionals(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "open" && fs.Arg(0) != "close") {
		return UsageError("wake dialog needs exactly one of open or close")
	}
	open := fs.Arg(0) == "open"
	codex := *harness == "codex"
	if !codex && *harness != "claude" {
		return UsageError("wake dialog --harness must be claude or codex")
	}

	var in wakeDialogHookInput
	inputErr := json.NewDecoder(io.LimitReader(stdin, maxWakeDialogHookInputBytes)).Decode(&in)
	if errors.Is(inputErr, io.EOF) {
		inputErr = nil
	}
	if !open && inputErr != nil {
		return nil
	}
	if codex && adoptCodexHookIdentity(in.SessionID) != nil {
		return nil
	}

	root := *rootFlag
	if root == "" {
		// Discovery reads cwd; the hook's cwd field is the session's.
		if strings.TrimSpace(os.Getenv(envRoot)) == "" && filepath.IsAbs(in.Cwd) {
			_ = os.Chdir(in.Cwd)
		}
		var err error
		if root, err = resolveDefaultRoot(); err != nil {
			return nil
		}
	}
	root = resolveRoot(root)
	if *meFlag == "" {
		*meFlag = defaultMe() // an adopted Codex identity
	}
	me, err := normalizeHandle(*meFlag)
	if err != nil {
		return nil
	}
	if err := guardMailboxContext("wake dialog", root, false, false, *rootFlag != ""); err != nil {
		return nil
	}
	agentDir, err := openExistingWakeAgentDir(root, me)
	if err != nil {
		return nil
	}
	defer func() { _ = agentDir.Close() }()
	_ = agentDir.withFD(func(dirfd int) error {
		switch {
		case codex && open:
			return writeCodexDialogMarker(dirfd, agentDir, in)
		case codex:
			return clearCodexDialogMarker(dirfd, agentDir, in)
		case open:
			return writeWakeDialogMarker(dirfd, agentDir, in)
		}
		return clearWakeDialogMarker(dirfd, agentDir, in.ToolUseID)
	})
	return nil
}

// writeWakeDialogMarker writes the marker for in. A marker without a
// tool_use_id (a permission dialog) stays without one, so a PostToolUse of
// a later tool call cannot clear the permission dialog's hold.
func writeWakeDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	if in.ToolUseID != "" {
		current, _, exists, err := readWakeRepairMetadataAt(
			dirfd,
			wakeDialogFileName,
			"wake dialog marker",
			filepath.Join(agentDir.path, wakeDialogFileName),
			maxWakeMetadataFileBytes,
		)
		var marker wakeDialogMarker
		if err == nil && exists && json.Unmarshal(current, &marker) == nil && marker.ToolUseID == "" {
			in.ToolUseID = ""
		}
	}
	raw, err := json.Marshal(wakeDialogMarker{
		Schema:    1,
		Event:     in.Event,
		Tool:      in.Tool,
		ToolUseID: in.ToolUseID,
		SessionID: in.SessionID,
		OpenedAt:  time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return replaceWakeDialogMarkerAt(dirfd, agentDir, append(raw, '\n'))
}

// replaceWakeDialogMarkerAt installs data as the marker by an exclusive
// temp file and a rename, without fsync or read-back: the marker is
// advisory, and a marker lost in a crash leaves the wake as it is without the
// hook. The open runs as the dialog renders, so it must not wait on the disk.
func replaceWakeDialogMarkerAt(dirfd int, agentDir *wakeAgentDir, data []byte) error {
	const label = "wake dialog marker"
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate %s temp name: %w", label, err)
	}
	temp := fmt.Sprintf(".%s.tmp.%d.%s", strings.TrimLeft(wakeDialogFileName, "."), os.Getpid(), hex.EncodeToString(nonce[:]))
	fd, err := unix.Openat(dirfd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s temp: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), temp)
	tempPresent := true
	defer func() {
		_ = file.Close()
		if tempPresent {
			_ = wakeUnlinkAt(dirfd, temp, 0)
		}
	}()
	if n, err := file.Write(data); err != nil {
		return fmt.Errorf("write %s temp: %w", label, err)
	} else if n != len(data) {
		return fmt.Errorf("write %s temp: %w", label, io.ErrShortWrite)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s temp: %w", label, err)
	}
	if err := validateWakeRepairMetadataDestinationAt(dirfd, agentDir, wakeDialogFileName, label); err != nil {
		return err
	}
	if err := wakeRenameAt(dirfd, temp, dirfd, wakeDialogFileName); err != nil {
		return fmt.Errorf("install %s: %w", label, err)
	}
	tempPresent = false
	return nil
}

// clearWakeDialogMarker removes the marker. With toolUseID (PostToolUse) it
// removes only the marker that tool call opened; a permission marker carries
// no tool_use_id and waits for a turn-level clear.
func clearWakeDialogMarker(dirfd int, agentDir *wakeAgentDir, toolUseID string) error {
	if toolUseID != "" {
		raw, _, exists, err := readWakeRepairMetadataAt(
			dirfd,
			wakeDialogFileName,
			"wake dialog marker",
			filepath.Join(agentDir.path, wakeDialogFileName),
			maxWakeMetadataFileBytes,
		)
		if err != nil || !exists {
			return err
		}
		var marker wakeDialogMarker
		if json.Unmarshal(raw, &marker) != nil || marker.ToolUseID != toolUseID {
			return nil
		}
	}
	if err := wakeUnlinkAt(dirfd, wakeDialogFileName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// wakeDialogHookCommand is the shell command one hook runs: the marker, a
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
func wakeDialogHookCommand(bin, verb string) string {
	gate := `[ -z "${AM_ME-}" ] || `
	if verb == "close" {
		gate += wakeDialogNoMarkerTest + ` || `
	}
	return wakeDialogHookMarker + gate +
		"'" + strings.ReplaceAll(bin, "'", `'\''`) + "'" +
		" --no-update-check wake dialog " + verb + " || true"
}

// wakeDialogNoMarkerTest succeeds only when AM_ROOT and AM_ME name exactly
// the agent directory the receiver opens and it holds no marker; see
// wakeDialogHookCommand.
const wakeDialogNoMarkerTest = `{ case "${AM_ROOT-}" in */./*|*/../*|*/.|*/..) false ;; /*[[:alnum:]._/-]) ;; *) false ;; esac && ` +
	`case "$AM_ME" in *[![:alnum:]_-]*) false ;; esac && ` +
	`[ -d "$AM_ROOT/agents/$AM_ME" ] && ` +
	`[ ! -e "$AM_ROOT/agents/$AM_ME/` + wakeDialogFileName + `" ] && ` +
	`[ ! -L "$AM_ROOT/agents/$AM_ME/` + wakeDialogFileName + `" ]; }`

// wakeDialogHookBinRe reads the binary back from wakeDialogHookCommand.
var wakeDialogHookBinRe = regexp.MustCompile(`'((?:[^']|'\\'')*)' --no-update-check wake dialog (?:open|close) \|\| true$`)

// wakeDialogHookVerb is the receiver verb the installer wires to event.
func wakeDialogHookVerb(event string) string {
	if event == "PreToolUse" || event == "PermissionRequest" {
		return "open"
	}
	return "close"
}

func wakeDialogHooks(bin string) []claude.CommandHook {
	hooks := make([]claude.CommandHook, 0, len(wakeDialogHookEvents))
	for _, event := range wakeDialogHookEvents {
		hook := claude.CommandHook{
			Event:   event,
			Marker:  wakeDialogHookMarker,
			Command: wakeDialogHookCommand(bin, wakeDialogHookVerb(event)),
			Timeout: 5,
		}
		switch event {
		case "PreToolUse", "PostToolUse":
			hook.Matcher = wakeDialogHookTools
		case "SessionEnd":
			// SessionEnd hooks share a 1.5 s exit budget that a longer
			// timeout raises for every hook.
			hook.Timeout = 1
		}
		hooks = append(hooks, hook)
	}
	return hooks
}

func runWakeInstallDialogHook(args []string) error {
	fs := flag.NewFlagSet("wake install-dialog-hook", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "Print the hooks without changing the settings file")
	harness := fs.String("harness", "claude", "claude, codex, or all (each harness whose home exists)")
	usage := usageWithFlags(fs, "amq wake install-dialog-hook [--dry-run] [--harness claude|codex|all]",
		"Install the wake dialog guard hooks in ~/.claude/settings.json, or with --harness",
		"codex in $CODEX_HOME/hooks.json (Codex runs them once you trust them in /hooks).",
		"",
		"While Claude Code shows AskUserQuestion, ExitPlanMode or a permission dialog,",
		"the hooks keep a marker in the agent's mailbox and every wake of that agent",
		"holds its doorbell, so injected input cannot answer the dialog. The hooks find",
		"the agent through AM_ME and AM_ROOT in the Claude Code environment, which",
		"amq coop exec and amq launch set; without them the guard does nothing.",
		"",
		"Running again keeps the same entries and replaces entries of another amq",
		"binary. Other settings and hooks are not changed. Running Claude Code",
		"sessions pick the change up through the settings watcher; if /hooks does not",
		"list the AMQ_DIALOG_HOOK entries, restart the session.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	claudeOn, codexOn, err := dialogHookHarnesses(*harness)
	if err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve the amq binary for the dialog hook: %w", err)
	}
	if codexOn {
		if err := installCodexDialogHook(bin, *dryRun); err != nil {
			return err
		}
	}
	if !claudeOn {
		return nil
	}
	hooks := wakeDialogHooks(bin)
	if *dryRun {
		return printDialogHooks(hooks)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home for the dialog hook: %w", err)
	}
	if err := claude.InstallCommandHooks(home, hooks); err != nil {
		return err
	}
	return writeStdout(
		"dialog hook installed in %s\n"+
			"The guard needs AM_ME and AM_ROOT in the Claude Code environment (amq coop exec\n"+
			"and amq launch set them). Running sessions pick it up through the settings\n"+
			"watcher; if /hooks does not list it, restart the session.\n",
		filepath.Join(home, ".claude", "settings.json"),
	)
}

// printDialogHooks prints the hooks an install would write, one per line.
func printDialogHooks(hooks []claude.CommandHook) error {
	for _, hook := range hooks {
		matcher := hook.Matcher
		if matcher == "" {
			matcher = "(all)"
		}
		if err := writeStdout("%s\t%s\ttimeout=%ds\t%s\n", hook.Event, matcher, hook.Timeout, hook.Command); err != nil {
			return err
		}
	}
	return nil
}

// dialogHookHarnesses reads --harness: claude, codex, or all, which picks
// each harness whose home directory exists.
func dialogHookHarnesses(harness string) (claudeOn, codexOn bool, err error) {
	switch harness {
	case "claude":
		return true, false, nil
	case "codex":
		return false, true, nil
	case "all":
		if home, err := os.UserHomeDir(); err == nil {
			info, err := os.Stat(filepath.Join(home, ".claude"))
			claudeOn = err == nil && info.IsDir()
		}
		if path, err := codexHooksPath(); err == nil {
			info, err := os.Stat(filepath.Dir(path))
			codexOn = err == nil && info.IsDir()
		}
		return claudeOn, codexOn, nil
	}
	return false, false, UsageError("--harness must be claude, codex or all")
}

func runWakeUninstallDialogHook(args []string) error {
	fs := flag.NewFlagSet("wake uninstall-dialog-hook", flag.ContinueOnError)
	harness := fs.String("harness", "claude", "claude, codex, or all (each harness whose home exists)")
	usage := usageWithFlags(fs, "amq wake uninstall-dialog-hook [--harness claude|codex|all]",
		"Remove the wake dialog guard hooks (AMQ_DIALOG_HOOK entries) from",
		"~/.claude/settings.json, or $CODEX_HOME/hooks.json with --harness codex.",
		"Other settings and hooks are not changed.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	claudeOn, codexOn, err := dialogHookHarnesses(*harness)
	if err != nil {
		return err
	}
	if codexOn {
		if err := uninstallCodexDialogHook(); err != nil {
			return err
		}
	}
	if !claudeOn {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home for the dialog hook: %w", err)
	}
	if err := claude.UninstallCommandHooks(home, wakeDialogHookMarker, wakeDialogHookEvents); err != nil {
		return err
	}
	return writeStdout("dialog hook removed from %s\n", filepath.Join(home, ".claude", "settings.json"))
}

// checkClaudeDialogHook reports a missing or partly installed dialog hook,
// or one whose amq binary is gone, when ~/.claude exists. It is read-only.
func checkClaudeDialogHook() (doctorCheck, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return doctorCheck{}, false
	}
	if info, err := os.Stat(filepath.Join(home, ".claude")); err != nil || !info.IsDir() {
		return doctorCheck{}, false
	}
	check := doctorCheck{Name: "Claude dialog hook", Status: "warn"}
	type installed struct{ event, command string }
	var commands []installed
	var missing []string
	for _, event := range wakeDialogHookEvents {
		found, err := claude.MarkedHookCommands(home, event, wakeDialogHookMarker)
		if err != nil {
			check.Message = fmt.Sprintf("read Claude settings: %v", err)
			return check, true
		}
		if len(found) == 0 {
			missing = append(missing, event)
		}
		for _, command := range found {
			commands = append(commands, installed{event, command})
		}
	}
	if len(commands) == 0 {
		check.Message = "not installed: a wake doorbell can answer an open Claude Code dialog; remedy: amq wake install-dialog-hook"
		return check, true
	}
	if len(missing) > 0 {
		check.Message = fmt.Sprintf("partly installed, missing %s; remedy: amq wake install-dialog-hook", strings.Join(missing, ", "))
		return check, true
	}
	for _, hook := range commands {
		match := wakeDialogHookBinRe.FindStringSubmatch(hook.command)
		if match == nil {
			check.Message = fmt.Sprintf("stale form on %s; remedy: amq wake install-dialog-hook", hook.event)
			return check, true
		}
		bin := strings.ReplaceAll(match[1], `'\''`, `'`)
		if _, err := os.Stat(bin); err != nil {
			check.Message = fmt.Sprintf("the hook runs a missing amq binary %s; remedy: amq wake install-dialog-hook", bin)
			return check, true
		}
		if hook.command != wakeDialogHookCommand(bin, wakeDialogHookVerb(hook.event)) {
			check.Message = fmt.Sprintf("stale form on %s; remedy: amq wake install-dialog-hook", hook.event)
			return check, true
		}
	}
	check.Status = "ok"
	check.Message = "installed"
	return check, true
}
