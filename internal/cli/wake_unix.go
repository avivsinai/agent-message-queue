//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

// wakeLock represents the lock file content for wake process deduplication.
type wakeLock struct {
	PID          int      `json:"pid"`
	TTY          string   `json:"tty"`
	Root         string   `json:"root"`                    // Absolute path to disambiguate relative AM_ROOT
	Agent        string   `json:"agent,omitempty"`         // Agent handle that owns this lock
	Hostname     string   `json:"hostname,omitempty"`      // Host that created the lock
	Started      string   `json:"started"`                 // Wall-clock diagnostic timestamp
	ProcessStart string   `json:"process_start,omitempty"` // Kernel process start token, guards PID reuse
	BootID       string   `json:"boot_id,omitempty"`       // Boot identity paired with ProcessStart when available
	Executable   string   `json:"executable,omitempty"`    // Diagnostic process executable basename/path
	Args         []string `json:"args,omitempty"`          // Diagnostic argv when available
}

type wakeProcessInfo struct {
	PID          int
	Running      bool
	StartToken   string
	BootID       string
	Executable   string
	Args         []string
	InspectError error
}

type wakeLockStatus string

const (
	wakeLockMissing    wakeLockStatus = "missing"
	wakeLockValid      wakeLockStatus = "valid"
	wakeLockStale      wakeLockStatus = "stale"
	wakeLockCreating   wakeLockStatus = "creating"
	wakeLockUnverified wakeLockStatus = "unverified"
)

type wakeLockInspection struct {
	Exists            bool
	Status            wakeLockStatus
	Reason            string
	Root              string
	Agent             string
	LockPath          string
	PID               int
	Lock              wakeLock
	Process           wakeProcessInfo
	IdentityConfirmed bool
	raw               []byte
}

var inspectWakeProcess = inspectWakeProcessPlatform

// acquireWakeLock attempts to acquire the wake lock for an agent's inbox.
// Returns cleanup function and error. If another wake is running, returns error.
func acquireWakeLock(root, me string) (cleanup func(), err error) {
	agentBase := fsq.AgentBase(root, me)
	lockPath := filepath.Join(agentBase, ".wake.lock")

	// Ensure agent directory exists before attempting lock
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create agent directory: %w", err)
	}

	// Check existing lock
	inspection := inspectWakeLock(root, me)
	if inspection.Exists {
		switch inspection.Status {
		case wakeLockStale:
			if err := removeWakeLockIfUnchanged(inspection); err != nil {
				return nil, err
			}
		case wakeLockCreating:
			return nil, fmt.Errorf("wake lock is being created (retry shortly)")
		case wakeLockValid:
			if shouldReplaceOrphanedWakeLock(inspection) {
				goto createLock
			}
			return nil, wakeLockAlreadyRunningError(me, inspection.Lock)
		case wakeLockUnverified:
			return nil, fmt.Errorf("wake lock for %s is unverified (pid %d on %s since %s): %s; run 'amq doctor --ops' for details",
				me, inspection.Lock.PID, inspection.Lock.TTY, inspection.Lock.Started, inspection.Reason)
		}
	}

