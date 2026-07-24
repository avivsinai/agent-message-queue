//go:build darwin || linux

package cli

import (
	"crypto/rand"
	"encoding/hex"
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
	wakeTerminateGrace   = 100 * time.Millisecond
	wakeBaselineTimeout  = 5 * time.Second
	wakeBaselineSettle   = 50 * time.Millisecond
	getWakeCurrentTTY    = getCurrentTTY
	getWakeProcessSID    = unix.Getsid
	wakeTIOCSTIAvailable = func() bool { return tiocsti.Available() }
	wakeInputIsTTY       = func() bool { return tiocsti.IsTTY() }
)

type fsnotifyWakeEventWatcher struct {
	watcher *fsnotify.Watcher
}

func (w *fsnotifyWakeEventWatcher) Events() <-chan fsnotify.Event {
	return w.watcher.Events
}

func (w *fsnotifyWakeEventWatcher) Errors() <-chan error {
	return w.watcher.Errors
}

func (w *fsnotifyWakeEventWatcher) Close() error {
	return w.watcher.Close()
}

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

type wakeRepairChild struct {
	Process            *os.Process
	Waiter             *wakeProcessWaiter
	ProcessStart       string
	Source             wakeRepairHandoffSource
	Prepared           wakeRepairHandoffPrepared
	Capability         *wakeRepairChildCapability
	Handoff            *wakeRepairParentHandoff
	validateAdmission  func() error
	admit              func() error
	capabilityDetached bool
}

func (child *wakeRepairChild) Admit() error {
	if child == nil || child.admit == nil {
		return fmt.Errorf("wake repair child admission capability is missing")
	}
	return child.admit()
}

type wakeLockAcquireOptions struct {
	acceptExistingValid  bool
	target               *wakeTarget
	wakeMode             string
	repairLineage        *wakeRepairLineage
	repairFloorAuthority *wakeRepairFloorAuthority
}

type wakeLockCreatingError struct{}

func (err *wakeLockCreatingError) Error() string {
	return "wake lock is being created (retry shortly)"
}

func childRepairSource(lineage *wakeRepairLineage) wakeRepairHandoffSource {
	source := wakeRepairHandoffSource{
		schema:             wakeRepairHandoffSchema,
		root:               lineage.source.Root,
		rootIdentity:       lineage.source.RootIdentity,
		agent:              lineage.source.Agent,
		sourceGeneration:   lineage.source.DeadGeneration,
		sourceTargetDigest: lineage.source.SourceTargetDigest,
		sourceFloorDigest:  lineage.source.SourceFloorDigest,
		bootID:             lineage.source.BootID,
		agentDirDevice:     lineage.source.AgentDirDevice,
		agentDirInode:      lineage.source.AgentDirInode,
		inboxDirDevice:     lineage.source.InboxDirDevice,
		inboxDirInode:      lineage.source.InboxDirInode,
	}
	if lineage.source.Owner != nil {
		source.hasOwner = true
		source.ownerPID = lineage.source.Owner.PID
		source.ownerProcessStart = lineage.source.Owner.ProcessStart
		source.ownerBootID = lineage.source.Owner.BootID
		source.ownerSessionID = lineage.source.Owner.SessionID
	}
	return source
}

var startWakeFromTarget = startWakeFromTargetDefault

// acquireWakeLock attempts to acquire the wake lock for an agent's inbox.
// Returns cleanup function and error. If another wake is running, returns error.
func acquireWakeLock(root, me string, target *wakeTarget) (cleanup func(), err error) {
	return acquireWakeLockWithOptions(root, me, wakeLockAcquireOptions{target: target})
}

func acquireWakeLockWithOptions(root, me string, options wakeLockAcquireOptions) (cleanup func(), err error) {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return nil, err
	}
	innerCleanup, err := acquireWakeLockWithOptionsInDir(agentDir, root, me, options)
	if err != nil {
		_ = agentDir.Close()
		return nil, err
	}
	return func() {
		innerCleanup()
		_ = agentDir.Close()
	}, nil
}

func acquireWakeLockWithOptionsInDir(
	agentDir *wakeAgentDir,
	root, me string,
	options wakeLockAcquireOptions,
) (cleanup func(), err error) {
	if agentDir == nil {
		return nil, fmt.Errorf("wake agent directory capability is missing")
	}
	if options.repairLineage != nil && options.target != nil && options.target.Owner != nil {
		return nil, fmt.Errorf("owner-bearing wake state requires 'amq wake recover-owner --me %s'", me)
	}
	if options.target != nil && options.target.Owner != nil {
		return acquireAuthoritativeWakeLockWithOptions(root, me, options)
	}

	for {
		var replace wakeLockInspection
		var created wakeLockInspection
		err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
			inspection := inspectWakeLockAt(dirfd, agentDir, root, me)
			if options.repairLineage != nil && inspection.Exists {
				return fmt.Errorf("wake lock changed before repair acquisition")
			}
			if err := validateGenericWakeLifecycleTransition(inspection, wakeGenericRequestAcquire); err != nil {
				return err
			}
			if inspection.Exists && inspection.Lock.TargetDigest != "" {
				persisted, exists, readErr := readWakeTargetAt(dirfd, agentDir, root, me)
				if readErr != nil {
					return fmt.Errorf("persisted wake target for %s is unverified: %w", me, readErr)
				}
				if exists && persisted.Owner != nil {
					return fmt.Errorf("wake handle %s has legacy owner-bearing state; run 'amq wake recover-owner --me %s'", me, me)
				}
			}
			if inspection.Exists {
				switch inspection.Status {
				case wakeLockStale:
					if err := validateWakeLockStaleRemoval(inspection); err != nil {
						return err
					}
					if err := removeWakeLockIfUnchangedGuarded(inspection); err != nil {
						return err
					}
				case wakeLockCreating:
					return &wakeLockCreatingError{}
				case wakeLockValid:
					if options.acceptExistingValid {
						if err := requireWakeLockUsable(inspection, options.wakeMode, options.target); err != nil {
							return err
						}
						return wakeLockAlreadyRunningError(me, inspection)
					}
					replaceNeeded, replaceErr := wakeLockReplacementNeeded(inspection)
					if replaceErr != nil {
						return replaceErr
					}
					if replaceNeeded {
						replace = inspection
						return nil
					}
					return wakeLockAlreadyRunningError(me, inspection)
				case wakeLockUnverified:
					return fmt.Errorf("wake lock for %s is unverified (pid %d on %s since %s): %s; run 'amq doctor --ops' for details",
						me, inspection.Lock.PID, inspection.Lock.TTY, inspection.Lock.Started, inspection.Reason)
				}
			}
			if replace.Exists {
				return nil
			}
			if orphan, exists, readErr := readWakeTargetAt(dirfd, agentDir, root, me); readErr != nil {
				return fmt.Errorf("wake target for %s is unverified: %w", me, readErr)
			} else if exists && orphan.Owner != nil {
				return fmt.Errorf("wake handle %s has an owner-bearing orphan target; run 'amq wake recover-owner --me %s'", me, me)
			}
			if options.repairLineage != nil {
				if options.target == nil {
					return fmt.Errorf("wake repair lineage requires an inject-via target")
				}
				persisted, exists, readErr := readWakeTargetAt(dirfd, agentDir, root, me)
				if readErr != nil {
					return fmt.Errorf("wake repair target is unverified: %w", readErr)
				}
				if !exists {
					return fmt.Errorf("wake repair target disappeared before acquisition")
				}
				if err := validateWakeTarget(persisted, root, me); err != nil {
					return err
				}
				if !sameWakeTarget(persisted, *options.target) {
					return fmt.Errorf("wake repair target changed before acquisition")
				}
				if err := validateWakeRepairLineageGuardedAt(
					dirfd, agentDir, root, me, persisted, options.repairLineage,
				); err != nil {
					return err
				}
			} else if err := removeWakeRepairFloorGuardedAt(dirfd, agentDir); err != nil {
				return err
			}

			// Stage target metadata first. The lock is the transaction commit point.
			if options.target != nil {
				if err := writeWakeTargetGuardedAt(dirfd, agentDir, root, me, *options.target); err != nil {
					return err
				}
			} else if err := removeWakeTargetGuardedAt(dirfd, agentDir); err != nil {
				return err
			}

			lock, err := newWakeLock(root, me, options)
			if err != nil {
				return err
			}
			if options.repairLineage != nil {
				err = createWakeRepairLockAt(
					dirfd,
					agentDir,
					root,
					me,
					options.repairLineage.source.RootIdentity,
					lock,
				)
			} else {
				err = createWakeLockAt(dirfd, agentDir, root, me, lock)
			}
			if err != nil {
				return err
			}
			created = inspectWakeLockAt(dirfd, agentDir, root, me)
			if !created.Exists || created.Lock.Generation != lock.Generation {
				return fmt.Errorf("failed to verify created wake lock generation")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if replace.Exists {
			if _, err := terminateAndRemoveOrphanedWakeLock(replace); err != nil {
				return nil, err
			}
			continue
		}

		cleanup = func() {
			_ = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
				current := inspectWakeLockAt(dirfd, agentDir, root, me)
				if !sameWakeLockGeneration(created, current) || !currentWakeLockMatches(current.Lock) {
					return nil
				}
				if err := removeWakeLockIfUnchangedGuardedAt(dirfd, agentDir, current); err != nil {
					return err
				}
				if options.repairLineage != nil {
					if options.repairFloorAuthority == nil {
						return fmt.Errorf("wake repair floor cleanup authority is missing")
					}
					return removeWakeRepairFloorIfGenerationGuardedAt(
						dirfd,
						agentDir,
						*options.repairFloorAuthority,
					)
				}
				floor, exists, err := readWakeRepairFloorAt(dirfd, agentDir)
				if err != nil || !exists || floor.Generation != created.Lock.Generation {
					return err
				}
				return removeWakeRepairFloorGuardedAt(dirfd, agentDir)
			})
		}
		return cleanup, nil
	}
}

