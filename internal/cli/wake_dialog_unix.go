//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"golang.org/x/sys/unix"
)

const (
	// wakeDialogHookMarker starts every hook command the installer writes;
	// uninstall and doctor find AMQ's entries by it.
	wakeDialogHookMarker = "AMQ_DIALOG_HOOK=1 "
	// wakeDialogHookTools are the tools whose dialog opens in PreToolUse and
	// closes in PostToolUse or PostToolUseFailure.
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
	"PostToolUseFailure",
	"UserPromptSubmit",
	"Stop",
	"SessionStart",
	"SessionEnd",
	"PreToolUse",
	"PermissionRequest",
}

// wakeDialogHookEventFloor is the first Claude Code version known to have
// each gated event: the changelog first names PostToolUseFailure in
// 2.1.119. Before 2.1.101 an unknown event made Claude Code ignore the
// whole settings file, so the installer adds a gated event only for a
// version at or past its floor.
var wakeDialogHookEventFloor = map[string]claudeVersion{
	"PostToolUseFailure": {2, 1, 119},
}

// claudeVersion is a Claude Code major.minor.patch.
type claudeVersion [3]int

func (v claudeVersion) less(floor claudeVersion) bool {
	for i := range v {
		if v[i] != floor[i] {
			return v[i] < floor[i]
		}
	}
	return false
}

var claudeVersionRe = regexp.MustCompile(`^\s*(\d+)\.(\d+)\.(\d+)`)

// installedClaudeVersion reads `claude --version` from the claude on PATH,
// through probeHarnessVersion: bounded output, and the process group killed
// and reaped at the deadline, so a launcher's child that keeps the pipe open
// cannot stall the installer or doctor. ok is false when it is missing,
// fails, or prints no version.
func installedClaudeVersion() (claudeVersion, bool) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return claudeVersion{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, ok := probeHarnessVersion(ctx, path)
	if !ok {
		return claudeVersion{}, false
	}
	match := claudeVersionRe.FindStringSubmatch(out)
	if match == nil {
		return claudeVersion{}, false
	}
	var v claudeVersion
	for i := range v {
		if v[i], err = strconv.Atoi(match[i+1]); err != nil {
			return claudeVersion{}, false
		}
	}
	return v, true
}

// wakeDialogHookEventsFor is wakeDialogHookEvents without the events the
// given Claude Code version may not know; an unknown version gets none of
// the gated events.
func wakeDialogHookEventsFor(version claudeVersion, known bool) []string {
	events := make([]string, 0, len(wakeDialogHookEvents))
	for _, event := range wakeDialogHookEvents {
		if floor, gated := wakeDialogHookEventFloor[event]; gated && (!known || version.less(floor)) {
			continue
		}
		events = append(events, event)
	}
	return events
}