createLock:
	// Get TTY name - reuse getCurrentTTY for consistency
	ttyName := getCurrentTTY()
	if ttyName == "" {
		ttyName = "unknown"
	}

	// Write lock atomically using O_EXCL to prevent race conditions
	lock := wakeLock{
		PID:     os.Getpid(),
		TTY:     ttyName,
		Root:    canonicalWakeRoot(root),
		Agent:   me,
		Started: time.Now().UTC().Format(time.RFC3339),
	}
	if hostname, err := os.Hostname(); err == nil {
		lock.Hostname = hostname
	}
	if proc := inspectWakeProcess(os.Getpid()); proc.Running {
		lock.ProcessStart = proc.StartToken
		lock.BootID = proc.BootID
		lock.Executable = proc.Executable
		lock.Args = proc.Args
	}
	lockData, _ := json.Marshal(lock)

	// Use O_EXCL for atomic creation - fails if file exists (race protection)
	f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Another process won the race - re-read to get its info
			if data, readErr := os.ReadFile(lockPath); readErr == nil {
				var winner wakeLock
				if json.Unmarshal(data, &winner) == nil {
					return nil, fmt.Errorf("wake already running for %s (pid %d on %s since %s)",
						me, winner.PID, winner.TTY, winner.Started)
				}
			}
			return nil, fmt.Errorf("wake lock exists for %s (concurrent start)", me)
		}
		return nil, fmt.Errorf("failed to create wake lock: %w", err)
	}
	_, writeErr := f.Write(lockData)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("failed to write wake lock: %w", writeErr)
	}
	if closeErr != nil {
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("failed to close wake lock: %w", closeErr)
	}

	cleanup = func() {
		// Only remove if it's still our lock
		if data, err := os.ReadFile(lockPath); err == nil {
			var current wakeLock
			if json.Unmarshal(data, &current) == nil && currentWakeLockMatches(current) {
				_ = os.Remove(lockPath)
			}
		}
	}

	return cleanup, nil
}

func inspectWakeLock(root, me string) wakeLockInspection {
	lockPath := filepath.Join(fsq.AgentBase(root, me), ".wake.lock")
	inspection := wakeLockInspection{
		Status:   wakeLockMissing,
		Root:     canonicalWakeRoot(root),
		Agent:    me,
		LockPath: lockPath,
	}

	data, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return inspection
		}
		inspection.Exists = true
		inspection.Status = wakeLockUnverified
		inspection.Reason = fmt.Sprintf("cannot read lock: %v", err)
		return inspection
	}

	inspection.Exists = true
	inspection.raw = data
	var existing wakeLock
	if err := json.Unmarshal(data, &existing); err != nil {
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) < 2*time.Second {
			inspection.Status = wakeLockCreating
			inspection.Reason = "lock is being created"
			return inspection
		}
		inspection.Status = wakeLockStale
		inspection.Reason = "invalid lock json"
		return inspection
	}

	inspection.Lock = existing
	inspection.PID = existing.PID
	inspection.Process = inspectWakeProcess(existing.PID)
	classifyWakeLock(root, me, &inspection)
	return inspection
}

func classifyWakeLock(root, me string, inspection *wakeLockInspection) {
	lock := inspection.Lock
	if lock.PID <= 0 {
		inspection.Status = wakeLockStale
		inspection.Reason = "invalid pid"
		return
	}
	if strings.TrimSpace(lock.Root) == "" {
		inspection.Status = wakeLockStale
		inspection.Reason = "lock root missing"
		return
	}
	if canonicalWakeRoot(lock.Root) != canonicalWakeRoot(root) {
		inspection.Status = wakeLockStale
		inspection.Reason = "root mismatch"
		return
	}
	if lock.Agent != "" && lock.Agent != me {
		inspection.Status = wakeLockStale
		inspection.Reason = "agent mismatch"
		return
	}
	if lock.Hostname != "" {
		if hostname, err := os.Hostname(); err == nil && hostname != "" && lock.Hostname != hostname {
			inspection.Status = wakeLockUnverified
			inspection.Reason = "hostname mismatch"
			return
		}
	}

	proc := inspection.Process
	if !proc.Running {
		inspection.Status = wakeLockStale
		inspection.Reason = "pid not running"
		return
	}
	if lock.ProcessStart != "" {
		if proc.StartToken == "" {
			inspection.Status = wakeLockUnverified
			inspection.Reason = inspectionReason("process start time unavailable", proc.InspectError)
			return
		}
		if lock.BootID != "" && proc.BootID != "" && lock.BootID != proc.BootID {
			inspection.Status = wakeLockStale
			inspection.Reason = "boot id mismatch"
			return
		}
		if lock.ProcessStart != proc.StartToken {
			inspection.Status = wakeLockStale
			inspection.Reason = "process start time mismatch"
			return
		}
	}
	if proc.Executable == "" {
		inspection.Status = wakeLockUnverified
		inspection.Reason = inspectionReason("process identity unavailable", proc.InspectError)
		return
	}
	if !processLooksLikeAMQ(proc) {
		inspection.Status = wakeLockStale
		inspection.Reason = "pid is not amq"
		return
	}
	if len(proc.Args) > 0 && !processArgsLookLikeWake(proc.Args) {
		inspection.Status = wakeLockStale
		inspection.Reason = "pid is not amq wake"
		return
	}

	if lock.ProcessStart != "" {
		inspection.IdentityConfirmed = true
		inspection.Status = wakeLockValid
		return
	}

	if wakeArgsMatchRootAgent(proc.Args, root, me) {
		inspection.IdentityConfirmed = true
		inspection.Status = wakeLockValid
		return
	}

	inspection.Status = wakeLockUnverified
	inspection.Reason = "legacy lock lacks process start metadata"
}