func newWakeLock(root, me string, options wakeLockAcquireOptions) (wakeLock, error) {
	generationBytes := make([]byte, 16)
	if _, err := rand.Read(generationBytes); err != nil {
		return wakeLock{}, fmt.Errorf("generate wake lock nonce: %w", err)
	}
	ttyName := getCurrentTTY()
	if ttyName == "" {
		ttyName = "unknown"
	}
	lock := wakeLock{
		PID:        os.Getpid(),
		TTY:        ttyName,
		Root:       canonicalWakeRoot(root),
		Agent:      me,
		Started:    time.Now().UTC().Format(time.RFC3339),
		Generation: hex.EncodeToString(generationBytes),
		WakeMode:   options.wakeMode,
	}
	if options.target != nil {
		targetDigest, err := wakeTargetDigest(*options.target)
		if err != nil {
			return wakeLock{}, err
		}
		lock.WakeMode = wakeTargetInjectVia
		lock.TargetDigest = targetDigest
		lock.ControlSocket = wakeControlSocketPath(root, me, lock.Generation)
		if options.target.Owner != nil {
			owner := *options.target.Owner
			lock.WakeMode = wakeOwnerWakeMode
			lock.OwnerSchema = wakeOwnerLockSchema
			lock.Owner = &owner
		}
	}
	if options.repairLineage != nil {
		lock.SourceGeneration = options.repairLineage.source.DeadGeneration
		lock.SourceFloorDigest = options.repairLineage.source.SourceFloorDigest
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
	return lock, nil
}

func shouldReplaceOrphanedWakeLock(inspection wakeLockInspection) (bool, error) {
	replace, err := wakeLockReplacementNeeded(inspection)
	if err != nil || !replace {
		return replace, err
	}
	return terminateAndRemoveOrphanedWakeLock(inspection)
}

func wakeLockReplacementNeeded(inspection wakeLockInspection) (bool, error) {
	if err := validateWakeLockOwnerlessMutation(inspection); err != nil {
		return false, err
	}
	return wakeLockNeedsReplacement(inspection), nil
}

func validateWakeLockOwnerlessMutation(inspection wakeLockInspection) error {
	if err := validateGenericWakeLifecycleTransition(inspection, wakeGenericRequestMutate); err != nil {
		return err
	}
	if inspection.Lock.TargetDigest != "" {
		target, exists, err := readWakeTarget(inspection.Root, inspection.Agent)
		if err != nil {
			return fmt.Errorf("wake target is unverified before ownerless mutation: %w", err)
		}
		if exists && target.Owner != nil {
			return fmt.Errorf("owner-bearing wake state requires 'amq wake recover-owner --me %s'", inspection.Agent)
		}
	}
	return nil
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

func requireWakeLockUsable(inspection wakeLockInspection, requiredMode string, requestedTarget *wakeTarget) error {
	if !inspection.Exists || inspection.Status != wakeLockValid || !inspection.IdentityConfirmed {
		return fmt.Errorf("existing wake lock for %s is not a confirmed valid wake", inspection.Agent)
	}
	if inspection.Lock.WakeMode != requiredMode {
		if requiredMode == wakeInjectModeNone {
			return fmt.Errorf("existing wake for %s cannot satisfy requested --inject-mode none; stop the existing wake and retry", inspection.Agent)
		}
		// Legacy locks recorded WakeMode only for none and inject-via.
		legacyTTYWake := inspection.Lock.WakeMode == "" &&
			(requiredMode == wakeInjectModeRaw || requiredMode == wakeInjectModePaste)
		if !legacyTTYWake {
			return fmt.Errorf("existing wake for %s cannot satisfy requested wake mode %q (existing %q); stop the existing wake and retry", inspection.Agent, requiredMode, inspection.Lock.WakeMode)
		}
	}
	if !wakeLockHasUsableNotificationPath(inspection) {
		return fmt.Errorf("existing wake lock for %s is not usable for --require-wake (pid %d on %s since %s)",
			inspection.Agent, inspection.Lock.PID, inspection.Lock.TTY, inspection.Lock.Started)
	}
	if wakeLockNeedsReplacement(inspection) {
		return fmt.Errorf("existing wake lock for %s is not usable for --require-wake (pid %d on %s since %s)",
			inspection.Agent, inspection.Lock.PID, inspection.Lock.TTY, inspection.Lock.Started)
	}
	if requiredMode == wakeTargetInjectVia {
		if requestedTarget == nil {
			return fmt.Errorf("existing inject-via wake for %s cannot be reused without a requested wake target", inspection.Agent)
		}
		persistedTarget, exists, err := readWakeTarget(inspection.Root, inspection.Agent)
		if err != nil {
			return fmt.Errorf("existing inject-via wake target for %s is not usable: %w", inspection.Agent, err)
		}
		if !exists {
			return fmt.Errorf("existing inject-via wake for %s has no persisted wake target", inspection.Agent)
		}
		if err := validateWakeTargetMatchesLock(inspection.Lock, persistedTarget); err != nil {
			return fmt.Errorf("existing inject-via wake target for %s is not bound to its lock: %w", inspection.Agent, err)
		}
		if err := validateWakeTarget(persistedTarget, inspection.Root, inspection.Agent); err != nil {
			return fmt.Errorf("existing inject-via wake target for %s is invalid: %w", inspection.Agent, err)
		}
		if !sameWakeInjectorIdentity(persistedTarget, *requestedTarget) {
			return fmt.Errorf("existing inject-via wake for %s uses a different injector path or fixed arguments", inspection.Agent)
		}
	}
	return nil
}

func wakeLockHasUsableNotificationPath(inspection wakeLockInspection) bool {
	if inspection.Lock.WakeMode == wakeInjectModeNone {
		return true
	}
	if ((inspection.Lock.WakeMode == wakeTargetInjectVia || inspection.Lock.WakeMode == wakeOwnerWakeMode) &&
		inspection.Lock.TargetDigest != "") || wakeArgsUseInjectVia(inspection.Process.Args) {
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

func sameWakeLockInspection(first, second wakeLockInspection) bool {
	if !second.Exists || second.Status != wakeLockValid {
		return false
	}
	if first.PID != second.PID || first.Root != second.Root || first.Agent != second.Agent {
		return false
	}
	return sameWakeLockGeneration(first, second)
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
	if len(args) > 0 {
		switch args[0] {
		case "repair":
			return runWakeRepair(args[1:])
		case "retire":
			return runWakeRetire(args[1:])
		case "recover-owner":
			return runWakeRecoverOwner(args[1:])
		}
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
	if err := os.MkdirAll(fsq.AgentBase(root, me), 0o700); err != nil {
		result.Status = "error"
		result.Reason = err.Error()
		return result, err
	}
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		result.Status = "error"
		result.Reason = err.Error()
		return result, err
	}
	defer func() { _ = agentDir.Close() }()

	var target wakeTarget
	var repairFloor wakeRepairFloor
	var lineage wakeRepairLineage
	var inboxDir *wakeInboxDir
	defer func() { _ = inboxDir.Close() }()
	prepareErr := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		inspection := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !inspection.Exists {
			result.Status = "refused"
			result.Reason = "no wake lock present; start wake normally"
			return errors.New(result.Reason)
		}
		switch inspection.Status {
		case wakeLockValid:
			result.Status = "refused"
			result.PID = inspection.PID
			result.Reason = "wake lock is already valid; refusing repair"
			return errors.New(result.Reason)
		case wakeLockStale:
			if err := validateWakeLockRepairable(inspection); err != nil {
				result.Status = "refused"
				result.PID = inspection.PID
				result.Reason = err.Error()
				return err
			}
		case wakeLockCreating:
			result.Status = "refused"
			result.Reason = "wake lock is being created; retry shortly"
			return errors.New(result.Reason)
		case wakeLockUnverified:
			result.Status = "refused"
			result.PID = inspection.PID
			result.Reason = "wake lock is unverified; refusing to start a second injector"
			return fmt.Errorf("%s: %s", result.Reason, inspection.Reason)
		default:
			result.Status = "refused"
			result.Reason = fmt.Sprintf("wake lock status %q is not repairable", inspection.Status)
			return errors.New(result.Reason)
		}

		var exists bool
		var err error
		target, exists, err = readWakeTargetAt(dirfd, agentDir, root, me)
		if err != nil {
			result.Status = "refused"
			result.Reason = err.Error()
			return err
		}
		if !exists {
			result.Status = "refused"
			result.Reason = "no inject-via wake target; restart wake manually"
			return errors.New(result.Reason)
		}
		if err := validateWakeTarget(target, root, me); err != nil {
			result.Status = "refused"
			result.Reason = err.Error()
			return err
		}
		if target.Owner != nil {
			result.Status = "refused"
			result.PID = inspection.PID
			result.Reason = fmt.Sprintf("owner-bearing wake state requires 'amq wake recover-owner --me %s'", me)
			return errors.New(result.Reason)
		}
		if err := validateWakeTargetMatchesLock(inspection.Lock, target); err != nil {
			result.Status = "refused"
			result.Reason = err.Error()
			return err
		}
		repairFloor, exists, err = readWakeRepairFloorAt(dirfd, agentDir)
		if err != nil {
			result.Status = "refused"
			result.Reason = err.Error()
			return err
		}
		if !exists {
			result.Status = "refused"
			result.Reason = "wake repair floor is missing; restart wake manually"
			return errors.New(result.Reason)
		}
		if err := validateWakeRepairFloor(repairFloor, root, me, inspection.Lock, target); err != nil {
			result.Status = "refused"
			result.Reason = err.Error()
			return err
		}
		if err := validateWakeRepairFloorCurrentBoot(repairFloor); err != nil {
			result.Status = "refused"
			result.Reason = err.Error()
			return err
		}
		inboxDir, err = openWakeRepairInboxDir(agentDir)
		if err != nil {
			result.Status = "refused"
			result.Reason = err.Error()
			return err
		}
		handoffSource, err := newWakeRepairHandoffSource(
			repairFloor,
			target,
			agentDir,
			inboxDir,
		)
		if err != nil {
			result.Status = "refused"
			result.Reason = err.Error()
			return err
		}
		lineage = wakeRepairLineage{
			source: wakeRepairSource{
				Root:               handoffSource.Root(),
				RootIdentity:       handoffSource.RootIdentity(),
				Agent:              handoffSource.Agent(),
				DeadGeneration:     handoffSource.SourceGeneration(),
				BootID:             handoffSource.BootID(),
				Owner:              handoffSource.Owner(),
				SourceTargetDigest: handoffSource.SourceTargetDigest(),
				SourceFloorDigest:  handoffSource.SourceFloorDigest(),
				AgentDirDevice:     handoffSource.agentDirDevice,
				AgentDirInode:      handoffSource.agentDirInode,
				InboxDirDevice:     handoffSource.inboxDirDevice,
				InboxDirInode:      handoffSource.inboxDirInode,
			},
			floor: repairFloor,
		}
		result.RepairAvailable = true
		if err := removeWakeLockIfUnchangedGuardedAt(dirfd, agentDir, inspection); err != nil {
			result.Status = "refused"
			result.RepairAvailable = false
			result.PID = inspectWakeLockAt(dirfd, agentDir, root, me).PID
			result.Reason = "wake lock changed before repair"
			return errors.New(result.Reason)
		}
		return nil
	})
	if prepareErr != nil {
		return result, prepareErr
	}

	// Spawning and the private prepared handshake happen without the lifecycle
	// guard. The retained directory capability remains open across the wait.
	child, startErr := startWakeFromTarget(agentDir, inboxDir, root, me, target, lineage)
	if startErr != nil {
		if child != nil {
			_ = cleanupFailedWakeRepairChild(agentDir, root, me, child)
		}
		result.RepairAvailable = false
		result.Status = "error"
		result.Reason = startErr.Error()
		return result, startErr
	}
	winner, winnerErr := validatePreparedRepairWakeWinnerInDir(
		agentDir,
		root,
		me,
		target,
		lineage,
		child,
	)
	if winnerErr != nil {
		cleanupErr := cleanupFailedWakeRepairChild(agentDir, root, me, child)
		result.RepairAvailable = false
		result.Status = "error"
		if child != nil && child.Process != nil {
			result.PID = child.Process.Pid
		}
		result.Reason = fmt.Sprintf("repaired wake failed exact preparation validation: %v", winnerErr)
		if cleanupErr != nil {
			return result, fmt.Errorf("%s (cleanup: %v)", result.Reason, cleanupErr)
		}
		return result, errors.New(result.Reason)
	}
	validateAfterAcknowledgement := child.validateAdmission
	child.validateAdmission = func() error {
		if validateAfterAcknowledgement == nil {
			return fmt.Errorf("wake repair child final admission validation is missing")
		}
		if err := validateAfterAcknowledgement(); err != nil {
			return err
		}
		_, err := validatePreparedRepairWakeWinnerInDir(
			agentDir,
			root,
			me,
			target,
			lineage,
			child,
		)
		return err
	}
	if err := child.Admit(); err != nil {
		cleanupErr := cleanupFailedWakeRepairChild(agentDir, root, me, child)
		result.RepairAvailable = false
		result.Status = "error"
		result.PID = winner.PID
		result.Reason = fmt.Sprintf("repaired wake admission failed: %v", err)
		if cleanupErr != nil {
			return result, fmt.Errorf("%s (cleanup: %v)", result.Reason, cleanupErr)
		}
		return result, errors.New(result.Reason)
	}
	result.Status = "repaired"
	result.PID = winner.PID
	return result, nil
}

func validatePreparedRepairWakeWinnerInDir(
	agentDir *wakeAgentDir,
	root, me string,
	expected wakeTarget,
	lineage wakeRepairLineage,
	child *wakeRepairChild,
) (wakeLockInspection, error) {
	var winner wakeLockInspection
	if child == nil || child.Process == nil || child.Process.Pid <= 0 {
		return winner, fmt.Errorf("started wake repair child is missing")
	}
	if err := child.Prepared.validateSource(child.Source); err != nil {
		return winner, err
	}
	if child.Source.SourceGeneration() != lineage.source.DeadGeneration ||
		child.Source.SourceTargetDigest() != lineage.source.SourceTargetDigest ||
		child.Source.SourceFloorDigest() != lineage.source.SourceFloorDigest ||
		child.Source.RootIdentity() != lineage.source.RootIdentity {
		return winner, fmt.Errorf("started wake repair child source lineage mismatch")
	}
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		if err := revalidateWakeRepairRootIdentity(root, lineage.source.RootIdentity); err != nil {
			return err
		}
		if err := validateCanonicalWakeRepairDirectories(root, me, child.Source); err != nil {
			return err
		}
		winner = inspectWakeLockAt(dirfd, agentDir, root, me)
		if winner.Status != wakeLockValid || !winner.IdentityConfirmed || winner.Lock.Generation == "" {
			return fmt.Errorf("no confirmed generation-bound wake is ready")
		}
		if winner.PID != child.Process.Pid || winner.PID != child.Prepared.ChildPID() {
			return fmt.Errorf(
				"prepared wake pid %d does not match started pid %d",
				winner.PID,
				child.Process.Pid,
			)
		}
		if winner.Lock.Generation != child.Prepared.ChildGeneration() {
			return fmt.Errorf("prepared wake generation changed before admission")
		}
		if child.ProcessStart == "" || winner.Lock.ProcessStart != child.ProcessStart {
			return fmt.Errorf("prepared wake process identity changed before admission")
		}
		if winner.Lock.SourceGeneration != lineage.source.DeadGeneration ||
			winner.Lock.SourceFloorDigest != lineage.source.SourceFloorDigest {
			return fmt.Errorf("prepared wake lock lineage mismatch")
		}
		persisted, exists, err := readWakeTargetAt(dirfd, agentDir, root, me)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("repaired wake target is missing")
		}
		if err := validateWakeTarget(persisted, root, me); err != nil {
			return err
		}
		if err := validateWakeTargetMatchesLock(winner.Lock, persisted); err != nil {
			return err
		}
		targetDigest, err := wakeTargetDigest(persisted)
		if err != nil {
			return err
		}
		if !sameWakeTarget(persisted, expected) ||
			targetDigest != lineage.source.SourceTargetDigest ||
			targetDigest != child.Prepared.ChildTargetDigest() {
			return fmt.Errorf("prepared wake uses a different exact target")
		}
		floorSnapshot, exists, err := readWakeRepairFloorSnapshotAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("repaired wake floor is missing")
		}
		floor := floorSnapshot.Floor
		if err := validateWakeRepairFloor(floor, root, me, winner.Lock, persisted); err != nil {
			return err
		}
		if floor.SourceGeneration != lineage.source.DeadGeneration ||
			floor.SourceFloorDigest != lineage.source.SourceFloorDigest ||
			floor.RootIdentity != lineage.source.RootIdentity {
			return fmt.Errorf("prepared wake floor lineage mismatch")
		}
		floorDigest, err := wakeRepairFloorDigest(floor)
		if err != nil {
			return err
		}
		if floorDigest != child.Prepared.ChildFloorDigest() {
			return fmt.Errorf("prepared wake floor digest changed before admission")
		}
		floorAuthority, err := newWakeRepairFloorAuthority(floorSnapshot)
		if err != nil {
			return err
		}
		if floorAuthority != child.Prepared.ChildFloorAuthority() {
			return fmt.Errorf("prepared wake floor file changed before admission")
		}
		if !sameWakeRepairSuppression(floor, lineage.floor) {
			return fmt.Errorf("repaired wake changed the inherited suppression floor")
		}
		return nil
	})
	return winner, err
}