// wakeDialogHookInput is the part of Claude Code's hook input the receiver
// reads.
type wakeDialogHookInput struct {
	Event          string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	Tool           string `json:"tool_name"`
	ToolUseID      string `json:"tool_use_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	// TurnID is the Codex turn (wake_dialog_codex_unix.go).
	TurnID string `json:"turn_id"`
	// ownIdentity reports a hook whose AM_ROOT and AM_ME came from its own
	// environment, not from a thread identity record.
	ownIdentity  bool
	GenerationID string `json:"generation_id"`
	Status       string `json:"status"`
	emptyInput   bool
}

// wakeDialogMarker is the .wake.dialog content. The wake uses the file's
// presence and mtime, and for a Startup marker its harness and cwd
// (startupDialogClosed); tool_use_id scopes a PostToolUse close and the
// transcript proof, and the rest is for diagnosis.
type wakeDialogMarker struct {
	Schema    int    `json:"schema"`
	Event     string `json:"event,omitempty"`
	Harness   string `json:"harness,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Tool      string `json:"tool,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	// TranscriptPath is the Claude Code session transcript the wake reads
	// for proof that Esc or a deny closed the tool call's dialog.
	TranscriptPath string `json:"transcript_path,omitempty"`
	// TurnID is the Codex turn that asked, for diagnosis.
	TurnID    string `json:"turn_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	OpenedAt  string `json:"opened_at"`
	// Launch is the process amq coop exec execs into for a Codex Startup
	// marker; only a prompt hook that descends from it releases the marker.
	Launch *launch.HolderIdentity `json:"launch,omitempty"`
}

// wakeDialogSourceInDir is the running wake of harnessName's dialog source.
// It reads only through the canonical agent directory. A Claude Code
// Startup marker reads as closed once Claude Code has recorded its trust
// decision (a Codex one never does), so the doorbell rings without a
// SessionStart hook. A Claude Code marker whose session transcript shows a
// result for its tool call (claudeDialogAnswered) is removed; a missing or
// unreadable transcript, a busy lock or a changed marker keeps the hold
// until the next recheck.
//
// A marker that proves no open dialog (older than wakeDialogQuietHold,
// dated in the future, unreadable, or behind a detached directory) reads as
// unknown, and so holds input, only while the dialog guard of the harness
// that wrote it (else harnessName) is on (dialogGuardEnabled). Without the
// guard it reads as closed: the default wake is a plain doorbell that never
// stays held by a marker nothing will close.
func wakeDialogSourceInDir(agentDir *wakeAgentDir, harnessName string) func(time.Time) (wakeDialogState, error) {
	var startup startupDialogCache
	var proof claudeDialogProof
	guard := dialogGuardCache{}
	unknown := func(raw []byte, readErr error) (wakeDialogState, error) {
		name := wakeDialogMarkerHarness(raw)
		if name == "" {
			name = harnessName
		}
		if !guard.enabled(name) {
			return wakeDialogClosed, nil
		}
		return wakeDialogUnknown, readErr
	}
	return func(now time.Time) (wakeDialogState, error) {
		raw, info, exists, readErr, err := readWakeDialogMarkerInDir(agentDir)
		if err != nil {
			return unknown(nil, err)
		}
		if readErr == nil && exists && startupDialogClosed(raw, &startup) {
			return wakeDialogClosed, nil
		}
		if readErr == nil && exists && info != nil && proof.answered(raw, info.ModTime()) &&
			removeWakeDialogMarkerVersion(agentDir, raw, info) {
			return wakeDialogClosed, nil
		}
		var modTime time.Time
		if info != nil {
			modTime = info.ModTime()
		}
		state, stateErr := classifyWakeDialogMarker(modTime, exists, readErr, now)
		if state == wakeDialogUnknown {
			return unknown(raw, stateErr)
		}
		return state, stateErr
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
// held-doorbell notice. A marker that cannot be read or parsed gives the
// generic reason.
func wakeDialogHoldReasonInDir(agentDir *wakeAgentDir) func() wakeDialogHoldCause {
	return func() wakeDialogHoldCause {
		raw, _, exists, readErr, err := readWakeDialogMarkerInDir(agentDir)
		if err != nil || readErr != nil {
			return wakeDialogHoldCause{reason: wakeDialogGenericHoldReason, marker: true}
		}
		if !exists {
			return wakeDialogHoldCause{}
		}
		var marker wakeDialogMarker
		if json.Unmarshal(raw, &marker) != nil {
			return wakeDialogHoldCause{reason: wakeDialogGenericHoldReason, marker: true}
		}
		sum := sha256.Sum256(raw)
		return wakeDialogHoldCause{reason: wakeDialogHoldReason(marker), marker: true, busy: agyTurnMarker(marker), generation: hex.EncodeToString(sum[:])}
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
		transcript := "none"
		if marker.TranscriptPath != "" && marker.ToolUseID != "" && harness == "claude" {
			transcript = "no proof yet"
			if _, err := claudeDialogAnswered(marker); err != nil {
				transcript = fmt.Sprintf("unreadable, holding: %q", err.Error())
			}
		}
		return fmt.Sprintf("marker event=%q kind=%q harness=%q tool=%q age=%s transcript=%s",
			marker.Event, marker.Kind, harness, marker.Tool, now.Sub(info.ModTime()).Round(time.Second), transcript)
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
	case agyTurnMarker(marker):
		return harness + " is busy; the doorbell rings when the turn ends"
	case marker.Event == wakeDialogStartupEvent && marker.Harness == launch.CodexProvider:
		return harness + " startup screen open; answer it in that terminal; " + codexStartupHoldRemedy
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
	usage := usageWithFlags(fs, "amq wake dialog [--me <agent>] [--root <path>] [--harness claude|codex|cursor] open|close",
		"Claude Code hook receiver for the wake dialog guard; amq wake install-dialog-hook",
		"installs it. Reads the hook input JSON on stdin.",
		"",
		"open writes <root>/agents/<me>/.wake.dialog; the wake writes no terminal input while",
		"it is present. close removes it; a close with a tool_use_id removes only the marker",
		"of that tool call, and a close with unreadable input keeps it. A close with empty",
		"stdin removes it. With --harness codex or cursor only a turn or session event",
		"removes it; for codex without AM_* the seat is the one amq coop exec recorded for",
		"the hook's session_id.",
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
	turn := codex || *harness == "cursor"
	if !turn && *harness != "claude" {
		return UsageError("wake dialog --harness must be claude, codex or cursor")
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
	_, ownRoot := os.LookupEnv(envRoot)
	_, ownMe := os.LookupEnv(envMe)
	in.ownIdentity = ownRoot && ownMe
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
		if !open && !turn {
			acknowledgeClaudeNativeHook(dirfd, agentDir, in)
		}
		switch {
		case turn && open:
			return writeTurnDialogMarker(dirfd, agentDir, in, *harness)
		case turn:
			return clearTurnDialogMarker(dirfd, agentDir, in)
		case open:
			return writeWakeDialogMarker(dirfd, agentDir, in)
		}
		return clearWakeDialogMarker(dirfd, agentDir, in)
	})
	return nil
}

// writeWakeDialogMarker writes the marker for in. A present marker without
// a tool_use_id (a permission dialog) stays as it is when a tool call opens,
// so a PostToolUse of that call cannot clear the permission dialog's hold: a
// permission marker closes only on a turn or session event. A PermissionRequest names
// no tool call, so it never takes the tool_use_id of the marker it
// replaces: that marker can belong to another call of the same tool.
func writeWakeDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	raw, err := json.Marshal(wakeDialogMarker{
		Schema:         1,
		Event:          in.Event,
		Tool:           in.Tool,
		ToolUseID:      in.ToolUseID,
		TranscriptPath: in.TranscriptPath,
		SessionID:      in.SessionID,
		OpenedAt:       time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	if in.ToolUseID == "" {
		return replaceWakeDialogMarkerAt(dirfd, agentDir, append(raw, '\n'))
	}
	// A tool call's open keeps a present marker without a tool_use_id: that
	// hold closes only on a turn or session event, and the decision shares
	// the marker lock with the replacement.
	return replaceWakeDialogMarkerUnlessAt(dirfd, agentDir, append(raw, '\n'), func(current []byte) bool {
		var marker wakeDialogMarker
		return json.Unmarshal(current, &marker) == nil && marker.ToolUseID == ""
	})
}

// replaceWakeDialogMarkerAt installs data as the marker by an exclusive
// temp file and a rename, without fsync or read-back: the marker is
// advisory, and a marker lost in a crash leaves the wake as it is without the
// hook. The open runs as the dialog renders, so it must not wait on the disk.
func replaceWakeDialogMarkerAt(dirfd int, agentDir *wakeAgentDir, data []byte) error {
	return replaceWakeDialogMarkerUnlessAt(dirfd, agentDir, data, nil)
}

// replaceWakeDialogMarkerUnlessAt is replaceWakeDialogMarkerAt that keeps
// the present marker when keep accepts its bytes; the read and the
// replacement share the marker lock.
func replaceWakeDialogMarkerUnlessAt(dirfd int, agentDir *wakeAgentDir, data []byte, keep func(current []byte) bool) error {
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
		if keep != nil {
			current, _, exists, err := readWakeRepairMetadataAt(dirfd, wakeDialogFileName, label,
				filepath.Join(agentDir.path, wakeDialogFileName), maxWakeMetadataFileBytes)
			if err != nil {
				return err
			}
			if exists && keep(current) {
				return nil
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
// replace the marker between a remover's read and its unlink, and a guarded
// writer reads and replaces under it. The wake takes it only to remove a
// Claude Code marker whose transcript shows its tool call's result, and
// never waits for it.
func withWakeDialogMarkerLock(dirfd int, fn func() error) error {
	return lockWakeDialogMarker(dirfd, unix.LOCK_EX, fn)
}

// errWakeDialogLockBusy is a non-blocking marker lock held by a hook.
var errWakeDialogLockBusy = errors.New("wake dialog marker lock busy")

func lockWakeDialogMarker(dirfd, how int, fn func() error) error {
	fd, err := unix.Openat(dirfd, wakeDialogLockFileName, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("open wake dialog marker lock: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	for {
		err = unix.Flock(fd, how)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errWakeDialogLockBusy
	}
	if err != nil {
		return fmt.Errorf("lock wake dialog marker: %w", err)
	}
	return fn()
}

// removeWakeDialogMarkerVersion removes the marker the wake read as raw
// with info, only if it is still that version (same bytes, mtime and
// size), under the marker lock taken without waiting. It reports whether
// it removed it.
func removeWakeDialogMarkerVersion(agentDir *wakeAgentDir, raw []byte, info os.FileInfo) bool {
	if validateCanonicalWakeAgentDir(agentDir) != nil {
		return false
	}
	removed := false
	err := agentDir.withFD(func(dirfd int) error {
		return lockWakeDialogMarker(dirfd, unix.LOCK_EX|unix.LOCK_NB, func() error {
			current, now, exists, err := readWakeRepairMetadataAt(
				dirfd,
				wakeDialogFileName,
				"wake dialog marker",
				filepath.Join(agentDir.path, wakeDialogFileName),
				maxWakeMetadataFileBytes,
			)
			if err != nil || !exists || now == nil || !bytes.Equal(current, raw) ||
				!now.ModTime().Equal(info.ModTime()) || now.Size() != info.Size() {
				return err
			}
			if err := wakeUnlinkAt(dirfd, wakeDialogFileName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
			removed = true
			return nil
		})
	})
	return err == nil && removed
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
// PostToolUse or PostToolUseFailure closes only the marker of the same
// tool_use_id.
func clearWakeDialogMarker(dirfd int, agentDir *wakeAgentDir, in wakeDialogHookInput) error {
	return removeWakeDialogMarkerIf(dirfd, agentDir, func(raw []byte) bool {
		var marker wakeDialogMarker
		decoded := json.Unmarshal(raw, &marker) == nil
		if decoded && marker.Kind == claudeNativeStartupKind {
			return acknowledgeClaudeNativeStartup(dirfd, agentDir, marker, in)
		}
		if decoded && marker.Harness == "cursor" {
			return cursorDialogCloseMatches(marker, in)
		}
		return in.ToolUseID == "" || marker.ToolUseID == in.ToolUseID
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
		gate += `{ [ -z "${AMQ_CLAUDE_NATIVE_LAUNCH-}" ] && ` + wakeDialogNoMarkerTest + `; } || `
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
var wakeDialogHookBinRe = regexp.MustCompile(`'((?:[^']|'\\'')*)' --no-update-check wake dialog (?:open|close) \|\| true$`)

// wakeDialogHookVerb is the receiver verb the installer wires to event.
func wakeDialogHookVerb(event string) string {
	if event == "PreToolUse" || event == "PermissionRequest" {
		return "open"
	}
	return "close"
}

func wakeDialogHooks(bin string, events []string) []claude.CommandHook {
	hooks := make([]claude.CommandHook, 0, len(events))
	for _, event := range events {
		hook := claude.CommandHook{
			Event:   event,
			Marker:  wakeDialogHookMarker,
			Command: wakeDialogHookCommand(bin, "", wakeDialogHookVerb(event)),
			Timeout: 5,
		}
		switch event {
		case "PreToolUse", "PostToolUse", "PostToolUseFailure":
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
	harness := fs.String("harness", "claude", "claude, codex, cursor, or all (each harness whose home exists)")
	usage := usageWithFlags(fs, "amq wake install-dialog-hook [--dry-run] [--harness claude|codex|cursor|all]",
		"Install the wake dialog guard hooks in $CLAUDE_CONFIG_DIR/settings.json (default",
		"~/.claude/settings.json), with --harness codex in $CODEX_HOME/hooks.json",
		"(Codex runs them once you trust them in /hooks),",
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
	hooks := wakeDialogHooks(bin, wakeDialogHookEventsFor(installedClaudeVersion()))
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
		claude.SettingsPath(home),
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
			claudeOn = isDir(filepath.Dir(claude.SettingsPath(home)))
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
		"$CLAUDE_CONFIG_DIR/settings.json (default ~/.claude/settings.json),",
		"$CODEX_HOME/hooks.json with --harness codex, or",
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
	return writeStdout("dialog hook removed from %s\n", claude.SettingsPath(home))
}

// checkClaudeDialogHook reports a missing or partly installed dialog hook,
// or one whose amq binary is gone, when ~/.claude exists. It is read-only.
func checkClaudeDialogHook() (doctorCheck, bool) {
	return claudeDialogHookCheck(installedClaudeVersion())
}

// claudeDialogHookCheck is checkClaudeDialogHook for a known or unknown
// Claude Code version. An unknown version needs only the ungated events, so
// the check runs without starting claude.
func claudeDialogHookCheck(version claudeVersion, known bool) (doctorCheck, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return doctorCheck{}, false
	}
	if info, err := os.Stat(filepath.Dir(claude.SettingsPath(home))); err != nil || !info.IsDir() {
		return doctorCheck{}, false
	}
	check := doctorCheck{Name: "Claude dialog hook", Status: "warn"}
	type installed struct{ event, command string }
	var commands []installed
	var missing []string
	supported := make(map[string]bool)
	for _, event := range wakeDialogHookEventsFor(version, known) {
		supported[event] = true
	}
	for _, event := range wakeDialogHookEvents {
		found, err := claude.MarkedHookCommands(home, event, wakeDialogHookMarker)
		if err != nil {
			check.Message = fmt.Sprintf("read Claude settings: %v", err)
			return check, true
		}
		if len(found) == 0 && supported[event] {
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

// dialogGuardEnabled reports whether the AMQ dialog hook of the harness
// name is installed, which is the opt-in that lets a marker that proves no
// open dialog keep holding input: in full with its amq binary present, and
// for Codex trusted and enabled in /hooks. Only Claude Code, Codex and
// Cursor have one. The agy relay (an opt-in through AGY_CLI_OSC7501=on) and
// the pi extension are not visible to the wake, so their markers hold only
// while they prove an open prompt or turn. It starts no process.
func dialogGuardEnabled(name string) bool {
	var check func() (doctorCheck, bool)
	switch name {
	case harness.Claude:
		check = func() (doctorCheck, bool) { return claudeDialogHookCheck(claudeVersion{}, false) }
	case harness.Codex:
		check = checkCodexDialogHook
	case harness.Cursor:
		check = checkCursorDialogHook
	default:
		return false
	}
	result, ok := check()
	return ok && result.Status == "ok"
}

// dialogGuardCache keeps the last dialogGuardEnabled answer per harness
// while the files that decide it and the presence of the hooks' amq
// binaries are unchanged, so a wake that rechecks a marker does not reparse
// the hooks files each time, and a removed binary turns the guard off.
type dialogGuardCache map[string]dialogGuardCacheEntry

type dialogGuardCacheEntry struct {
	stamp   string
	bins    []string
	present string
	enabled bool
}

func (c dialogGuardCache) enabled(name string) bool {
	stamp := dialogGuardStamp(name)
	if entry, ok := c[name]; ok && entry.stamp == stamp && entry.present == binariesPresent(entry.bins) {
		return entry.enabled
	}
	bins := dialogHookBinaries(name)
	entry := dialogGuardCacheEntry{stamp: stamp, bins: bins, present: binariesPresent(bins), enabled: dialogGuardEnabled(name)}
	c[name] = entry
	return entry.enabled
}

// binariesPresent records which of bins exist, one byte each.
func binariesPresent(bins []string) string {
	present := make([]byte, len(bins))
	for i, bin := range bins {
		present[i] = '0'
		if _, err := os.Stat(bin); err == nil {
			present[i] = '1'
		}
	}
	return string(present)
}

// dialogHookBinaries are the amq binaries the AMQ dialog hook entries of
// the harness name run.
func dialogHookBinaries(name string) []string {
	var commands []string
	var re *regexp.Regexp
	switch name {
	case harness.Claude:
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		for _, event := range wakeDialogHookEvents {
			found, _ := claude.MarkedHookCommands(home, event, wakeDialogHookMarker)
			commands = append(commands, found...)
		}
		re = wakeDialogHookBinRe
	case harness.Codex:
		path, err := codexHooksPath()
		if err != nil {
			return nil
		}
		entries, _ := readCodexHookEntries(path)
		for _, list := range entries {
			for _, entry := range list {
				commands = append(commands, entry.Command)
			}
		}
		re = codexDialogHookBinRe
	case harness.Cursor:
		path, err := cursorHooksPath()
		if err != nil {
			return nil
		}
		for _, event := range cursorDialogHookEvents {
			found, _ := claude.MarkedCursorHookCommandsAt(path, event, wakeDialogHookMarker)
			commands = append(commands, found...)
		}
		re = cursorDialogHookBinRe
	default:
		return nil
	}
	var bins []string
	for _, command := range commands {
		if match := re.FindStringSubmatch(command); match != nil {
			if bin := strings.ReplaceAll(match[1], `'\''`, `'`); !slices.Contains(bins, bin) {
				bins = append(bins, bin)
			}
		}
	}
	return bins
}