func removeWakeLockIfUnchanged(inspection wakeLockInspection) error {
	current, err := os.ReadFile(inspection.LockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("re-read wake lock before removal: %w", err)
	}
	if !bytes.Equal(current, inspection.raw) {
		return fmt.Errorf("wake lock changed while cleaning stale lock; retry")
	}
	if err := os.Remove(inspection.LockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale wake lock: %w", err)
	}
	return nil
}

func shouldReplaceOrphanedWakeLock(inspection wakeLockInspection) bool {
	if !inspection.IdentityConfirmed {
		return false
	}
	existing := inspection.Lock

	// Process is a confirmed matching amq wake. If its TTY disappeared, stop
	// that orphan before taking over; never signal an unconfirmed PID.
	if strings.HasPrefix(existing.TTY, "/dev/") {
		if _, statErr := os.Stat(existing.TTY); os.IsNotExist(statErr) {
			return replaceConfirmedOrphanedWakeLock(inspection)
		}
	}

	currentTTY := getCurrentTTY()
	existingTTY := existing.TTY
	if strings.HasPrefix(existingTTY, "/dev/") {
		if real, err := filepath.EvalSymlinks(existingTTY); err == nil {
			existingTTY = real
		}
	}
	if currentTTY != "" && currentTTY == existingTTY {
		existingSid, sidErr := unix.Getsid(existing.PID)
		currentSid, _ := unix.Getsid(0)
		if sidErr == nil && existingSid != currentSid {
			return replaceConfirmedOrphanedWakeLock(inspection)
		}
	}
	return false
}

func replaceConfirmedOrphanedWakeLock(inspection wakeLockInspection) bool {
	if !terminateWakeProcessIfStillConfirmed(inspection) {
		return false
	}
	return removeWakeLockIfUnchanged(inspection) == nil
}

func terminateWakeProcessIfStillConfirmed(inspection wakeLockInspection) bool {
	if !sameConfirmedWakeLock(inspection) {
		return false
	}
	pid := inspection.PID
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGTERM)
		time.Sleep(100 * time.Millisecond)
		if processAlive(pid) {
			if !sameConfirmedWakeLock(inspection) {
				return false
			}
			_ = proc.Signal(syscall.SIGKILL)
		}
	}
	return true
}

func sameConfirmedWakeLock(inspection wakeLockInspection) bool {
	recheck := inspectWakeLock(inspection.Root, inspection.Agent)
	return recheck.Exists &&
		recheck.Status == wakeLockValid &&
		recheck.IdentityConfirmed &&
		recheck.PID == inspection.PID &&
		bytes.Equal(recheck.raw, inspection.raw)
}