func cleanupFailedWakeRepairChild(
	agentDir *wakeAgentDir,
	root, me string,
	child *wakeRepairChild,
) error {
	if child == nil {
		return nil
	}
	var cleanupErr error
	if child.capabilityDetached {
		// Once stable stop authority is detached, close the unreleased handoff
		// first so the blocked child observes EOF before we wait for its exit.
		cleanupErr = errors.Join(cleanupErr, child.Handoff.Close(), child.Capability.Close())
		if child.Waiter != nil {
			cleanupErr = errors.Join(cleanupErr, child.Waiter.waitForExit(wakeProcessExitTimeout))
		}
	} else {
		if child.Capability != nil {
			cleanupErr = errors.Join(cleanupErr, child.Capability.Stop())
		}
		if child.Waiter != nil {
			cleanupErr = errors.Join(cleanupErr, child.Waiter.waitForExit(wakeProcessExitTimeout))
		}
		cleanupErr = errors.Join(cleanupErr, child.Handoff.Close(), child.Capability.Close())
	}

	if child.Process == nil || child.Process.Pid <= 0 {
		return cleanupErr
	}
	metadataErr := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !current.Exists {
			return nil
		}
		if current.PID != child.Process.Pid ||
			current.Lock.ProcessStart == "" ||
			current.Lock.ProcessStart != child.ProcessStart ||
			current.Lock.SourceGeneration != child.Source.SourceGeneration() ||
			current.Lock.SourceFloorDigest != child.Source.SourceFloorDigest() ||
			current.Lock.TargetDigest != child.Source.SourceTargetDigest() {
			return nil
		}
		if generation := child.Prepared.ChildGeneration(); generation != "" &&
			current.Lock.Generation != generation {
			return nil
		}
		if err := removeWakeLockIfUnchangedGuardedAt(dirfd, agentDir, current); err != nil {
			return err
		}
		return removeWakeRepairFloorIfGenerationGuardedAt(
			dirfd,
			agentDir,
			child.Prepared.ChildFloorAuthority(),
		)
	})
	return errors.Join(cleanupErr, metadataErr)
}

