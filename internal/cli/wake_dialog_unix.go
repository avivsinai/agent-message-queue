//go:build darwin || linux

package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
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

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"golang.org/x/sys/unix"
)

const (
	// wakeDialogHookMarker starts every hook command the installer writes;
	// uninstall and doctor find AMQ's entries by it.
	wakeDialogHookMarker = "AMQ_DIALOG_HOOK=1 "
	// wakeDialogHookTools are the tools whose dialog opens in PreToolUse.
	wakeDialogHookTools = "AskUserQuestion|ExitPlanMode"
	// wakeDialogHookNotifications are the Notification types the receiver
	// reads: permission_prompt (a permission dialog waited 6 s) opens a
	// missing marker; idle_prompt (60 s after a Stop, the prompt idle)
	// closes it. Live on Claude Code 2.1.310, idle_prompt did not fire while
	// a permission or AskUserQuestion dialog was open, nor after Esc or a
	// deny.
	wakeDialogHookNotifications = "idle_prompt|permission_prompt"
	// ExitPlanMode carries the whole plan in its hook input.
	maxWakeDialogHookInputBytes = 8 << 20
)

// wakeDialogHookEvents are the events the installer wires, in order: the
// closing events first, so an install that fails part way never leaves an
// open without its close. Only long-standing events: a settings file with an
// event an older Claude Code does not know could cost every other hook in it.
var wakeDialogHookEvents = []string{
	"PostToolUse",
	"PostToolUseFailure",
	"UserPromptSubmit",
	"Stop",
	"SessionStart",
	"SessionEnd",
	"Notification",
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
	// ToolInput identifies a permission dialog's tool call: Claude Code
	// gives PermissionRequest no tool_use_id, but the same tool_input as
	// that call's PostToolUse or PostToolUseFailure.
	ToolInput        json.RawMessage `json:"tool_input"`
	NotificationType string          `json:"notification_type"`
	Cwd              string          `json:"cwd"`
	// TurnID is the Codex turn (wake_dialog_codex_unix.go).
	TurnID       string `json:"turn_id"`
	GenerationID string `json:"generation_id"`
	Status       string `json:"status"`
	emptyInput   bool
}

