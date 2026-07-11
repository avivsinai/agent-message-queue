//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
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

var (
	signalWakeProcess = func(pid int, sig os.Signal) error {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		return proc.Signal(sig)
	}
	wakeTerminateGrace = 100 * time.Millisecond
	getWakeCurrentTTY  = getCurrentTTY
	getWakeProcessSID  = unix.Getsid
)

type wakeRepairResult struct {
	Status          string `json:"status"`
	Agent           string `json:"agent"`
	Root            string `json:"root"`
	Lock            string `json:"lock"`
	Target          string `json:"target,omitempty"`
	PID             int    `json:"pid,omitempty"`
	Reason          string `json:"reason,omitempty"`
	RepairAvailable bool   `json:"repair_available,omitempty"`
}

type wakeRepairStarter func(root, me string, target wakeTarget) (int, error)

type wakeLockAcquireOptions struct {
	acceptExistingValid bool
	target              *wakeTarget
	wakeMode            string
}

var startWakeFromTarget = startWakeFromTargetDefault

// acquireWakeLock attempts to acquire the wake lock for an agent's inbox.
// Returns cleanup function and error. If another wake is running, returns error.
func acquireWakeLock(root, me string, target *wakeTarget) (cleanup func(), err error) {
	return acquireWakeLockWithOptions(root, me, wakeLockAcquireOptions{target: target})
}