func startWakeFromTargetDefault(
	agentDir *wakeAgentDir,
	inboxDir *wakeInboxDir,
	root, me string,
	target wakeTarget,
	lineage wakeRepairLineage,
) (*wakeRepairChild, error) {
	amqBin, err := os.Executable()
	if err != nil {
		amqBin = "amq"
	}
	source, err := newWakeRepairHandoffSource(lineage.floor, target, agentDir, inboxDir)
	if err != nil {
		return nil, err
	}
	if source.SourceGeneration() != lineage.source.DeadGeneration ||
		source.SourceTargetDigest() != lineage.source.SourceTargetDigest ||
		source.SourceFloorDigest() != lineage.source.SourceFloorDigest ||
		source.RootIdentity() != lineage.source.RootIdentity ||
		source.agentDirDevice != lineage.source.AgentDirDevice ||
		source.agentDirInode != lineage.source.AgentDirInode ||
		source.inboxDirDevice != lineage.source.InboxDirDevice ||
		source.inboxDirInode != lineage.source.InboxDirInode {
		return nil, fmt.Errorf("wake repair child source does not match retained lineage")
	}
	args := buildRepairWakeArgs(root, me, target, lineage.source.DeadGeneration, "")
	cmd := exec.Command(amqBin, args...)
	env, err := wakeCommandEnv(os.Environ(), root, target.Owner)
	if err != nil {
		return nil, err
	}
	cmd.Env = env
	output, err := openWakeRepairOutputInDir(agentDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = output.Close() }()
	configureRepairWakeCommand(cmd, output)
	handoff, err := prepareWakeRepairHandoff(cmd, source, agentDir, inboxDir)
	if err != nil {
		return nil, err
	}
	capability, err := prepareWakeRepairChildCapability(cmd)
	if err != nil {
		_ = handoff.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = handoff.Close()
		_ = capability.Close()
		return nil, fmt.Errorf("start repaired amq wake: %w", err)
	}
	waiter := newWakeProcessWaiter(cmd.Process)
	child := &wakeRepairChild{
		Process:    cmd.Process,
		Waiter:     waiter,
		Source:     source,
		Capability: capability,
		Handoff:    handoff,
	}
	if err := capability.Bind(cmd.Process); err != nil {
		return child, fmt.Errorf("bind exact wake repair child capability: %w", err)
	}
	if err := handoff.Bind(cmd.Process); err != nil {
		return child, fmt.Errorf("bind wake repair handoff: %w", err)
	}
	process := inspectWakeProcess(cmd.Process.Pid)
	if !process.Running || process.StartToken == "" {
		return child, fmt.Errorf("capture exact wake repair child process identity")
	}
	child.ProcessStart = process.StartToken

	type preparedResult struct {
		prepared wakeRepairHandoffPrepared
		err      error
	}
	preparedCh := make(chan preparedResult, 1)
	go func() {
		prepared, receiveErr := handoff.ReceivePrepared(source)
		preparedCh <- preparedResult{prepared: prepared, err: receiveErr}
	}()
	timer := time.NewTimer(wakeReadyTimeout)
	defer timer.Stop()
	select {
	case prepared := <-preparedCh:
		if prepared.err != nil {
			return child, fmt.Errorf("receive wake repair prepared tuple: %w", prepared.err)
		}
		child.Prepared = prepared.prepared
	case <-waiter.done:
		return child, fmt.Errorf("repaired amq wake exited before preparation")
	case <-timer.C:
		return child, fmt.Errorf("repaired amq wake did not prepare within %s", wakeReadyTimeout)
	}
	child.admit = func() error {
		if err := handoff.Admit(child.Prepared); err != nil {
			return err
		}
		if child.validateAdmission == nil {
			return fmt.Errorf("wake repair child final admission validation is missing")
		}
		if err := child.validateAdmission(); err != nil {
			return fmt.Errorf("final wake repair admission validation: %w", err)
		}
		if err := capability.Detach(); err != nil {
			return fmt.Errorf("detach admitted wake repair child capability: %w", err)
		}
		child.capabilityDetached = true
		if err := handoff.Release(child.Prepared); err != nil {
			return err
		}
		// A complete release frame is the irreversible admission commit. Cleanup
		// errors after it cannot revoke authorization or safely stop the child.
		_ = handoff.Close()
		_ = capability.Close()
		return nil
	}
	child.validateAdmission = func() error {
		return validateCanonicalWakeRepairDirectories(root, me, child.Source)
	}
	return child, nil
}