func currentWakeLockMatches(lock wakeLock) bool {
	if lock.PID != os.Getpid() {
		return false
	}
	if lock.ProcessStart == "" {
		return true
	}
	proc := inspectWakeProcess(os.Getpid())
	if !proc.Running || proc.StartToken == "" {
		return false
	}
	if lock.BootID != "" && proc.BootID != "" && lock.BootID != proc.BootID {
		return false
	}
	return lock.ProcessStart == proc.StartToken
}

func canonicalWakeRoot(root string) string {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}
	if realRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = realRoot
	}
	return filepath.Clean(absRoot)
}

func wakeLockAlreadyRunningError(me string, lock wakeLock) error {
	return fmt.Errorf("wake already running for %s (pid %d on %s since %s)",
		me, lock.PID, lock.TTY, lock.Started)
}

func inspectionReason(base string, err error) string {
	if err == nil {
		return base
	}
	return fmt.Sprintf("%s: %v", base, err)
}

func processLooksLikeAMQ(proc wakeProcessInfo) bool {
	if isAMQExecutable(proc.Executable) {
		return true
	}
	if len(proc.Args) > 0 && isAMQExecutable(proc.Args[0]) {
		return true
	}
	return false
}

func processArgsLookLikeWake(args []string) bool {
	if len(args) < 2 {
		return false
	}
	for _, arg := range args[1:] {
		if arg == "wake" {
			return true
		}
	}
	return false
}

func wakeArgsMatchRootAgent(args []string, root, me string) bool {
	if !processArgsLookLikeWake(args) {
		return false
	}
	rootMatch := false
	meMatch := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--root" && i+1 < len(args):
			rootMatch = canonicalWakeRoot(args[i+1]) == canonicalWakeRoot(root)
			i++
		case strings.HasPrefix(arg, "--root="):
			rootMatch = canonicalWakeRoot(strings.TrimPrefix(arg, "--root=")) == canonicalWakeRoot(root)
		case arg == "--me" && i+1 < len(args):
			meMatch = args[i+1] == me
			i++
		case strings.HasPrefix(arg, "--me="):
			meMatch = strings.TrimPrefix(arg, "--me=") == me
		}
	}
	return rootMatch && meMatch
}

func isAMQExecutable(value string) bool {
	base := filepath.Base(strings.Trim(value, `"'`))
	return base == "amq"
}

// processAlive checks if a process with given PID is running.
func processAlive(pid int) bool {
	// Guard against invalid PIDs - pid<=0 would signal process group
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds; send signal 0 to check.
	// ESRCH => process doesn't exist (dead).
	// EPERM => process exists but we lack permission (alive).
	// nil   => process exists and we can signal it (alive).
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EPERM) {
		return true // process exists, just can't signal it
	}
	return false // ESRCH or other error => treat as dead
}

type wakeLoopFunc func(wakeConfig) error

func runWake(args []string) error {
	return runWakeWithLoop(args, runWakeLoop)
}