func acquireWakeLockWithOptions(root, me string, options wakeLockAcquireOptions) (cleanup func(), err error) {
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
			if err := validateWakeLockStaleRemoval(inspection); err != nil {
				return nil, err
			}
			if err := removeWakeLockIfUnchanged(inspection); err != nil {
				return nil, err
			}
		case wakeLockCreating:
			return nil, fmt.Errorf("wake lock is being created (retry shortly)")
		case wakeLockValid:
			if options.acceptExistingValid {
				if err := requireWakeLockUsable(inspection, options.wakeMode); err != nil {
					return nil, err
				}
				return nil, wakeLockAlreadyRunningError(me, inspection)
			}
			replace, replaceErr := shouldReplaceOrphanedWakeLock(inspection)
			if replaceErr != nil {
				return nil, replaceErr
			}
			if replace {
				goto createLock
			}
			return nil, wakeLockAlreadyRunningError(me, inspection)
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
	if options.target != nil {
		lock.WakeMode = wakeTargetInjectVia
		lock.TargetDigest = wakeTargetDigest(*options.target)
	} else if options.wakeMode == wakeInjectModeNone {
		lock.WakeMode = wakeInjectModeNone
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
			// Another process won the race; reuse only if the shared
			// classifier proves it is the matching wake.
			winner := inspectWakeLock(root, me)
			if winner.Status == wakeLockValid {
				if options.acceptExistingValid {
					if usableErr := requireWakeLockUsable(winner, options.wakeMode); usableErr != nil {
						return nil, usableErr
					}
				}
				return nil, wakeLockAlreadyRunningError(me, winner)
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

func shouldReplaceOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	if !wakeLockNeedsReplacement(inspection) {
		replace, err := wakeLockNeedsOwnerReplacement(inspection)
		if err != nil || !replace {
			return replace, err
		}
	}
	return replaceConfirmedOrphanedWakeLock(inspection)
}

func wakeLockNeedsReplacement(inspection wakeLockInspection) bool {
	if !inspection.IdentityConfirmed {
		return false
	}
	existing := inspection.Lock

	// Process is a confirmed matching amq wake. If its TTY disappeared, stop
	// that orphan before taking over; never signal an unconfirmed PID.
	if strings.HasPrefix(existing.TTY, "/dev/") {
		if _, statErr := os.Stat(existing.TTY); os.IsNotExist(statErr) {
			return true
		}
	}

	currentTTY := getWakeCurrentTTY()
	existingTTY := existing.TTY
	if strings.HasPrefix(existingTTY, "/dev/") {
		if real, err := filepath.EvalSymlinks(existingTTY); err == nil {
			existingTTY = real
		}
	}
	if currentTTY != "" && currentTTY == existingTTY {
		existingSid, sidErr := getWakeProcessSID(existing.PID)
		currentSid, _ := getWakeProcessSID(0)
		if sidErr == nil && existingSid != currentSid {
			return true
		}
	}
	return false
}

func wakeLockNeedsOwnerReplacement(inspection wakeLockInspection) (bool, error) {
	if !inspection.IdentityConfirmed || inspection.Lock.WakeMode != wakeTargetInjectVia || inspection.Lock.TargetDigest == "" {
		return false, nil
	}
	target, exists, err := readWakeTarget(inspection.Root, inspection.Agent)
	if err != nil || !exists {
		return false, nil
	}
	if err := validateWakeTargetMatchesLock(inspection.Lock, target); err != nil {
		return false, nil
	}
	if target.Owner == nil {
		return false, nil
	}
	return wakeOwnerHealthCheck(*target.Owner) != nil, nil
}

func requireWakeLockUsable(inspection wakeLockInspection, requiredMode string) error {
	if !inspection.Exists || inspection.Status != wakeLockValid || !inspection.IdentityConfirmed {
		return fmt.Errorf("existing wake lock for %s is not a confirmed valid wake", inspection.Agent)
	}
	if requiredMode == wakeInjectModeNone && inspection.Lock.WakeMode != wakeInjectModeNone {
		return fmt.Errorf("existing wake for %s cannot satisfy requested --inject-mode none; stop the existing wake and retry", inspection.Agent)
	}
	if !wakeLockHasUsableNotificationPath(inspection) {
		return fmt.Errorf("existing wake lock for %s is not usable for --require-wake (pid %d on %s since %s)",
			inspection.Agent, inspection.Lock.PID, inspection.Lock.TTY, inspection.Lock.Started)
	}
	if wakeLockNeedsReplacement(inspection) {
		return fmt.Errorf("existing wake lock for %s is not usable for --require-wake (pid %d on %s since %s)",
			inspection.Agent, inspection.Lock.PID, inspection.Lock.TTY, inspection.Lock.Started)
	}
	return nil
}

func wakeLockHasUsableNotificationPath(inspection wakeLockInspection) bool {
	if inspection.Lock.WakeMode == wakeInjectModeNone {
		return true
	}
	if inspection.Lock.WakeMode == wakeTargetInjectVia || wakeArgsUseInjectVia(inspection.Process.Args) {
		return true
	}
	tty := strings.TrimSpace(inspection.Lock.TTY)
	return tty != "" && tty != "unknown"
}

func wakeArgsUseInjectVia(args []string) bool {
	for _, arg := range args {
		if arg == "--inject-via" || strings.HasPrefix(arg, "--inject-via=") {
			return true
		}
	}
	return false
}

func replaceConfirmedOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	return terminateAndRemoveOrphanedWakeLock(inspection)
}

func terminateAndRemoveOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	recheck := inspectWakeLock(inspection.Root, inspection.Agent)
	if !sameWakeLockInspection(inspection, recheck) || !recheck.IdentityConfirmed {
		return false, nil
	}
	if err := terminateWakeProcess(recheck); err != nil {
		return false, err
	}
	if err := removeWakeLockIfUnchanged(recheck); err != nil {
		return false, err
	}
	return true, nil
}

func sameWakeLockInspection(first, second wakeLockInspection) bool {
	if !second.Exists || second.Status != wakeLockValid {
		return false
	}
	if first.PID != second.PID || first.Root != second.Root || first.Agent != second.Agent {
		return false
	}
	return bytes.Equal(first.raw, second.raw)
}