func openWakeRepairOutputInDir(agentDir *wakeAgentDir) (*os.File, error) {
	if agentDir == nil {
		return nil, fmt.Errorf("wake repair agent directory capability is missing")
	}
	var file *os.File
	err := agentDir.withFD(func(dirfd int) error {
		fd, err := unix.Openat(
			dirfd,
			".wake.repair.log",
			unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0o600,
		)
		if err != nil {
			if err == unix.ELOOP {
				return fmt.Errorf("repair wake log %s must not be a symlink", filepath.Join(agentDir.path, ".wake.repair.log"))
			}
			if err == unix.ENXIO {
				return fmt.Errorf("repair wake log %s must be a regular file", filepath.Join(agentDir.path, ".wake.repair.log"))
			}
			return fmt.Errorf("open repair wake log: %w", err)
		}
		file = os.NewFile(uintptr(fd), filepath.Join(agentDir.path, ".wake.repair.log"))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if info, err := file.Stat(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat repair wake log: %w", err)
	} else if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("repair wake log %s must be a regular file", file.Name())
	}
	return file, nil
}

func configureRepairWakeCommand(cmd *exec.Cmd, output *os.File) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = output
	cmd.Stderr = output
}

func buildRepairWakeArgs(root, me string, target wakeTarget, generation, readyPath string) []string {
	args := []string{"--no-update-check", "wake", "--me", me, "--root", root, "--repair-lineage", generation, "--inject-via", target.InjectVia}
	for _, arg := range target.InjectArgs {
		args = append(args, "--inject-arg", arg)
	}
	if readyPath != "" {
		args = append(args, "--ready-file", readyPath)
	}
	return args
}

