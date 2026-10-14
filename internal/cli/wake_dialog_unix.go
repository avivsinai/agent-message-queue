//go:build darwin || linux

package cli

import (
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

// wakeDialogHookEvents are the events the installer wires, in order. Only
// long-standing events: a settings file with an event an older Claude Code
// does not know could cost every other hook in it.
var wakeDialogHookEvents = []string{
	"PreToolUse",
	"PermissionRequest",
	"PostToolUse",
	"UserPromptSubmit",
	"Stop",
	"SessionStart",
	"SessionEnd",
}

// wakeDialogHookInput is the part of Claude Code's hook input the receiver
// reads.
type wakeDialogHookInput struct {
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	Tool      string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
	Cwd       string `json:"cwd"`
}

// wakeDialogMarker is the .wake.dialog content. The wake uses only the
// file's presence and mtime; tool_use_id scopes a PostToolUse close and the
// rest is for diagnosis.
type wakeDialogMarker struct {
	Schema    int    `json:"schema"`
	Event     string `json:"event,omitempty"`
	Tool      string `json:"tool,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
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
	usage := usageWithFlags(fs, "amq wake dialog open|close [--me <agent>] [--root <path>]",
		"Claude Code hook receiver for the wake dialog guard; amq wake install-dialog-hook",
		"installs it. Reads the hook input JSON on stdin.",
		"",
		"open writes <root>/agents/<me>/.wake.dialog; the wake writes no terminal input while",
		"it is present. close removes it; a close with a tool_use_id removes only the marker",
		"of that tool call, and a close with unreadable input keeps it. A close with empty",
		"stdin removes it. Always exits 0 once the arguments parse and prints nothing.")
	if handled, err := parseFlagsAllowPositionals(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "open" && fs.Arg(0) != "close") {
		return UsageError("wake dialog needs exactly one of open or close")
	}
	open := fs.Arg(0) == "open"

	var in wakeDialogHookInput
	inputErr := json.NewDecoder(io.LimitReader(stdin, maxWakeDialogHookInputBytes)).Decode(&in)
	if errors.Is(inputErr, io.EOF) {
		inputErr = nil
	}
	if !open && inputErr != nil {
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
		if open {
			return writeWakeDialogMarker(dirfd, agentDir, in)
		}
		return clearWakeDialogMarker(dirfd, agentDir, in.ToolUseID)
	})
	return nil
}

func writeWakeDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
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
	return writeWakeRepairMetadataAt(
		dirfd,
		agentDir,
		wakeDialogFileName,
		"wake dialog marker",
		append(raw, '\n'),
		maxWakeMetadataFileBytes,
	)
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

// wakeDialogHookCommand is the shell command one hook runs: the marker, the
// single-quoted binary, and a trailing || true. Claude Code reads exit 2 as
// a blocking error for PreToolUse, UserPromptSubmit and Stop, and an older
// amq at this path would exit 2 on the unknown subcommand.
func wakeDialogHookCommand(bin, verb string) string {
	return wakeDialogHookMarker +
		"'" + strings.ReplaceAll(bin, "'", `'\''`) + "'" +
		" --no-update-check wake dialog " + verb + " || true"
}

// wakeDialogHookBinRe reads the binary back from wakeDialogHookCommand.
var wakeDialogHookBinRe = regexp.MustCompile(`^` + regexp.QuoteMeta(wakeDialogHookMarker) + `'((?:[^']|'\\'')*)'`)

func wakeDialogHooks(bin string) []claude.CommandHook {
	hooks := make([]claude.CommandHook, 0, len(wakeDialogHookEvents))
	for _, event := range wakeDialogHookEvents {
		hook := claude.CommandHook{Event: event, Marker: wakeDialogHookMarker, Timeout: 5}
		switch event {
		case "PreToolUse":
			hook.Matcher = wakeDialogHookTools
			hook.Command = wakeDialogHookCommand(bin, "open")
		case "PermissionRequest":
			hook.Command = wakeDialogHookCommand(bin, "open")
		case "PostToolUse":
			hook.Matcher = wakeDialogHookTools
			hook.Command = wakeDialogHookCommand(bin, "close")
		default:
			hook.Command = wakeDialogHookCommand(bin, "close")
		}
		hooks = append(hooks, hook)
	}
	return hooks
}

func runWakeInstallDialogHook(args []string) error {
	fs := flag.NewFlagSet("wake install-dialog-hook", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "Print the hooks without changing the settings file")
	usage := usageWithFlags(fs, "amq wake install-dialog-hook [--dry-run]",
		"Install the wake dialog guard hooks in ~/.claude/settings.json.",
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
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve the amq binary for the dialog hook: %w", err)
	}
	hooks := wakeDialogHooks(bin)
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
		return nil
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

func runWakeUninstallDialogHook(args []string) error {
	fs := flag.NewFlagSet("wake uninstall-dialog-hook", flag.ContinueOnError)
	usage := usageWithFlags(fs, "amq wake uninstall-dialog-hook",
		"Remove the wake dialog guard hooks (AMQ_DIALOG_HOOK entries) from",
		"~/.claude/settings.json. Other settings and hooks are not changed.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
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

// checkClaudeDialogHook reports a missing dialog hook, or one whose amq
// binary is gone, when ~/.claude exists. It is read-only.
func checkClaudeDialogHook() (doctorCheck, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return doctorCheck{}, false
	}
	if info, err := os.Stat(filepath.Join(home, ".claude")); err != nil || !info.IsDir() {
		return doctorCheck{}, false
	}
	check := doctorCheck{Name: "Claude dialog hook", Status: "warn"}
	commands, err := claude.MarkedHookCommands(home, "PreToolUse", wakeDialogHookMarker)
	if err != nil {
		check.Message = fmt.Sprintf("read Claude settings: %v", err)
		return check, true
	}
	if len(commands) == 0 {
		check.Message = "not installed: a wake doorbell can answer an open Claude Code dialog; remedy: amq wake install-dialog-hook"
		return check, true
	}
	for _, command := range commands {
		match := wakeDialogHookBinRe.FindStringSubmatch(command)
		if match == nil {
			continue
		}
		bin := strings.ReplaceAll(match[1], `'\''`, `'`)
		if _, err := os.Stat(bin); err != nil {
			check.Message = fmt.Sprintf("the hook runs a missing amq binary %s; remedy: amq wake install-dialog-hook", bin)
			return check, true
		}
	}
	check.Status = "ok"
	check.Message = "installed"
	return check, true
}