// wakeDialogMarker is the .wake.dialog content. The wake uses the file's
// presence and mtime, and for a Startup marker its harness and cwd
// (startupDialogClosed); tool_use_id and input_digest scope a PostToolUse
// close and the rest is for diagnosis.
type wakeDialogMarker struct {
	Schema    int    `json:"schema"`
	Event     string `json:"event,omitempty"`
	Harness   string `json:"harness,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Tool      string `json:"tool,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	// InputDigest is wakeDialogInputDigest of the PermissionRequest that
	// opened the marker, set only when no other marker was present.
	InputDigest string `json:"input_digest,omitempty"`
	// TurnID is the Codex turn that asked, for diagnosis.
	TurnID    string `json:"turn_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	OpenedAt  string `json:"opened_at"`
}

// wakeDialogSourceInDir is the running wake's dialog source. It reads only
// through the canonical agent directory; a detached directory cannot show
// the hook's marker, so it reads as unknown, never as closed. A Startup
// marker reads as closed once the harness has recorded its trust decision,
// so the doorbell rings without a SessionStart hook.
func wakeDialogSourceInDir(agentDir *wakeAgentDir) func(time.Time) (wakeDialogState, error) {
	var startup startupDialogCache
	return func(now time.Time) (wakeDialogState, error) {
		raw, info, exists, readErr, err := readWakeDialogMarkerInDir(agentDir)
		if err != nil {
			return wakeDialogUnknown, err
		}
		if readErr == nil && exists && startupDialogClosed(raw, &startup) {
			return wakeDialogClosed, nil
		}
		var modTime time.Time
		if info != nil {
			modTime = info.ModTime()
		}
		return classifyWakeDialogMarker(modTime, exists, readErr, now)
	}
}

// wakeStartupTrustReleaseInDir reports whether the agent's Startup marker
// is still present and reads closed only because the harness recorded its
// trust decision. That record proves the trust dialog is gone, not that
// the harness accepts input yet: the harness's SessionStart hook removes
// the marker once the session is ready. Any doubt reads as a trust
// release.
func wakeStartupTrustReleaseInDir(agentDir *wakeAgentDir) func() bool {
	var startup startupDialogCache
	return func() bool {
		raw, _, exists, readErr, err := readWakeDialogMarkerInDir(agentDir)
		if err != nil || readErr != nil {
			return true
		}
		return exists && startupDialogClosed(raw, &startup)
	}
}

// wakeDialogHoldReasonInDir says why the agent's marker holds input, for the
// held-doorbell notice. marker is false when no marker exists. A marker that
// cannot be read or parsed gives the generic reason.
func wakeDialogHoldReasonInDir(agentDir *wakeAgentDir) func() (string, bool) {
	return func() (string, bool) {
		raw, _, exists, readErr, err := readWakeDialogMarkerInDir(agentDir)
		if err != nil || readErr != nil {
			return wakeDialogGenericHoldReason, true
		}
		if !exists {
			return "", false
		}
		var marker wakeDialogMarker
		if json.Unmarshal(raw, &marker) != nil {
			return wakeDialogGenericHoldReason, true
		}
		return wakeDialogHoldReason(marker), true
	}
}

// wakeDialogHoldDetailInDir names the marker that holds input for the wake
// log: its event, kind, harness, tool and age by mtime. Marker fields are
// quoted, so no hook input reaches the log raw.
func wakeDialogHoldDetailInDir(agentDir *wakeAgentDir) func(time.Time) string {
	return func(now time.Time) string {
		raw, info, exists, readErr, err := readWakeDialogMarkerInDir(agentDir)
		switch {
		case err != nil || readErr != nil:
			return "marker unreadable"
		case !exists:
			return "no marker"
		}
		var marker wakeDialogMarker
		if json.Unmarshal(raw, &marker) != nil {
			return "marker unparsable"
		}
		harness := marker.Harness
		if harness == "" && marker.Event != wakeDialogStartupEvent {
			harness = "claude" // Claude Code dialog hooks write no harness
		}
		return fmt.Sprintf("marker event=%q kind=%q harness=%q tool=%q age=%s",
			marker.Event, marker.Kind, harness, marker.Tool, now.Sub(info.ModTime()).Round(time.Second))
	}
}

// wakeDialogHoldReason maps a marker to fixed text; no marker field is
// copied into it.
func wakeDialogHoldReason(marker wakeDialogMarker) string {
	var harness string
	switch marker.Harness {
	case launch.CodexProvider:
		harness = "Codex"
	case "claude":
		harness = "Claude Code"
	case "cursor":
		harness = "Cursor"
	case "agy":
		harness = "Antigravity"
	case "pi":
		harness = "pi"
	case "":
		// Claude Code dialog hooks write no harness.
		if marker.Event != wakeDialogStartupEvent {
			harness = "Claude Code"
		}
	}
	if harness == "" {
		harness = "The harness"
	}
	switch {
	case marker.Event == wakeDialogStartupEvent && marker.Harness == launch.CodexProvider:
		return harness + " startup screen open; type a prompt in that terminal to release"
	case marker.Event == wakeDialogStartupEvent:
		return harness + " startup screen open; answer it in that terminal to release"
	}
	return harness + " dialog open; answer it in that terminal to release"
}

// readWakeDialogMarkerInDir reads the marker through the canonical agent
// directory. err reports a directory that is no longer canonical; readErr
// a marker that cannot be read.
func readWakeDialogMarkerInDir(agentDir *wakeAgentDir) (raw []byte, info os.FileInfo, exists bool, readErr, err error) {
	if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
		return nil, nil, false, nil, err
	}
	err = agentDir.withFD(func(dirfd int) error {
		raw, info, exists, readErr = readWakeRepairMetadataAt(
			dirfd,
			wakeDialogFileName,
			"wake dialog marker",
			filepath.Join(agentDir.path, wakeDialogFileName),
			maxWakeMetadataFileBytes,
		)
		return nil
	})
	return raw, info, exists, readErr, err
}

// runWakeDialog is the hook receiver. Errors after the arguments parse
// return nil: a hook must never block Claude Code, and a missing identity or
// mailbox only leaves today's behavior. It prints nothing on stdout, so it
// never decides a PreToolUse or PermissionRequest.
func runWakeDialog(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("wake dialog", flag.ContinueOnError)
	meFlag := fs.String("me", defaultMe(), "Agent handle (or AM_ME)")
	rootFlag := fs.String("root", "", "Root directory for the queue (default: AM_ROOT, else .amqrc discovery from the hook's cwd)")
	harness := fs.String("harness", "claude", "Hook input of claude, codex or cursor")
	usage := usageWithFlags(fs, "amq wake dialog [--me <agent>] [--root <path>] [--harness claude|codex|cursor] open|close|notify",
		"Claude Code hook receiver for the wake dialog guard; amq wake install-dialog-hook",
		"installs it. Reads the hook input JSON on stdin.",
		"",
		"open writes <root>/agents/<me>/.wake.dialog; the wake writes no terminal input while",
		"it is present. close removes it; a close with a tool_use_id removes only the marker",
		"of that tool call or of the permission dialog with the same tool_input, and a close",
		"with unreadable input keeps it. A close with empty stdin removes it. notify reads a",
		"Claude Code Notification: idle_prompt closes, permission_prompt opens a missing",
		"marker. With --harness codex or cursor only a turn or session event",
		"removes it; for codex without AM_* the seat is the one amq coop exec recorded for",
		"the hook's session_id.",
		"Always exits 0 once the arguments parse and prints nothing.")
	if handled, err := parseFlagsAllowPositionals(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	verb := fs.Arg(0)
	if fs.NArg() != 1 || (verb != "open" && verb != "close" && verb != "notify") {
		return UsageError("wake dialog needs exactly one of open, close or notify")
	}
	open := verb == "open"
	codex := *harness == "codex"
	turn := codex || *harness == "cursor"
	if !turn && *harness != "claude" {
		return UsageError("wake dialog --harness must be claude, codex or cursor")
	}
	if turn && verb == "notify" {
		return UsageError("wake dialog notify reads Claude Code input only")
	}

	var in wakeDialogHookInput
	inputErr := json.NewDecoder(io.LimitReader(stdin, maxWakeDialogHookInputBytes)).Decode(&in)
	if errors.Is(inputErr, io.EOF) {
		inputErr = nil
		in.emptyInput = true
	}
	if !open && inputErr != nil {
		return nil
	}
	// notify: idle_prompt is a close, permission_prompt an open of a
	// missing marker; any other type changes nothing.
	openIfAbsent := false
	if verb == "notify" {
		switch in.NotificationType {
		case "idle_prompt":
		case "permission_prompt":
			open, openIfAbsent = true, true
		default:
			return nil
		}
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
		case turn && open:
			return writeTurnDialogMarker(dirfd, agentDir, in, *harness)
		case turn:
			return clearTurnDialogMarker(dirfd, agentDir, in)
		case openIfAbsent:
			return openMissingWakeDialogMarker(dirfd, agentDir, in)
		case open:
			return writeWakeDialogMarker(dirfd, agentDir, in)
		}
		return clearWakeDialogMarker(dirfd, agentDir, in)
	})
	return nil
}

// writeWakeDialogMarker writes the marker for in. A marker without a
// tool_use_id (a permission dialog) stays without one, so a PostToolUse of
// a later tool call cannot clear the permission dialog's hold. A
// PermissionRequest records its tool call's input digest only when no
// marker is present: a marker that replaced another dialog's closes only on
// a turn or session event. Claude Code also sends PermissionRequest for
// AskUserQuestion and ExitPlanMode right after their PreToolUse; that
// marker keeps the PreToolUse tool_use_id, because the answered call's
// PostToolUse input adds the answers and matches no digest.
func writeWakeDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	current, _, exists, err := readWakeRepairMetadataAt(
		dirfd,
		wakeDialogFileName,
		"wake dialog marker",
		filepath.Join(agentDir.path, wakeDialogFileName),
		maxWakeMetadataFileBytes,
	)
	var marker wakeDialogMarker
	parsed := err == nil && exists && json.Unmarshal(current, &marker) == nil
	if in.ToolUseID != "" && parsed && marker.ToolUseID == "" {
		in.ToolUseID = ""
	}
	if in.Event == "PermissionRequest" && parsed && marker.Event == "PreToolUse" &&
		marker.ToolUseID != "" && marker.Tool == in.Tool {
		in.ToolUseID = marker.ToolUseID
	}
	var digest string
	if in.Event == "PermissionRequest" && err == nil && !exists {
		digest = wakeDialogInputDigest(in)
	}
	raw, err := json.Marshal(wakeDialogMarker{
		Schema:      1,
		Event:       in.Event,
		Tool:        in.Tool,
		ToolUseID:   in.ToolUseID,
		InputDigest: digest,
		SessionID:   in.SessionID,
		OpenedAt:    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return replaceWakeDialogMarkerAt(dirfd, agentDir, append(raw, '\n'))
}

// openMissingWakeDialogMarker writes a marker for a permission_prompt
// notification only when none is present: the PermissionRequest marker it
// follows keeps its digest, and a missing one (its hook failed) still holds.
func openMissingWakeDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	raw, err := json.Marshal(wakeDialogMarker{
		Schema:    1,
		Event:     in.Event,
		Kind:      in.NotificationType,
		SessionID: in.SessionID,
		OpenedAt:  time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return installWakeDialogMarkerAt(dirfd, agentDir, append(raw, '\n'), true)
}

// wakeDialogInputDigest identifies one tool call by its name and input, so
// a PostToolUse can close the permission dialog that asked for it. It is
// empty without both.
func wakeDialogInputDigest(in wakeDialogHookInput) string {
	if in.Tool == "" || len(in.ToolInput) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(in.ToolInput))
	dec.UseNumber()
	var input any
	if dec.Decode(&input) != nil {
		return ""
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	sum := sha256.New()
	sum.Write([]byte(in.Tool))
	sum.Write([]byte{0})
	sum.Write(canonical)
	return hex.EncodeToString(sum.Sum(nil))
}

// replaceWakeDialogMarkerAt installs data as the marker by an exclusive
// temp file and a rename, without fsync or read-back: the marker is
// advisory, and a marker lost in a crash leaves the wake as it is without the
// hook. The open runs as the dialog renders, so it must not wait on the disk.
func replaceWakeDialogMarkerAt(dirfd int, agentDir *wakeAgentDir, data []byte) error {
	return installWakeDialogMarkerAt(dirfd, agentDir, data, false)
}

// installWakeDialogMarkerAt is replaceWakeDialogMarkerAt; with onlyIfAbsent
// it keeps a present or unreadable marker.
func installWakeDialogMarkerAt(dirfd int, agentDir *wakeAgentDir, data []byte, onlyIfAbsent bool) error {
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
	if err := withWakeDialogMarkerLock(dirfd, func() error {
		if err := validateWakeRepairMetadataDestinationAt(dirfd, agentDir, wakeDialogFileName, label); err != nil {
			return err
		}
		if onlyIfAbsent {
			_, _, exists, err := readWakeRepairMetadataAt(dirfd, wakeDialogFileName, label,
				filepath.Join(agentDir.path, wakeDialogFileName), maxWakeMetadataFileBytes)
			if err != nil || exists {
				return err
			}
		}
		if err := wakeRenameAt(dirfd, temp, dirfd, wakeDialogFileName); err != nil {
			return fmt.Errorf("install %s: %w", label, err)
		}
		tempPresent = false
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// wakeDialogLockFileName serializes every change of the dialog marker.
const wakeDialogLockFileName = ".wake.dialog.lock"

// withWakeDialogMarkerLock runs fn holding the marker lock, so a
// replacement and a conditional removal never interleave: a writer cannot
// replace the marker between a remover's read and its unlink. The wake
// only reads the marker and takes no lock.
func withWakeDialogMarkerLock(dirfd int, fn func() error) error {
	fd, err := unix.Openat(dirfd, wakeDialogLockFileName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("open wake dialog marker lock: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	for {
		err = unix.Flock(fd, unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("lock wake dialog marker: %w", err)
	}
	return fn()
}

// removeWakeDialogMarkerIf removes the marker when remove accepts its
// current bytes, under the marker lock.
func removeWakeDialogMarkerIf(dirfd int, agentDir *wakeAgentDir, remove func(current []byte) bool) error {
	return withWakeDialogMarkerLock(dirfd, func() error {
		raw, _, exists, err := readWakeRepairMetadataAt(
			dirfd,
			wakeDialogFileName,
			"wake dialog marker",
			filepath.Join(agentDir.path, wakeDialogFileName),
			maxWakeMetadataFileBytes,
		)
		if err != nil || !exists || !remove(raw) {
			return err
		}
		if err := wakeUnlinkAt(dirfd, wakeDialogFileName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		return nil
	})
}

// clearWakeDialogMarker checks the current marker under its lock. Cursor
// can queue a prompt during an approval and sends abort/error stop hooks
// without waiting; neither may release a later turn's hold. A tool call's
// PostToolUse or PostToolUseFailure closes the marker its own PreToolUse or
// PermissionRequest opened, matched by tool_use_id or input digest: Claude
// Code asks parallel tool calls one dialog at a time, so the next call's
// PermissionRequest can land before the previous call's PostToolUse.
func clearWakeDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	return removeWakeDialogMarkerIf(dirfd, agentDir, func(raw []byte) bool {
		var marker wakeDialogMarker
		if json.Unmarshal(raw, &marker) == nil && marker.Harness == "cursor" {
			return cursorDialogCloseMatches(marker, in)
		}
		if in.ToolUseID == "" || marker.ToolUseID == in.ToolUseID {
			return true
		}
		return marker.ToolUseID == "" && marker.InputDigest != "" &&
			marker.InputDigest == wakeDialogInputDigest(in)
	})
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
// harnessFlag is empty for Claude Code, or a --harness flag and a space.
func wakeDialogHookCommand(bin, harnessFlag, verb string) string {
	gate := `[ -z "${AM_ME-}" ] || `
	if verb == "close" {
		gate += wakeDialogNoMarkerTest + ` || `
	}
	return wakeDialogHookMarker + gate + singleQuote(bin) +
		" --no-update-check wake dialog " + harnessFlag + verb + " || true"
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
var wakeDialogHookBinRe = regexp.MustCompile(`'((?:[^']|'\\'')*)' --no-update-check wake dialog (?:open|close|notify) \|\| true$`)

// wakeDialogHookVerb is the receiver verb the installer wires to event.
// Notification can open a missing marker, so it runs under the open gate.
func wakeDialogHookVerb(event string) string {
	switch event {
	case "PreToolUse", "PermissionRequest":
		return "open"
	case "Notification":
		return "notify"
	}
	return "close"
}

func wakeDialogHooks(bin string) []claude.CommandHook {
	hooks := make([]claude.CommandHook, 0, len(wakeDialogHookEvents))
	for _, event := range wakeDialogHookEvents {
		hook := claude.CommandHook{
			Event:   event,
			Marker:  wakeDialogHookMarker,
			Command: wakeDialogHookCommand(bin, "", wakeDialogHookVerb(event)),
			Timeout: 5,
		}
		switch event {
		case "PreToolUse":
			hook.Matcher = wakeDialogHookTools
		case "Notification":
			hook.Matcher = wakeDialogHookNotifications
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
	harness := fs.String("harness", "claude", "claude, codex, cursor, or all (each harness whose home exists)")
	usage := usageWithFlags(fs, "amq wake install-dialog-hook [--dry-run] [--harness claude|codex|cursor|all]",
		"Install the wake dialog guard hooks in ~/.claude/settings.json, with --harness",
		"codex in $CODEX_HOME/hooks.json (Codex runs them once you trust them in /hooks),",
		"or with --harness cursor in ~/.cursor/hooks.json (Cursor reads it when a session",
		"starts; it holds the doorbell from a tool call to the end of that turn).",
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
	claudeOn, codexOn, cursorOn, err := dialogHookHarnesses(*harness)
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
	if cursorOn {
		if err := installCursorDialogHook(bin, *dryRun); err != nil {
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

// dialogHookHarnesses reads --harness: claude, codex, cursor, or all,
// which picks each harness whose home directory exists.
func dialogHookHarnesses(harness string) (claudeOn, codexOn, cursorOn bool, err error) {
	switch harness {
	case "claude":
		return true, false, false, nil
	case "codex":
		return false, true, false, nil
	case "cursor":
		return false, false, true, nil
	case "all":
		isDir := func(path string) bool {
			info, err := os.Stat(path)
			return err == nil && info.IsDir()
		}
		if home, err := os.UserHomeDir(); err == nil {
			claudeOn = isDir(filepath.Join(home, ".claude"))
		}
		if path, err := codexHooksPath(); err == nil {
			codexOn = isDir(filepath.Dir(path))
		}
		if path, err := cursorHooksPath(); err == nil {
			cursorOn = isDir(filepath.Dir(path))
		}
		return claudeOn, codexOn, cursorOn, nil
	}
	return false, false, false, UsageError("--harness must be claude, codex, cursor or all")
}

func runWakeUninstallDialogHook(args []string) error {
	fs := flag.NewFlagSet("wake uninstall-dialog-hook", flag.ContinueOnError)
	harness := fs.String("harness", "claude", "claude, codex, cursor, or all (each harness whose home exists)")
	usage := usageWithFlags(fs, "amq wake uninstall-dialog-hook [--harness claude|codex|cursor|all]",
		"Remove the wake dialog guard hooks (AMQ_DIALOG_HOOK entries) from",
		"~/.claude/settings.json, $CODEX_HOME/hooks.json with --harness codex, or",
		"~/.cursor/hooks.json with --harness cursor.",
		"Other settings and hooks are not changed.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	claudeOn, codexOn, cursorOn, err := dialogHookHarnesses(*harness)
	if err != nil {
		return err
	}
	if codexOn {
		if err := uninstallCodexDialogHook(); err != nil {
			return err
		}
	}
	if cursorOn {
		if err := uninstallCursorDialogHook(); err != nil {
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
		if hook.command != wakeDialogHookCommand(bin, "", wakeDialogHookVerb(hook.event)) {
			check.Message = fmt.Sprintf("stale form on %s; remedy: amq wake install-dialog-hook", hook.event)
			return check, true
		}
	}
	check.Status = "ok"
	check.Message = "installed"
	return check, true
}