func runWakeWithLoop(args []string, loop wakeLoopFunc) error {
	privateStop, cleanupPrivateStop, err := authoritativeWakePrivateStopFromEnv()
	if err != nil {
		return err
	}
	defer cleanupPrivateStop()
	repairHandoff, repairHandoffPresent, err := wakeRepairChildHandoffFromEnv()
	if err != nil {
		return err
	}
	if repairHandoff != nil {
		defer func() { _ = repairHandoff.Close() }()
	}
	repairPrivateStop, cleanupRepairPrivateStop, err := wakeRepairChildStopFromEnv()
	if err != nil {
		return err
	}
	defer cleanupRepairPrivateStop()
	privateStop = mergeWakeStopChannels(privateStop, repairPrivateStop)

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
	repairLineageFlag := fs.String("repair-lineage", "", "Internal: inherit the suppression floor from an exact dead wake generation")
	baselineExistingFlag := fs.Bool("baseline-existing", false, "Ignore messages already waiting when this wake starts")

	usage := usageWithHiddenFlags(fs, "amq wake --me <agent> [options]",
		[]string{"ready-file", "accept-existing-wake", "repair-lineage"},
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
	repairGeneration := strings.TrimSpace(*repairLineageFlag)
	if *repairLineageFlag != "" && repairGeneration == "" {
		return UsageError("--repair-lineage must not be blank")
	}
	if repairGeneration != "" && injectVia == "" {
		return UsageError("--repair-lineage requires --inject-via")
	}
	if repairGeneration != "" && *baselineExistingFlag {
		return UsageError("--repair-lineage cannot be combined with --baseline-existing")
	}
	if repairGeneration != "" && !repairHandoffPresent {
		return fmt.Errorf("wake repair requires a private source/admission handoff")
	}
	if repairGeneration == "" && repairHandoffPresent {
		return fmt.Errorf("wake repair handoff requires --repair-lineage")
	}

	// Verify TIOCSTI is available (skip in inject-via mode — uses external command instead)
	if injectVia == "" && injectMode != wakeInjectModeNone {
		if !wakeTIOCSTIAvailable() {
			return errors.New("TIOCSTI not available on this platform; use tmux send-keys or terminal-specific injection")
		}

		// Verify we have a real TTY
		if !wakeInputIsTTY() {
			return errors.New("amq wake requires a real terminal (run in foreground or as background job in same terminal, or use --inject-via for external injection)")
		}
	}

	interruptKey, err := parseInterruptKey(*interruptCmdFlag)
	if err != nil {
		return UsageError("%v", err)
	}

	var target *wakeTarget
	var repairLineage *wakeRepairLineage
	var repairAgentDir *wakeAgentDir
	var repairInboxDir *wakeInboxDir
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
		if repairGeneration != "" {
			if owner != nil {
				return fmt.Errorf("owner-bearing wake state requires 'amq wake recover-owner --me %s'", me)
			}
			source, err := repairHandoff.ReceiveSource()
			if err != nil {
				return fmt.Errorf("receive wake repair source: %w", err)
			}
			if source.Root() != canonicalWakeRoot(root) ||
				source.Agent() != me ||
				source.SourceGeneration() != repairGeneration {
				return fmt.Errorf("wake repair source does not match requested root, agent, and generation")
			}
			repairAgentDir, repairInboxDir, err = repairHandoff.TakeRetainedDirectories(source)
			if err != nil {
				return err
			}
			defer func() { _ = repairAgentDir.Close() }()
			defer func() { _ = repairInboxDir.Close() }()
			var persisted wakeTarget
			var floor wakeRepairFloor
			err = withWakeLifecycleGuardInDir(repairAgentDir, func(dirfd int) error {
				if err := revalidateWakeRepairRootIdentity(root, source.RootIdentity()); err != nil {
					return err
				}
				var exists bool
				persisted, exists, err = readWakeTargetAt(dirfd, repairAgentDir, root, me)
				if err != nil {
					return err
				}
				if !exists {
					return fmt.Errorf("wake repair target is missing")
				}
				floor, exists, err = readWakeRepairFloorAt(dirfd, repairAgentDir)
				if err != nil {
					return err
				}
				if !exists {
					return fmt.Errorf("wake repair floor is missing")
				}
				return nil
			})
			if err != nil {
				return err
			}
			if value.Schema != persisted.Schema ||
				value.Mode != persisted.Mode ||
				value.Root != persisted.Root ||
				value.Agent != persisted.Agent ||
				!sameWakeInjectorIdentity(value, persisted) ||
				!sameWakeOwner(value.Owner, persisted.Owner) {
				return fmt.Errorf("wake repair target changed before child start")
			}
			// The repair CLI carries the requested injector behavior, not the
			// persisted instance timestamp. Continue with the exact retained
			// target whose digest is bound into the source handoff.
			value = persisted
			target = &value
			targetDigest, err := wakeTargetDigest(persisted)
			if err != nil {
				return err
			}
			floorDigest, err := wakeRepairFloorDigest(floor)
			if err != nil {
				return err
			}
			if targetDigest != source.SourceTargetDigest() ||
				floorDigest != source.SourceFloorDigest() ||
				floor.Generation != source.SourceGeneration() ||
				floor.RootIdentity != source.RootIdentity() ||
				floor.BootID != source.BootID() ||
				!sameWakeOwner(floor.Owner, source.Owner()) {
				return fmt.Errorf("wake repair source lineage changed before child acquisition")
			}
			repairLineage = &wakeRepairLineage{
				source: wakeRepairSource{
					Root:               source.Root(),
					RootIdentity:       source.RootIdentity(),
					Agent:              source.Agent(),
					DeadGeneration:     source.SourceGeneration(),
					BootID:             source.BootID(),
					Owner:              source.Owner(),
					SourceTargetDigest: source.SourceTargetDigest(),
					SourceFloorDigest:  source.SourceFloorDigest(),
					AgentDirDevice:     source.agentDirDevice,
					AgentDirInode:      source.agentDirInode,
					InboxDirDevice:     source.inboxDirDevice,
					InboxDirInode:      source.inboxDirInode,
				},
				floor: floor,
			}
			target = &persisted
			injectVia = persisted.InjectVia
			injectArgFlags = append(multiStringFlag(nil), persisted.InjectArgs...)
		}
	}

	// Acquire lock to prevent duplicate wake processes
	acceptExistingWake := readyFile != "" && *acceptExistingWakeFlag
	lockWakeMode := injectMode
	if target != nil {
		lockWakeMode = wakeTargetInjectVia
	} else if lockWakeMode != wakeInjectModeNone {
		lockWakeMode = effectiveInjectMode(&wakeConfig{me: me, injectMode: lockWakeMode})
	}
	acceptExistingDeadline := time.Now().Add(wakeReadyTimeout)
	var cleanup func()
	var repairFloorAuthority wakeRepairFloorAuthority
	for {
		options := wakeLockAcquireOptions{
			acceptExistingValid: acceptExistingWake,
			target:              target,
			wakeMode:            lockWakeMode,
			repairLineage:       repairLineage,
		}
		if repairLineage != nil {
			options.repairFloorAuthority = &repairFloorAuthority
		}
		if repairAgentDir != nil {
			cleanup, err = acquireWakeLockWithOptionsInDir(repairAgentDir, root, me, options)
		} else {
			cleanup, err = acquireWakeLockWithOptions(root, me, options)
		}
		if err == nil {
			break
		}
		var creating *wakeLockCreatingError
		if acceptExistingWake && errors.As(err, &creating) {
			if !waitForWakePreparedRetry(acceptExistingDeadline) {
				return fmt.Errorf("wake lock did not finish creation within %s", wakeReadyTimeout)
			}
			continue
		}
		var alreadyRunning *wakeAlreadyRunningError
		if acceptExistingWake && errors.As(err, &alreadyRunning) {
			if err := writeWakeReadyFileForPreparedWake(root, me, readyFile, alreadyRunning.Inspection, acceptExistingDeadline); err != nil {
				return err
			}
			if *baselineExistingFlag {
				_ = writeStderr("warning: reusing existing amq wake; this launch did not re-baseline it, so pending backlog may still notify\n")
			}
			return nil
		}
		return err
	}
	defer cleanup()
	var controlStop <-chan struct{}
	if injectVia != "" {
		var current wakeLockInspection
		if repairAgentDir != nil {
			if err := repairAgentDir.withFD(func(dirfd int) error {
				current = inspectWakeLockAt(dirfd, repairAgentDir, root, me)
				return nil
			}); err != nil {
				return err
			}
		} else {
			current = inspectWakeLock(root, me)
		}
		var controlCleanup func()
		var stop <-chan struct{}
		var markStopped func()
		var controlErr error
		if repairAgentDir != nil {
			controlCleanup, stop, markStopped, controlErr = startWakeControlListenerInDir(
				repairAgentDir, root, me, current.Lock,
			)
		} else {
			controlCleanup, stop, markStopped, controlErr = startWakeControlListener(root, me, current.Lock)
		}
		if controlErr != nil {
			return controlErr
		}
		defer controlCleanup()
		defer markStopped()
		controlStop = stop
	}
	controlStop = mergeWakeStopChannels(controlStop, privateStop)

	if injectVia != "" {
		if err := validateResolvedWakeInjectViaPath(injectVia); err != nil {
			return err
		}
	}

	var currentWake wakeLockInspection
	if repairAgentDir != nil {
		if err := repairAgentDir.withFD(func(dirfd int) error {
			currentWake = inspectWakeLockAt(dirfd, repairAgentDir, root, me)
			return nil
		}); err != nil {
			return err
		}
	} else {
		currentWake = inspectWakeLock(root, me)
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
		controlStop:       controlStop,
		baselineRequested: *baselineExistingFlag || repairLineage != nil,
		baselineInherited: repairLineage != nil,
		onPrepared: func(watcher wakeAdmissionWatcher) error {
			if repairLineage != nil {
				if err := writeWakePreparedFileInDir(
					repairAgentDir,
					root,
					me,
					currentWake,
				); err != nil {
					return err
				}
				var prepared wakeRepairHandoffPrepared
				err := withWakeLifecycleGuardInDir(repairAgentDir, func(dirfd int) error {
					if err := revalidateWakeRepairRootIdentity(
						root,
						repairLineage.source.RootIdentity,
					); err != nil {
						return err
					}
					current := inspectWakeLockAt(dirfd, repairAgentDir, root, me)
					if !sameWakeLockGeneration(currentWake, current) ||
						current.PID != os.Getpid() ||
						current.Lock.SourceGeneration != repairLineage.source.DeadGeneration ||
						current.Lock.SourceFloorDigest != repairLineage.source.SourceFloorDigest {
						return fmt.Errorf("wake repair lock changed before preparation")
					}
					persisted, exists, err := readWakeTargetAt(
						dirfd,
						repairAgentDir,
						root,
						me,
					)
					if err != nil {
						return err
					}
					if !exists || !sameWakeTarget(persisted, *target) {
						return fmt.Errorf("wake repair target changed before preparation")
					}
					targetDigest, err := wakeTargetDigest(persisted)
					if err != nil {
						return err
					}
					floorSnapshot, exists, err := readWakeRepairFloorSnapshotAt(dirfd, repairAgentDir)
					if err != nil {
						return err
					}
					floor := floorSnapshot.Floor
					if !exists ||
						floor.Generation != current.Lock.Generation ||
						floor.SourceGeneration != repairLineage.source.DeadGeneration ||
						floor.SourceFloorDigest != repairLineage.source.SourceFloorDigest ||
						floor.RootIdentity != repairLineage.source.RootIdentity {
						return fmt.Errorf("wake repair floor changed before preparation")
					}
					floorDigest, err := wakeRepairFloorDigest(floor)
					if err != nil {
						return err
					}
					floorAuthority, err := newWakeRepairFloorAuthority(floorSnapshot)
					if err != nil {
						return err
					}
					prepared, err = newWakeRepairHandoffPrepared(
						childRepairSource(repairLineage),
						os.Getpid(),
						current.Lock.Generation,
						targetDigest,
						floorDigest,
						floorAuthority,
					)
					return err
				})
				if err != nil {
					return err
				}
				if err := repairHandoff.SendPrepared(prepared); err != nil {
					return err
				}
				if err := repairHandoff.AwaitAdmitAcknowledgeAndRelease(
					prepared,
					func() error {
						return validateWakeRepairChildAdmission(
							watcher,
							root,
							me,
							childRepairSource(repairLineage),
						)
					},
				); err != nil {
					return err
				}
				select {
				case <-privateStop:
					return fmt.Errorf("wake repair child stopped before admission completed")
				default:
				}
				return nil
			}
			if err := writeWakePreparedFile(root, me, currentWake); err != nil {
				return err
			}
			return writeWakeReadyFile(root, me, readyFile, currentWake)
		},
	}
	if repairInboxDir != nil {
		cfg.retainedInbox = repairInboxDir
		cfg.touchPresence = func() error {
			return touchWakePresenceInDir(repairAgentDir, me)
		}
	}
	if repairLineage != nil {
		cfg.baselineExisting = cloneWakeFileIdentities(repairLineage.floor.Existing)
	}
	if target != nil && target.Owner == nil {
		persistedTarget := *target
		cfg.onBaselineReady = func(existing map[string]wakeFileIdentity) error {
			if repairLineage != nil {
				floor, err := newInheritedWakeRepairFloor(
					repairLineage.source,
					currentWake.Lock,
					persistedTarget,
					existing,
				)
				if err != nil {
					return err
				}
				return withWakeLifecycleGuardInDir(repairAgentDir, func(dirfd int) error {
					current := inspectWakeLockAt(dirfd, repairAgentDir, root, me)
					if !sameWakeLockGeneration(currentWake, current) {
						return fmt.Errorf("wake repair lock changed before inherited floor publication")
					}
					authority, err := writeWakeRepairFloorAndCaptureAuthorityAt(
						dirfd,
						repairAgentDir,
						root,
						floor,
					)
					if err != nil {
						return err
					}
					repairFloorAuthority = authority
					return nil
				})
			}
			floor, err := newWakeRepairFloor(root, me, currentWake.Lock, persistedTarget, existing)
			if err != nil {
				return err
			}
			return writeWakeRepairFloor(root, me, floor)
		}
	}

	return loop(cfg)
}