// dialogGuardStamp names the current version of the files that decide the
// dialog guard of a harness: path, mtime and size of each.
func dialogGuardStamp(name string) string {
	var paths []string
	switch name {
	case harness.Claude:
		if home, err := os.UserHomeDir(); err == nil {
			paths = append(paths, claude.SettingsPath(home))
		}
	case harness.Codex:
		if path, err := codexHooksPath(); err == nil {
			paths = append(paths, path, filepath.Join(filepath.Dir(path), "config.toml"), codexRequirementsPath)
		}
	case harness.Cursor:
		if path, err := cursorHooksPath(); err == nil {
			paths = append(paths, path)
		}
	}
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(path)
		if info, err := os.Stat(path); err == nil {
			fmt.Fprintf(&b, "\x00%d\x00%d", info.ModTime().UnixNano(), info.Size())
		}
		b.WriteByte(0)
	}
	return b.String()
}

// wakeDialogMarkerHarness is the harness that wrote a marker: its harness
// field, or Claude Code for a dialog hook marker, which names none. It is
// "" when the marker cannot be parsed.
func wakeDialogMarkerHarness(raw []byte) string {
	var marker wakeDialogMarker
	if json.Unmarshal(raw, &marker) != nil {
		return ""
	}
	if marker.Harness == "" && marker.Event != wakeDialogStartupEvent {
		return harness.Claude
	}
	return marker.Harness
}