func runWakeWithLoop(args []string, loop wakeLoopFunc) error {
	fs := flag.NewFlagSet("wake", flag.ContinueOnError)
	common := addCommonFlags(fs)
	injectCmdFlag := fs.String("inject-cmd", "", "Command to inject (power user mode)")
	injectViaFlag := fs.String("inject-via", "", "External executable for injection (payload appended as last arg, bypasses TTY requirement)")
	var injectArgFlags multiStringFlag
	fs.Var(&injectArgFlags, "inject-arg", "Argument for --inject-via before the payload (repeatable)")
	injectTimeoutFlag := fs.Duration("inject-timeout", defaultInjectTimeout, "Timeout for one --inject-via command")
	bellFlag := fs.Bool("bell", false, "Ring terminal bell on new messages")
	debounceFlag := fs.Duration("debounce", 250*time.Millisecond, "Debounce window for batching messages")
	previewLenFlag := fs.Int("preview-len", 48, "Max subject preview length")
	injectModeFlag := fs.String("inject-mode", "auto", "Injection mode: auto, raw, paste (auto detects CLI type)")
	deferWhileInputFlag := fs.Bool("defer-while-input", true, "Best-effort: defer non-interrupt injection while terminal input appears active")
	inputQuietForFlag := fs.Duration("input-quiet-for", 1200*time.Millisecond, "Quiet window before deferred injection (best-effort; Linux tty atime granularity is ~8s)")
	inputPollIntervalFlag := fs.Duration("input-poll-interval", 200*time.Millisecond, "Polling interval while waiting for quiet terminal input")
	inputMaxHoldFlag := fs.Duration("input-max-hold", 15*time.Second, "Maximum time to defer one wake injection (0 = no hold)")
	interruptFlag := fs.Bool("interrupt", true, "Enable interrupt injection for urgent interrupt messages")
	interruptLabelFlag := fs.String("interrupt-label", "interrupt", "Label required to trigger interrupt")
	interruptPriorityFlag := fs.String("interrupt-priority", "urgent", "Priority required to trigger interrupt")
	interruptCmdFlag := fs.String("interrupt-cmd", "ctrl-c", "Interrupt command to inject (ctrl-c or none)")
	interruptNoticeFlag := fs.String("interrupt-notice", "", "Custom interrupt notice (default: auto)")
	interruptCooldownFlag := fs.Duration("interrupt-cooldown", 7*time.Second, "Minimum time between interrupts")
	readyFileFlag := fs.String("ready-file", "", "Internal: write this file after wake lock acquisition")
	debugFlag := fs.Bool("debug", false, "Log injection diagnostics to stderr")

	usage := usageWithFlags(fs, "amq wake --me <agent> [options]",
		"Background waker: injects terminal notification when messages arrive.",
		"Run as background job before starting CLI: amq wake --me claude &",
		"",
		"Inject modes:",
		"  auto  - Detect CLI type: raw for Claude Code/Codex, paste for others",
		"  raw   - Plain text + CR, no bracketed paste (works with Ink-based CLIs)",
		"  paste - Bracketed paste with delayed CR (works with crossterm-based CLIs)",
		"",
		"External injection:",
		"  --inject-via runs a local executable for each notification, bypassing",
		"  the TIOCSTI/stdin-TTY startup requirement. Fixed arguments use repeatable",
		"  --inject-arg; AMQ appends the sanitized notification payload as the",
		"  final argv element. The command is not run through a shell.",
		"  Example: amq wake --me orchestrator --inject-via ghostty-bridge \\",
		"    --inject-arg exec --inject-arg \"$TERMINAL_ID\"",
		"  Trust boundary: --inject-via executes local code, and the payload can",
		"  contain sanitized but message-derived header content.",
		"",
		"Input deferral (default on): wake samples terminal input only after",
		"  a message is pending, then injects after a short quiet window.",
		"  Best-effort only: a pause longer than --input-quiet-for can still",
		"  inject while a prompt is being composed. Interrupt messages bypass it.",
		"  Atime sampling uses stdin (when a TTY) for cross-platform fidelity;",
		"  Linux tty atime is updated at ~8s granularity, so quiet windows",
		"  shorter than that are advisory.",
		"",
		"Interrupts (default on): urgent messages tagged with label \"interrupt\"",
		"  trigger Ctrl+C injection + an interrupt notice.",
		"",
		"EXPERIMENTAL: Uses TIOCSTI ioctl (macOS/Linux). May not work on all systems.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *previewLenFlag < 0 {
		return UsageError("--preview-len must be >= 0")
	}
	if *debounceFlag < 0 {
		return UsageError("--debounce must be >= 0")
	}
	if *interruptCooldownFlag < 0 {
		return UsageError("--interrupt-cooldown must be >= 0")
	}
	if *inputQuietForFlag < 0 {
		return UsageError("--input-quiet-for must be >= 0")
	}
	if *inputPollIntervalFlag <= 0 {
		return UsageError("--input-poll-interval must be > 0")
	}
	if *inputMaxHoldFlag < 0 {
		return UsageError("--input-max-hold must be >= 0")
	}
	if *injectTimeoutFlag <= 0 {
		return UsageError("--inject-timeout must be > 0")
	}

	injectMode := strings.ToLower(strings.TrimSpace(*injectModeFlag))
	if injectMode == "" {
		injectMode = "auto"
	}
	switch injectMode {
	case "auto", "raw", "paste":
		// ok
	default:
		return UsageError("invalid --inject-mode %q (supported: auto, raw, paste)", *injectModeFlag)
	}

	interruptLabel := strings.TrimSpace(*interruptLabelFlag)
	interruptPriority := strings.ToLower(strings.TrimSpace(*interruptPriorityFlag))
	if *interruptFlag && interruptLabel == "" {
		return UsageError("interrupt-label is required when interrupt is enabled")
	}
	if *interruptFlag && interruptPriority == "" {
		return UsageError("interrupt-priority is required when interrupt is enabled")
	}
	if *interruptFlag && !format.IsValidPriority(interruptPriority) {
		return UsageError("--interrupt-priority must be one of: urgent, normal, low")
	}

	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}

	root := resolveRoot(common.Root)
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	// Validate --inject-via: it is an executable path, not a shell command line.
	injectVia := strings.TrimSpace(*injectViaFlag)
	if *injectViaFlag != "" && injectVia == "" {
		return UsageError("--inject-via must not be blank")
	}
	if injectVia == "" && len(injectArgFlags) > 0 {
		return UsageError("--inject-arg requires --inject-via")
	}
	readyFile := strings.TrimSpace(*readyFileFlag)
	if *readyFileFlag != "" && readyFile == "" {
		return UsageError("--ready-file must not be blank")
	}

	// Verify TIOCSTI is available (skip in inject-via mode — uses external command instead)
	if injectVia == "" {
		if !tiocsti.Available() {
			return errors.New("TIOCSTI not available on this platform; use tmux send-keys or terminal-specific injection")
		}

		// Verify we have a real TTY
		if !tiocsti.IsTTY() {
			return errors.New("amq wake requires a real terminal (run in foreground or as background job in same terminal, or use --inject-via for external injection)")
		}
	}

	interruptKey, err := parseInterruptKey(*interruptCmdFlag)
	if err != nil {
		return UsageError("%v", err)
	}

	// Acquire lock to prevent duplicate wake processes
	cleanup, err := acquireWakeLock(root, me)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg := wakeConfig{
		me:                me,
		root:              root,
		session:           resolveSessionName(root),
		injectCmd:         *injectCmdFlag,
		injectVia:         injectVia,
		injectArgs:        []string(injectArgFlags),
		injectTimeout:     *injectTimeoutFlag,
		bell:              *bellFlag,
		debounce:          *debounceFlag,
		previewLen:        *previewLenFlag,
		strict:            common.Strict,
		fallbackWarn:      true,
		injectMode:        injectMode,
		debug:             *debugFlag,
		deferWhileInput:   *deferWhileInputFlag,
		inputQuietFor:     *inputQuietForFlag,
		inputPollInterval: *inputPollIntervalFlag,
		inputMaxHold:      *inputMaxHoldFlag,
		interrupt:         *interruptFlag,
		interruptLabel:    interruptLabel,
		interruptPriority: interruptPriority,
		interruptKey:      interruptKey,
		interruptNotice:   strings.TrimSpace(*interruptNoticeFlag),
		interruptCooldown: *interruptCooldownFlag,
	}

	if err := writeWakeReadyFile(readyFile); err != nil {
		return err
	}

	return loop(cfg)
}

func writeWakeReadyFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
		return fmt.Errorf("write wake ready file: %w", err)
	}
	return nil
}

func parseInterruptKey(raw string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" {
		normalized = "ctrl-c"
	}
	switch normalized {
	case "ctrl-c", "sigint":
		return "\x03", nil
	case "none", "off", "false":
		return "", nil
	default:
		return "", fmt.Errorf("invalid interrupt-cmd %q (use ctrl-c or none)", raw)
	}
}

func runWakeLoop(cfg wakeConfig) error {
	inboxNew := fsq.AgentInboxNew(cfg.root, cfg.me)

	// Ensure inbox exists
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		return err
	}

	// Set up watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(inboxNew); err != nil {
		return fmt.Errorf("failed to watch inbox: %w", err)
	}

	// Ignore job control signals so background job can operate freely.
	// Note: This also affects foreground mode (Ctrl+Z won't suspend), but wake
	// is designed to run as a background job (amq wake &) so this is intentional.
	// - SIGTTOU: allow writing to TTY from background
	// - SIGTSTP: prevent Ctrl+Z or shell from suspending us
	// - SIGTTIN: prevent suspension if stdin is accidentally read
	signal.Ignore(syscall.SIGTTOU, syscall.SIGTSTP, syscall.SIGTTIN)

	// Handle signals gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Debounce timer
	var debounceTimer *time.Timer
	pendingNotify := false

	// TTY health check timer - verify we can still inject every 30s
	healthTicker := time.NewTicker(30 * time.Second)
	defer healthTicker.Stop()

	// Touch presence immediately so `amq who` shows agent as active
	_ = presence.Touch(cfg.root, cfg.me)

	// Notify if messages already exist
	if err := notifyNewMessages(&cfg); err != nil {
		_ = writeStderr("amq wake: notify error: %v\n", err)
	}

	for {
		var debounceC <-chan time.Time
		if debounceTimer != nil {
			debounceC = debounceTimer.C
		}

		select {
		case <-sigCh:
			// Clean exit on SIGHUP/SIGTERM
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return errors.New("watcher closed")
			}
			// Only care about new files
			if event.Op&(fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			// Skip non-.md files
			if !strings.HasSuffix(event.Name, ".md") {
				continue
			}

			// Start or reset debounce timer
			pendingNotify = true
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(cfg.debounce)
			} else {
				if !debounceTimer.Stop() {
					select {
					case <-debounceTimer.C:
					default:
					}
				}
			}
			debounceTimer.Reset(cfg.debounce)

		case err, ok := <-watcher.Errors:
			if !ok {
				return errors.New("watcher closed")
			}
			_ = writeStderr("amq wake: watcher error: %v\n", err)

		case <-debounceC:
			if !pendingNotify {
				continue
			}
			pendingNotify = false

			// Collect and notify
			if err := notifyNewMessages(&cfg); err != nil {
				_ = writeStderr("amq wake: notify error: %v\n", err)
			}

		case <-healthTicker.C:
			// Keep presence alive so `amq who` reports the agent as active
			_ = presence.Touch(cfg.root, cfg.me)

			if err := wakeHealthCheck(cfg, ttyAvailable); err != nil {
				return err
			}
		}
	}
}

func wakeHealthCheck(cfg wakeConfig, ttyAvailableFn func() bool) error {
	if cfg.injectVia != "" {
		return nil
	}
	if !ttyAvailableFn() {
		return errors.New("TTY no longer available")
	}
	return nil
}

func ttyAvailable() bool {
	// Mirrors injection path: if /dev/tty can't be opened, wake can't inject.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}

// getCurrentTTY returns the normalized path to the current controlling terminal.
func getCurrentTTY() string {
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err != nil {
		return ""
	}
	defer func() { _ = tty.Close() }()
	if link, err := os.Readlink(fmt.Sprintf("/dev/fd/%d", tty.Fd())); err == nil {
		// Normalize symlinks for reliable comparison
		if real, err := filepath.EvalSymlinks(link); err == nil {
			return real
		}
		return link
	}
	return ""
}