func terminateWakeProcess(inspection wakeLockInspection) error {
	if !sameConfirmedWakeLock(inspection) {
		return fmt.Errorf("wake process identity changed before SIGTERM")
	}
	if err := signalWakeProcess(inspection.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal wake process SIGTERM: %w", err)
	}
	time.Sleep(wakeTerminateGrace)
	if !wakeProcessStillMatches(inspection) {
		return nil
	}
	if !sameConfirmedWakeLock(inspection) {
		return fmt.Errorf("wake process identity changed before SIGKILL")
	}
	if err := signalWakeProcess(inspection.PID, syscall.SIGKILL); err != nil {
		return fmt.Errorf("signal wake process SIGKILL: %w", err)
	}
	deadline := time.Now().Add(wakeTerminateGrace)
	for wakeProcessStillMatches(inspection) {
		if time.Now().After(deadline) {
			return fmt.Errorf("wake process still alive after SIGKILL")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func sameConfirmedWakeLock(inspection wakeLockInspection) bool {
	recheck := inspectWakeLock(inspection.Root, inspection.Agent)
	return sameWakeLockInspection(inspection, recheck) && recheck.IdentityConfirmed
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
	if len(args) > 0 && args[0] == "repair" {
		return runWakeRepair(args[1:])
	}
	return runWakeWithLoop(args, runWakeLoop)
}

func runWakeRepair(args []string) error {
	fs := flag.NewFlagSet("wake repair", flag.ContinueOnError)
	common := addCommonFlags(fs)
	usage := usageWithFlags(fs, "amq wake repair --me <agent> [options]",
		"Repair a proven-stale wake by restarting it from a saved inject-via target.",
		"",
		"Refuses unverified locks and raw terminal wake targets. This command only",
		"uses .wake.target files created for --inject-via wake processes.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
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
	result, repairErr := repairWake(root, me)
	if common.JSON {
		if err := writeJSON(os.Stdout, result); err != nil {
			return err
		}
		return repairErr
	}
	line := fmt.Sprintf("wake repair: %s agent=%s root=%s", result.Status, result.Agent, result.Root)
	if result.PID != 0 {
		line += fmt.Sprintf(" pid=%d", result.PID)
	}
	if result.Reason != "" {
		line += " reason=" + result.Reason
	}
	if err := writeStdoutLine(line); err != nil {
		return err
	}
	return repairErr
}

func repairWake(root, me string) (wakeRepairResult, error) {
	result := wakeRepairResult{
		Status: "unknown",
		Agent:  me,
		Root:   canonicalWakeRoot(root),
		Lock:   filepath.Join(fsq.AgentBase(root, me), ".wake.lock"),
		Target: wakeTargetPath(root, me),
	}
	inspection := inspectWakeLock(root, me)
	if !inspection.Exists {
		result.Status = "refused"
		result.Reason = "no wake lock present; start wake normally"
		return result, errors.New(result.Reason)
	}
	switch inspection.Status {
	case wakeLockValid:
		result.Status = "refused"
		result.PID = inspection.PID
		result.Reason = "wake lock is already valid; refusing repair"
		return result, errors.New(result.Reason)
	case wakeLockStale:
		if err := validateWakeLockRepairable(inspection); err != nil {
			result.Status = "refused"
			result.PID = inspection.PID
			result.Reason = err.Error()
			return result, err
		}
	case wakeLockCreating:
		result.Status = "refused"
		result.Reason = "wake lock is being created; retry shortly"
		return result, errors.New(result.Reason)
	case wakeLockUnverified:
		result.Status = "refused"
		result.PID = inspection.PID
		result.Reason = "wake lock is unverified; refusing to start a second injector"
		return result, fmt.Errorf("%s: %s", result.Reason, inspection.Reason)
	default:
		result.Status = "refused"
		result.Reason = fmt.Sprintf("wake lock status %q is not repairable", inspection.Status)
		return result, errors.New(result.Reason)
	}

	target, exists, err := readWakeTarget(root, me)
	if err != nil {
		result.Status = "refused"
		result.Reason = err.Error()
		return result, err
	}
	if !exists {
		result.Status = "refused"
		result.Reason = "no inject-via wake target; restart wake manually"
		return result, errors.New(result.Reason)
	}
	if err := validateWakeTarget(target, root, me); err != nil {
		result.Status = "refused"
		result.Reason = err.Error()
		return result, err
	}
	if err := validateWakeTargetMatchesLock(inspection.Lock, target); err != nil {
		result.Status = "refused"
		result.Reason = err.Error()
		return result, err
	}
	result.RepairAvailable = true
	clearRepairAvailable := func() {
		result.RepairAvailable = false
	}

	recheck := inspectWakeLock(root, me)
	if recheck.Status != wakeLockStale {
		clearRepairAvailable()
		result.Status = "refused"
		result.PID = recheck.PID
		result.Reason = "wake lock changed before repair"
		return result, errors.New(result.Reason)
	}
	if err := validateWakeLockRepairable(recheck); err != nil {
		clearRepairAvailable()
		result.Status = "refused"
		result.PID = recheck.PID
		result.Reason = "wake lock changed before repair"
		return result, errors.New(result.Reason)
	}
	if !bytes.Equal(inspection.raw, recheck.raw) {
		clearRepairAvailable()
		result.Status = "refused"
		result.PID = recheck.PID
		result.Reason = "wake lock changed before repair"
		return result, errors.New(result.Reason)
	}
	if err := validateWakeTargetMatchesLock(recheck.Lock, target); err != nil {
		clearRepairAvailable()
		result.Status = "refused"
		result.Reason = err.Error()
		return result, err
	}
	if err := removeWakeLockIfUnchanged(recheck); err != nil {
		clearRepairAvailable()
		result.Status = "error"
		result.Reason = err.Error()
		return result, err
	}
	pid, err := startWakeFromTarget(root, me, target)
	if err != nil {
		clearRepairAvailable()
		result.Status = "error"
		result.Reason = err.Error()
		return result, err
	}
	result.Status = "repaired"
	result.PID = pid
	return result, nil
}

func startWakeFromTargetDefault(root, me string, target wakeTarget) (int, error) {
	amqBin, err := os.Executable()
	if err != nil {
		amqBin = "amq"
	}
	readyPath, cleanupReady, err := newWakeReadyFile()
	if err != nil {
		return 0, fmt.Errorf("create wake readiness file: %w", err)
	}
	defer cleanupReady()
	args := buildRepairWakeArgs(root, me, target, readyPath)
	cmd := exec.Command(amqBin, args...)
	env, err := wakeCommandEnv(os.Environ(), root, target.Owner)
	if err != nil {
		return 0, err
	}
	cmd.Env = env
	output, err := openWakeRepairOutput(root, me)
	if err != nil {
		return 0, err
	}
	defer func() { _ = output.Close() }()
	configureRepairWakeCommand(cmd, output)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start repaired amq wake: %w", err)
	}
	if err := waitForWakeReady(cmd.Process, readyPath, wakeReadyTimeout); err != nil {
		_ = cmd.Process.Kill()
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func openWakeRepairOutput(root, me string) (*os.File, error) {
	agentBase := fsq.AgentBase(root, me)
	if err := os.MkdirAll(agentBase, 0o700); err != nil {
		return nil, fmt.Errorf("create repair wake log directory: %w", err)
	}
	path := filepath.Join(agentBase, ".wake.repair.log")
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("repair wake log %s must not be a symlink", path)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("repair wake log %s must be a regular file", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat repair wake log: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open repair wake log: %w", err)
	}
	if info, err := file.Stat(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat repair wake log: %w", err)
	} else if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("repair wake log %s must be a regular file", path)
	}
	return file, nil
}

func configureRepairWakeCommand(cmd *exec.Cmd, output *os.File) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = output
	cmd.Stderr = output
}

func buildRepairWakeArgs(root, me string, target wakeTarget, readyPath string) []string {
	args := []string{"--no-update-check", "wake", "--me", me, "--root", root, "--inject-via", target.InjectVia}
	for _, arg := range target.InjectArgs {
		args = append(args, "--inject-arg", arg)
	}
	return append(args, "--ready-file", readyPath)
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
	injectModeFlag := fs.String("inject-mode", wakeInjectModeAuto, "Injection mode: auto, raw, paste, none (auto detects CLI type)")
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
	acceptExistingWakeFlag := fs.Bool("accept-existing-wake", false, "Internal: allow a usable existing wake to satisfy readiness")

	usage := usageWithHiddenFlags(fs, "amq wake --me <agent> [options]",
		[]string{"ready-file", "accept-existing-wake"},
		"Background waker: injects terminal notification when messages arrive.",
		"Run as background job before starting CLI: amq wake --me claude &",
		"",
		"Inject modes:",
		"  auto  - Detect CLI type: raw for Claude Code/Codex, paste for others",
		"  raw   - Plain text + CR, no bracketed paste (works with Ink-based CLIs)",
		"  paste - Bracketed paste with delayed CR (works with crossterm-based CLIs)",
		"  none  - Output notice on wake stderr; zero terminal input injection",
		"          (urgent interrupts degrade to one bell + output notice)",
		"",
		"External injection:",
		"  --inject-via runs a local executable for each notification, bypassing",
		"  the TIOCSTI/stdin-TTY startup requirement. Fixed arguments use repeatable",
		"  --inject-arg; AMQ appends the sanitized notification payload as the",
		"  final argv element. The command is not run through a shell.",
		"  Example: amq wake --me orchestrator --inject-via /path/to/ghostty-bridge \\",
		"    --inject-arg exec --inject-arg \"$TERMINAL_ID\"",
		"  Trust boundary: --inject-via executes local code, and the payload can",
		"  contain sanitized but message-derived header content.",
		"",
		"Input deferral (default on): wake samples terminal input only after",
		"  a message is pending, then injects after a short quiet window.",
		"  Collision reduction only: it cannot detect permission/approval dialogs.",
		"  A pause longer than --input-quiet-for can still inject while a prompt",
		"  is being composed. Interrupt messages bypass it.",
		"  Atime sampling uses stdin (when a TTY) for cross-platform fidelity;",
		"  Linux tty atime is updated at ~8s granularity, so quiet windows",
		"  shorter than that are advisory.",
		"",
		"Interrupts (default on): urgent messages tagged with label \"interrupt\"",
		"  trigger Ctrl+C injection + an interrupt notice except in none mode.",
		"",
		"Safety: raw, paste, --inject-cmd, --inject-via, and interrupt Ctrl+C",
		"  can activate a focused permission/approval dialog. Use none when AMQ",
		"  must enforce zero synthetic input; stderr output may scribble until redraw.",
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

	injectMode, err := normalizeWakeInjectMode(*injectModeFlag)
	if err != nil {
		return UsageError("%v", err)
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

	injectVia := strings.TrimSpace(*injectViaFlag)
	if *injectViaFlag != "" && injectVia == "" {
		return UsageError("--inject-via must not be blank")
	}
	if injectMode == wakeInjectModeNone && injectVia != "" {
		return UsageError("--inject-via cannot be used with --inject-mode none")
	}
	if injectMode == wakeInjectModeNone && len(injectArgFlags) > 0 {
		return UsageError("--inject-arg cannot be used with --inject-mode none")
	}
	if injectMode == wakeInjectModeNone && *injectCmdFlag != "" {
		return UsageError("--inject-cmd cannot be used with --inject-mode none")
	}
	if injectVia == "" && len(injectArgFlags) > 0 {
		return UsageError("--inject-arg requires --inject-via")
	}
	readyFile := strings.TrimSpace(*readyFileFlag)
	if *readyFileFlag != "" && readyFile == "" {
		return UsageError("--ready-file must not be blank")
	}

	// Verify TIOCSTI is available (skip in inject-via mode — uses external command instead)
	if injectVia == "" && injectMode != wakeInjectModeNone {
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

	var target *wakeTarget
	if injectVia != "" {
		owner, err := wakeOwnerFromEnv()
		if err != nil {
			return err
		}
		value, err := newWakeTarget(root, me, injectVia, []string(injectArgFlags))
		if err != nil {
			return err
		}
		value.Owner = owner
		if err := validateWakeTarget(value, root, me); err != nil {
			return err
		}
		injectVia = value.InjectVia
		target = &value
	}

	// Acquire lock to prevent duplicate wake processes
	acceptExistingWake := readyFile != "" && *acceptExistingWakeFlag
	cleanup, err := acquireWakeLockWithOptions(root, me, wakeLockAcquireOptions{
		acceptExistingValid: acceptExistingWake,
		target:              target,
		wakeMode:            injectMode,
	})
	if err != nil {
		var alreadyRunning *wakeAlreadyRunningError
		if acceptExistingWake && errors.As(err, &alreadyRunning) {
			return writeWakeReadyFile(readyFile)
		}
		return err
	}
	defer cleanup()

	if injectVia != "" {
		if err := writeWakeTarget(root, me, *target); err != nil {
			_ = writeStderr("warning: wake target not persisted; live repair disabled: %v\n", err)
			if removeErr := removeWakeTarget(root, me); removeErr != nil {
				return fmt.Errorf("clear stale wake target after persist failure: %w", removeErr)
			}
		}
		if err := validateResolvedWakeInjectViaPath(injectVia); err != nil {
			return err
		}
	} else if err := removeWakeTarget(root, me); err != nil {
		return err
	}

	cfg := wakeConfig{
		me:                me,
		root:              root,
		session:           resolveSessionName(root),
		injectCmd:         *injectCmdFlag,
		injectVia:         injectVia,
		injectArgs:        []string(injectArgFlags),
		wakeOwner:         targetOwner(target),
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
	return writeWakeMetadataFile(path, []byte("ready\n"), "wake ready file")
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
	if cfg.injectMode == wakeInjectModeNone {
		return nil
	}
	if cfg.injectVia != "" {
		if cfg.wakeOwner != nil {
			return wakeOwnerHealthCheck(*cfg.wakeOwner)
		}
		return nil
	}
	if !ttyAvailableFn() {
		return errors.New("TTY no longer available")
	}
	return nil
}

func targetOwner(target *wakeTarget) *wakeOwner {
	if target == nil || target.Owner == nil {
		return nil
	}
	owner := *target.Owner
	return &owner
}

func currentWakeOwner() *wakeOwner {
	owner := wakeOwner{PID: os.Getpid()}
	if proc := inspectWakeProcess(owner.PID); proc.Running {
		owner.ProcessStart = proc.StartToken
		owner.BootID = proc.BootID
	}
	if sid, err := getWakeProcessSID(owner.PID); err == nil {
		owner.SessionID = sid
	}
	return &owner
}

func wakeCommandEnv(base []string, root string, owner *wakeOwner) ([]string, error) {
	env := setEnvVar(base, envRoot, root)
	env = unsetEnvVar(env, envWakeOwner)
	if owner == nil {
		return env, nil
	}
	encoded, err := encodeWakeOwnerEnv(*owner)
	if err != nil {
		return nil, err
	}
	return setEnvVar(env, envWakeOwner, encoded), nil
}

func unsetEnvVar(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}

func wakeOwnerHealthCheck(owner wakeOwner) error {
	if err := validateWakeOwner(owner); err != nil {
		return err
	}
	proc := inspectWakeProcess(owner.PID)
	if !proc.Running {
		return fmt.Errorf("inject-via wake owner pid %d is not running", owner.PID)
	}
	if owner.ProcessStart != "" {
		if proc.StartToken == "" {
			return fmt.Errorf("inject-via wake owner process start unavailable for pid %d: %v", owner.PID, proc.InspectError)
		}
		if proc.StartToken != owner.ProcessStart {
			return fmt.Errorf("inject-via wake owner process start changed for pid %d", owner.PID)
		}
	}
	if owner.BootID != "" && proc.BootID != "" && proc.BootID != owner.BootID {
		return fmt.Errorf("inject-via wake owner boot id changed for pid %d", owner.PID)
	}
	if owner.SessionID != 0 {
		sid, err := getWakeProcessSID(owner.PID)
		if err != nil {
			return fmt.Errorf("inject-via wake owner session unavailable for pid %d: %w", owner.PID, err)
		}
		if sid != owner.SessionID {
			return fmt.Errorf("inject-via wake owner session changed for pid %d", owner.PID)
		}
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