var snapshotWakeDirEntryInfo = func(entry os.DirEntry) (os.FileInfo, error) {
	return entry.Info()
}

func snapshotWakeExistingMessages(root, me string) (map[string]wakeFileIdentity, error) {
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, me))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]wakeFileIdentity{}, nil
		}
		return nil, err
	}
	baseline := make(map[string]wakeFileIdentity, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		info, err := snapshotWakeDirEntryInfo(entry)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		identity, ok := captureWakeFileIdentity(info)
		if !ok {
			return nil, fmt.Errorf("capture identity for %s", name)
		}
		baseline[name] = identity
	}
	return baseline, nil
}

func invalidateWakeBaselineEvent(cfg *wakeConfig, event fsnotify.Event) {
	if event.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write|fsnotify.Remove) == 0 {
		return
	}
	name := filepath.Base(event.Name)
	if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
		return
	}
	delete(cfg.baselineExisting, name)
}

// prepareWakeBaseline classifies startup backlog after the watcher is armed.
// Linux/inotify provides an ordered marker fence; Darwin/kqueue uses the marker
// plus a quiescence window, with watcher errors handled fail-closed.
func prepareWakeBaseline(cfg *wakeConfig, watcher *fsnotify.Watcher, inboxNew string) error {
	return prepareWakeBaselineEvents(cfg, watcher.Events, watcher.Errors, inboxNew)
}

func prepareWakeBaselineEvents(
	cfg *wakeConfig,
	events <-chan fsnotify.Event,
	watcherErrors <-chan error,
	inboxNew string,
) error {
	if !cfg.baselineRequested {
		return nil
	}
	if cfg.baselineInherited {
		if cfg.baselineExisting == nil {
			cfg.baselineExisting = map[string]wakeFileIdentity{}
		}
		return nil
	}
	// Individual local-filesystem calls are intentionally not cancellable. Coop
	// has an outer readiness timeout; standalone wake can wait on a stuck scan.
	baseline, err := snapshotWakeExistingMessages(cfg.root, cfg.me)
	if err != nil {
		return fmt.Errorf("snapshot existing wake messages: %w", err)
	}
	cfg.baselineExisting = baseline

	marker, err := os.CreateTemp(inboxNew, ".wake-baseline-barrier-")
	if err != nil {
		return fmt.Errorf("create wake baseline barrier: %w", err)
	}
	markerPath := marker.Name()
	if err := marker.Close(); err != nil {
		_ = os.Remove(markerPath)
		return fmt.Errorf("close wake baseline barrier: %w", err)
	}
	// A crash can leave this hidden marker behind; message scans ignore it.
	defer func() { _ = os.Remove(markerPath) }()

	timer := time.NewTimer(wakeBaselineTimeout)
	defer timer.Stop()
	var settleTimer *time.Timer
	var settleC <-chan time.Time
	defer func() {
		if settleTimer != nil {
			settleTimer.Stop()
		}
	}()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return failWakeOnWatcherError(cfg, "watcher closed while preparing wake baseline", nil)
			}
			invalidateWakeBaselineEvent(cfg, event)
			if filepath.Clean(event.Name) == filepath.Clean(markerPath) && event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				if settleTimer == nil {
					settleTimer = time.NewTimer(wakeBaselineSettle)
				} else {
					settleTimer.Reset(wakeBaselineSettle)
				}
				settleC = settleTimer.C
			} else if settleTimer != nil {
				if !settleTimer.Stop() {
					select {
					case <-settleTimer.C:
					default:
					}
				}
				settleTimer.Reset(wakeBaselineSettle)
			}
		case err, ok := <-watcherErrors:
			if !ok {
				return failWakeOnWatcherError(cfg, "watcher closed while preparing wake baseline", nil)
			}
			return failWakeOnWatcherError(cfg, "watcher error while preparing wake baseline", err)
		case <-timer.C:
			return fmt.Errorf("wake baseline barrier was not observed within %s", wakeBaselineTimeout)
		case <-settleC:
			return nil
		}
	}
}

func failWakeOnWatcherError(cfg *wakeConfig, context string, cause error) error {
	// Once event history is uncertain, retaining baseline tombstones could
	// suppress a real arrival. Exit with them cleared so any restart scans all.
	cfg.baselineExisting = nil
	if cause == nil {
		return errors.New(context)
	}
	return fmt.Errorf("%s: %w", context, cause)
}

func pendingWakeWatcherError(watcher wakeAdmissionWatcher) error {
	if watcher == nil {
		return fmt.Errorf("wake watcher is unavailable at admission")
	}
	select {
	case err, ok := <-watcher.Errors():
		if !ok {
			return fmt.Errorf("wake watcher closed before admission")
		}
		if err == nil {
			return fmt.Errorf("wake watcher reported an empty error before admission")
		}
		return fmt.Errorf("wake watcher failed before admission: %w", err)
	default:
		return nil
	}
}

func validateWakeRepairChildAdmission(
	watcher wakeAdmissionWatcher,
	root, me string,
	source wakeRepairHandoffSource,
) error {
	if err := pendingWakeWatcherError(watcher); err != nil {
		return err
	}
	return validateCanonicalWakeRepairDirectories(root, me, source)
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

	var watcher wakeEventWatcher
	if retained, ok := cfg.retainedInbox.(*wakeInboxDir); ok {
		var err error
		watcher, err = retained.NewWatcher()
		if err != nil {
			return fmt.Errorf("failed to create retained watcher: %w", err)
		}
	} else {
		if err := os.MkdirAll(inboxNew, 0o700); err != nil {
			return err
		}
		native, err := fsnotify.NewWatcher()
		if err != nil {
			return fmt.Errorf("failed to create watcher: %w", err)
		}
		if err := native.Add(inboxNew); err != nil {
			_ = native.Close()
			return fmt.Errorf("failed to watch inbox: %w", err)
		}
		watcher = &fsnotifyWakeEventWatcher{watcher: native}
	}
	defer func() { _ = watcher.Close() }()

	// The startup boundary is watcher installation, not lock acquisition;
	// messages delivered in between are intentionally treated as startup backlog.
	if err := prepareWakeBaselineEvents(&cfg, watcher.Events(), watcher.Errors(), inboxNew); err != nil {
		return err
	}
	if cfg.onBaselineReady != nil {
		if err := cfg.onBaselineReady(cloneWakeFileIdentities(cfg.baselineExisting)); err != nil {
			return err
		}
	}
	// This closes the already-pending stop case only; a stop or process death can
	// still race immediately after readiness publication.
	select {
	case <-cfg.controlStop:
		return nil
	default:
	}
	if cfg.onPrepared != nil {
		if err := cfg.onPrepared(watcher); err != nil {
			return err
		}
	}
	select {
	case <-cfg.controlStop:
		return nil
	default:
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
	if cfg.touchPresence != nil {
		_ = cfg.touchPresence()
	} else {
		_ = presence.Touch(cfg.root, cfg.me)
	}

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
		case <-cfg.controlStop:
			return nil
		case <-sigCh:
			// Clean exit on SIGHUP/SIGTERM
			return nil

		case event, ok := <-watcher.Events():
			if !ok {
				return failWakeOnWatcherError(&cfg, "watcher closed", nil)
			}
			invalidateWakeBaselineEvent(&cfg, event)
			// Only care about new files
			if event.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write) == 0 {
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

		case err, ok := <-watcher.Errors():
			if !ok {
				return failWakeOnWatcherError(&cfg, "watcher closed", nil)
			}
			return failWakeOnWatcherError(&cfg, "amq wake: watcher error", err)

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
			if cfg.touchPresence != nil {
				_ = cfg.touchPresence()
			} else {
				_ = presence.Touch(cfg.root, cfg.me)
			}

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
