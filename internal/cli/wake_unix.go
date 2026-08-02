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
	"runtime"
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
	wakeTerminateGrace              = 100 * time.Millisecond
	wakeBaselineTimeout             = 5 * time.Second
	wakeBaselineSettle              = 50 * time.Millisecond
	wakeTerminalAuthorityRetryDelay = 250 * time.Millisecond
	wakeInboxScanRetryBase          = 250 * time.Millisecond
	wakeInboxScanRetryMax           = 30 * time.Second
	getWakeCurrentTTY               = getCurrentTTY
	getWakeProcessSID               = unix.Getsid
	wakeTIOCSTIAvailable            = func() bool { return tiocsti.Available() }
	wakeInputIsTTY                  = func() bool { return tiocsti.IsTTY() }
	newWakeBaselineEventWatcher     = newWakePathEventWatcher
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

func newWakePathEventWatcher(path string) (wakeEventWatcher, error) {
	native, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := native.Add(path); err != nil {
		_ = native.Close()
		return nil, err
	}
	return &fsnotifyWakeEventWatcher{watcher: native}, nil
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
	acceptExistingValid     bool
	refuseUnverifiedGeneric bool
	target                  *wakeTarget
	wakeMode                string
	requestedOwner          *wakeOwner
	repairLineage           *wakeRepairLineage
	repairFloorAuthority    *wakeRepairFloorAuthority
	// resumeEligible is startup policy, not authority. newWakeLock still requires
	// the complete owner/process/control/image advertisement below.
	resumeEligible bool
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
var startWakeReloadTransportForWake = startWakeReloadTransportInDir
var afterExistingWakeReadyPublication = func() {}
var waitForWakeRepairChildExit = func(waiter *wakeProcessWaiter) error {
	return waiter.waitForExit(wakeProcessExitTimeout)
}

// acquireWakeLock attempts to acquire the wake lock for an agent's inbox.
// Returns cleanup function and error. If another wake is running, returns error.
func acquireWakeLock(root, me string, target *wakeTarget) (cleanup func(), err error) {
	return acquireWakeLockWithOptions(root, me, wakeLockAcquireOptions{target: target})
}

func acquireWakeLockWithOptions(root, me string, options wakeLockAcquireOptions) (cleanup func(), err error) {
	agentDir, innerCleanup, err := acquireWakeLockWithOptionsRetained(root, me, options)
	if err != nil {
		return nil, err
	}
	return func() {
		innerCleanup()
		_ = agentDir.Close()
	}, nil
}

func acquireWakeLockWithOptionsRetained(
	root, me string,
	options wakeLockAcquireOptions,
) (agentDir *wakeAgentDir, cleanup func(), err error) {
	agentDir, err = openWakeAgentDir(root, me)
	if err != nil {
		return nil, nil, err
	}
	innerCleanup, err := acquireWakeLockWithOptionsInDir(agentDir, root, me, options)
	if err != nil {
		_ = agentDir.Close()
		return nil, nil, err
	}
	return agentDir, innerCleanup, nil
}

func supersedeUnverifiedGenericWakeAt(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
) error {
	current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
	if !sameWakeLockGeneration(expected, current) {
		return fmt.Errorf("unverified wake changed before supersession; retry")
	}
	if current.Status != wakeLockUnverified ||
		classifyPersistedWakeClaim(current) != wakeClaimGeneric {
		return fmt.Errorf(
			"wake state for %s is not an unverified ownerless generic claim; use 'amq wake recover-owner --me %s' for an owner-bound claim",
			expected.Agent,
			expected.Agent,
		)
	}
	if err := removeWakeLockIfUnchangedGuardedAt(dirfd, agentDir, current); err != nil {
		return fmt.Errorf("supersede exact unverified wake claim: %w", err)
	}

	tty := strings.TrimSpace(current.Lock.TTY)
	if tty == "" {
		tty = "unknown"
	}
	_ = writeStderr(
		"warning: superseded unidentified wake helper for %s (pid %d on %s) without signaling it; fresh wake is starting, but duplicate notifications may continue until the old helper exits; stop that helper if duplicates persist\n",
		current.Agent,
		current.Lock.PID,
		tty,
	)
	return nil
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
		return acquireAuthoritativeWakeLockWithOptionsInDir(agentDir, root, me, options)
	}

	for {
		var replace wakeLockInspection
		var created wakeLockInspection
		err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
			inspection := inspectWakeLockAt(dirfd, agentDir, root, me)
			if options.repairLineage != nil && inspection.Exists {
				return fmt.Errorf("wake lock changed before repair acquisition")
			}
			if inspection.Status == wakeLockUnverified && wakeLockHasOwnerMarkers(inspection) {
				return fmt.Errorf(
					"wake state for %s is unverified: %s; run 'amq wake recover-owner --me %s'",
					me,
					inspection.Reason,
					me,
				)
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
					if err := removeWakeLockIfUnchangedGuardedAt(
						dirfd,
						agentDir,
						inspection,
					); err != nil {
						return err
					}
				case wakeLockCreating:
					return &wakeLockCreatingError{}
				case wakeLockValid:
					if options.acceptExistingValid {
						if options.requestedOwner != nil &&
							(inspection.Lock.WakeMode != wakeOwnerWakeMode ||
								!sameWakeOwner(inspection.Lock.Owner, options.requestedOwner)) {
							return fmt.Errorf(
								"existing wake for %s is not bound to the requested exact owner; refusing readiness reuse",
								me,
							)
						}
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
					if options.refuseUnverifiedGeneric {
						return fmt.Errorf(
							"wake lock for %s cannot be verified; lock: %s; root: %s; retry coop exec with -y",
							me,
							inspection.LockPath,
							inspection.Root,
						)
					}
					if err := supersedeUnverifiedGenericWakeAt(dirfd, agentDir, inspection); err != nil {
						return err
					}
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
			if options.target != nil {
				if err := publishWakeStateAndBindLockAt(dirfd, agentDir, root, me, &lock); err != nil {
					return fmt.Errorf("publish wake state before generic wake lock: %w", err)
				}
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
			if options.target != nil {
				if err := validateWakeBoundStateAt(dirfd, agentDir, root, me, created.Lock); err != nil {
					return fmt.Errorf("verify created generic wake binding: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if replace.Exists {
			if _, err := terminateAndRemoveOrphanedWakeLockInDir(
				agentDir,
				replace,
			); err != nil {
				return nil, err
			}
			continue
		}

		cleanup = func() {
			if cleanupErr := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
				return cleanupGenericWakeGenerationAt(
					dirfd,
					agentDir,
					root,
					me,
					created,
					options,
				)
			}); cleanupErr != nil {
				_ = writeStderr("amq wake: cleanup failed: %v\n", cleanupErr)
			}
		}
		return cleanup, nil
	}
}

func cleanupGenericWakeGenerationAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	created wakeLockInspection,
	options wakeLockAcquireOptions,
) error {
	current := inspectWakeLockAt(dirfd, agentDir, root, me)
	if !sameWakeLockGeneration(created, current) || !currentWakeLockMatches(current.Lock) {
		return nil
	}

	preparedSnapshot, preparedSnapshotErr := freezeGenericWakePreparedCleanupAt(
		dirfd,
		agentDir,
		root,
		me,
		current,
	)
	if err := removeWakeLockIfUnchangedGuardedAt(dirfd, agentDir, current); err != nil {
		return errors.Join(
			preparedSnapshotErr,
			fmt.Errorf("remove exact generic wake lock: %w", err),
		)
	}
	var lockSyncErr error
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		lockSyncErr = fmt.Errorf("sync exact generic wake lock removal: %w", err)
	}

	interleaveErr := afterGenericWakeLockRemoval(dirfd, agentDir)
	replacement := inspectWakeLockAt(dirfd, agentDir, root, me)
	var replacementErr error
	if replacement.Exists {
		replacementErr = fmt.Errorf(
			"replacement wake lock appeared during generic wake cleanup; preserving prepared marker",
		)
	}

	var preparedCleanupErr error
	if !replacement.Exists && preparedSnapshot != nil {
		_, preparedCleanupErr = removeWakeGenerationFileIfSnapshotMatchesAt(
			dirfd,
			agentDir,
			wakePreparedFileName,
			"wake prepared marker",
			*preparedSnapshot,
		)
		if preparedCleanupErr != nil {
			preparedCleanupErr = fmt.Errorf(
				"remove exact generic wake prepared marker: %w",
				preparedCleanupErr,
			)
		}
	}
	var stateRefreshErr error
	if !replacement.Exists {
		stateRefreshErr = reconcileWakeStateAfterLegacyMutationAt(dirfd, agentDir, root, me)
		if stateRefreshErr != nil {
			if continueAfterWakeStateProjectionError(stateRefreshErr) {
				stateRefreshErr = nil
			} else {
				stateRefreshErr = fmt.Errorf("refresh wake state after generic cleanup: %w", stateRefreshErr)
			}
		}
	}

	floorCleanupErr := cleanupGenericWakeRepairFloorAt(
		dirfd,
		agentDir,
		created,
		options,
	)
	return errors.Join(
		preparedSnapshotErr,
		lockSyncErr,
		interleaveErr,
		replacementErr,
		preparedCleanupErr,
		stateRefreshErr,
		floorCleanupErr,
	)
}

func cleanupGenericWakeRepairFloorAt(
	dirfd int,
	agentDir *wakeAgentDir,
	created wakeLockInspection,
	options wakeLockAcquireOptions,
) error {
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
	if imagePath, imageErr := os.Executable(); imageErr == nil {
		imagePath = strings.TrimSpace(imagePath)
		if imagePath != "" {
			if resolved, resolveErr := filepath.EvalSymlinks(imagePath); resolveErr == nil {
				imagePath = resolved
			}
			if filepath.IsAbs(imagePath) {
				lock.ImagePath = filepath.Clean(imagePath)
			}
		}
	}
	lock.ImageVersion = strings.TrimSpace(cliVersion)
	if runtime.GOOS == "darwin" {
		// Darwin has no retained executable FD. Capture the observed pathname
		// identity for diagnostics, but do not treat it as agent-safe resume
		// authority after exec.
		if evidence, evidenceErr := captureCurrentWakeImageEvidence(); evidenceErr == nil {
			lock.RunningImageEvidence = &evidence
			lock.ImagePath = evidence.ExecutionPath
			lock.ImageVersion = evidence.EmbeddedVersion
		}
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
	if options.resumeEligible && options.requestedOwner != nil && options.repairLineage == nil {
		// Resume metadata is additive capability. Missing or inconsistent evidence
		// keeps the ordinary notifier usable without advertising agent-safe reload.
		candidate := lock
		if candidate.ControlSocket == "" {
			candidate.ControlSocket = wakeControlSocketPath(root, me, candidate.Generation)
		}
		evidence := lock.RunningImageEvidence
		if evidence == nil {
			captured, evidenceErr := captureCurrentWakeImageEvidence()
			if evidenceErr == nil {
				evidence = &captured
			}
		}
		if evidence != nil {
			owner := *options.requestedOwner
			candidate.ResumeSchema = wakeResumeSchemaV2
			candidate.ResumeOwner = &owner
			candidate.RunningImageEvidence = evidence
			candidate.ImagePath = evidence.ExecutionPath
			candidate.ImageVersion = evidence.EmbeddedVersion
			if validateWakeResumeAdvertisement(candidate, root, me) == nil {
				lock = candidate
			}
		}
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

	// Process is a confirmed matching amq wake. If its TTY disappeared, stop
	// that orphan before taking over; never signal an unconfirmed PID.
	if wakeLockTerminalGone(inspection) {
		return true
	}

	return wakeLockSharesCurrentTerminalDifferentSession(inspection)
}

func wakeLockSharesCurrentTerminalDifferentSession(inspection wakeLockInspection) bool {
	if !sameWakeTerminalAsCurrent(inspection) {
		return false
	}
	existingSID, existingErr := getWakeProcessSID(inspection.Lock.PID)
	currentSID, currentErr := getWakeProcessSID(0)
	return existingErr == nil && currentErr == nil && existingSID != currentSID
}

func sameWakeTTYPathAsCurrent(inspection wakeLockInspection) bool {
	currentTTY := getWakeCurrentTTY()
	existingTTY := inspection.Lock.TTY
	if strings.HasPrefix(existingTTY, "/dev/") {
		if real, err := filepath.EvalSymlinks(existingTTY); err == nil {
			existingTTY = real
		}
	}
	return currentTTY != "" && currentTTY == existingTTY
}

type wakeTerminalAttachment uint8

const (
	wakeTerminalUndeterminable wakeTerminalAttachment = iota
	wakeTerminalGone
	wakeTerminalAttached
)

func wakeLockTerminalAttachment(inspection wakeLockInspection) wakeTerminalAttachment {
	tty := strings.TrimSpace(inspection.Lock.TTY)
	if strings.HasPrefix(tty, "/dev/") {
		if _, statErr := os.Stat(tty); os.IsNotExist(statErr) {
			return wakeTerminalGone
		}
	}
	if inspection.Process.ControllingTerminalKnown {
		if inspection.Process.HasControllingTerminal {
			return wakeTerminalAttached
		}
		return wakeTerminalGone
	}
	return wakeTerminalUndeterminable
}

func wakeLockTerminalGone(inspection wakeLockInspection) bool {
	return wakeLockTerminalAttachment(inspection) == wakeTerminalGone
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
		return fmt.Errorf("existing wake lock for %s is not usable for requested wake readiness (pid %d on %s since %s)",
			inspection.Agent, inspection.Lock.PID, inspection.Lock.TTY, inspection.Lock.Started)
	}
	if wakeLockNeedsReplacement(inspection) {
		return fmt.Errorf("existing wake lock for %s is not usable for requested wake readiness (pid %d on %s since %s)",
			inspection.Agent, inspection.Lock.PID, inspection.Lock.TTY, inspection.Lock.Started)
	}
	if requiredMode == wakeTargetInjectVia {
		if requestedTarget == nil {
			return fmt.Errorf("existing inject-via wake for %s cannot be reused without a requested wake target", inspection.Agent)
		}
		persistedTarget, exists, err := readWakeTargetFromStateForInspection(
			inspection.Root, inspection.Agent, inspection,
		)
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
	switch wakeLockTerminalAttachment(inspection) {
	case wakeTerminalGone:
		return false
	case wakeTerminalAttached:
		return true
	}
	// Linux cannot currently inspect controlling-terminal attachment. Preserve
	// its concrete-path evidence while treating an absent/legacy unknown name
	// as unusable when attachment is undeterminable.
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
		case "check":
			return runWakeCheck(args[1:])
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
		"Repair an eligible wake by restarting it from a saved inject-via target.",
		"",
		"Accepts proven-stale or unverified ownerless generic locks. Refuses",
		"owner-bound or invalid unverified claims and raw terminal wake targets.",
		"This command only uses .wake.target files created for --inject-via wakes.")
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
	var unverifiedSupersession wakeLockInspection
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
			if classifyPersistedWakeClaim(inspection) != wakeClaimGeneric {
				result.Status = "refused"
				result.PID = inspection.PID
				result.Reason = fmt.Sprintf(
					"unverified wake is not an ownerless generic claim; use 'amq wake recover-owner --me %s' for an owner-bound claim",
					me,
				)
				return errors.New(result.Reason)
			}
			unverifiedSupersession = inspection
		default:
			result.Status = "refused"
			result.Reason = fmt.Sprintf("wake lock status %q is not repairable", inspection.Status)
			return errors.New(result.Reason)
		}

		var exists bool
		var err error
		target, exists, err = readWakeTargetFromStateForInspectionAt(
			dirfd, agentDir, root, me, inspection,
		)
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
		var removeErr error
		if unverifiedSupersession.Exists {
			removeErr = supersedeUnverifiedGenericWakeAt(
				dirfd,
				agentDir,
				unverifiedSupersession,
			)
		} else {
			removeErr = removeWakeLockIfUnchangedGuardedAt(dirfd, agentDir, inspection)
		}
		if removeErr != nil {
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
	if !child.capabilityDetached && child.Capability != nil {
		cleanupErr = errors.Join(cleanupErr, child.Capability.Stop())
	}
	// Close the unreleased handoff before waiting so a child blocked on parent
	// admission observes EOF and can terminate.
	cleanupErr = errors.Join(cleanupErr, child.Handoff.Close(), child.Capability.Close())
	if child.Waiter == nil {
		return errors.Join(cleanupErr, errors.New("wake repair child exit waiter is missing"))
	}
	waitErr := waitForWakeRepairChildExit(child.Waiter)
	cleanupErr = errors.Join(cleanupErr, waitErr)
	if waitErr != nil {
		// A stop request is not exit evidence. Preserve the exact lock and floor
		// while the child may still be alive so no competing wake can take over.
		return cleanupErr
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
	return openWakeOutputInDir(
		agentDir,
		".wake.repair.log",
		"repair wake log",
	)
}

func openCoopWakeOutput(root, me string) (*os.File, error) {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return nil, err
	}
	defer func() { _ = agentDir.Close() }()
	return openWakeOutputInDir(agentDir, ".wake.log", "wake log")
}

// wakeOutputMaxBytes bounds accumulated diagnostics both at launch and on the
// existing wake maintenance tick.
const wakeOutputMaxBytes int64 = 1 << 20

var truncateWakeOutput = unix.Ftruncate

func maintainWakeOutputBounds(outputs ...*os.File) error {
	var bounded []os.FileInfo
	var errs []error
	for _, output := range outputs {
		if output == nil {
			continue
		}
		info, err := output.Stat()
		if err != nil {
			errs = append(errs, fmt.Errorf("stat wake output %s: %w", output.Name(), err))
			continue
		}
		if !info.Mode().IsRegular() || info.Size() < wakeOutputMaxBytes {
			continue
		}
		duplicate := false
		for _, prior := range bounded {
			if os.SameFile(prior, info) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		bounded = append(bounded, info)
		if err := truncateWakeOutput(int(output.Fd()), 0); err != nil {
			errs = append(errs, fmt.Errorf("truncate wake output %s: %w", output.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func openWakeOutputInDir(
	agentDir *wakeAgentDir,
	name, label string,
) (*os.File, error) {
	if agentDir == nil {
		return nil, fmt.Errorf("%s agent directory capability is missing", label)
	}
	var file *os.File
	var truncateErr error
	err := agentDir.withFD(func(dirfd int) error {
		fd, err := unix.Openat(
			dirfd,
			name,
			unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0o600,
		)
		if err != nil {
			if err == unix.ELOOP {
				return fmt.Errorf("%s %s must not be a symlink", label, filepath.Join(agentDir.path, name))
			}
			if err == unix.ENXIO {
				return fmt.Errorf("%s %s must be a regular file", label, filepath.Join(agentDir.path, name))
			}
			return fmt.Errorf("open %s: %w", label, err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("stat %s: %w", label, err)
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			_ = unix.Close(fd)
			return fmt.Errorf("%s %s must be a regular file", label, filepath.Join(agentDir.path, name))
		}
		if stat.Size >= wakeOutputMaxBytes {
			if err := truncateWakeOutput(fd, 0); err != nil {
				truncateErr = err
			}
		}
		file = os.NewFile(uintptr(fd), filepath.Join(agentDir.path, name))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if truncateErr != nil {
		_, _ = fmt.Fprintf(
			file,
			"amq wake: warning: %s reached the launch bound but could not be truncated: %v; continuing without truncation\n",
			label,
			truncateErr,
		)
	}
	if info, err := file.Stat(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat %s: %w", label, err)
	} else if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%s %s must be a regular file", label, file.Name())
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

func runWakeWithLoop(args []string, loop wakeLoopFunc) (returnErr error) {
	privateStop, cleanupPrivateStop, err := authoritativeWakePrivateStopFromEnv()
	if err != nil {
		return err
	}
	defer cleanupPrivateStop()
	attention, err := wakeAttentionFromEnv()
	if err != nil {
		return err
	}
	if attention != nil {
		defer func() { _ = attention.Close() }()
	}
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
	inputQuietForFlag := fs.Duration("input-quiet-for", 1200*time.Millisecond, "Quiet window before deferred injection (advisory only on Linux; tty atime granularity is ~8s)")
	inputPollIntervalFlag := fs.Duration("input-poll-interval", 200*time.Millisecond, "Polling interval while waiting for quiet terminal input")
	inputMaxHoldFlag := fs.Duration("input-max-hold", 15*time.Second, "Maximum time to defer one wake injection (0 = no hold)")
	interruptFlag := fs.Bool("interrupt", true, "Enable interrupt injection for urgent interrupt messages")
	interruptLabelFlag := fs.String("interrupt-label", "interrupt", "Label required to trigger interrupt")
	interruptPriorityFlag := fs.String("interrupt-priority", "urgent", "Priority required to trigger interrupt")
	interruptCmdFlag := fs.String("interrupt-cmd", "none", "Interrupt command to inject: none (default) or ctrl-c (sends real SIGINT to the foreground process group and can interrupt or crash the agent)")
	interruptNoticeFlag := fs.String("interrupt-notice", "", "Custom interrupt notice (default: auto)")
	interruptCooldownFlag := fs.Duration("interrupt-cooldown", 7*time.Second, "Minimum time between interrupts")
	readyFileFlag := fs.String("ready-file", "", "Internal: write this file after wake lock acquisition")
	debugFlag := fs.Bool("debug", false, "Log injection diagnostics to stderr")
	acceptExistingWakeFlag := fs.Bool("accept-existing-wake", false, "Internal: allow a usable existing wake to satisfy readiness")
	refuseUnverifiedWakeFlag := fs.Bool("refuse-unverified-wake", false, "Internal: refuse unverified wake locks instead of superseding them")
	repairLineageFlag := fs.String("repair-lineage", "", "Internal: inherit the suppression floor from an exact dead wake generation")
	baselineExistingFlag := fs.Bool("baseline-existing", false, "Ignore messages already waiting when this wake starts")

	usage := usageWithHiddenFlags(fs, "amq wake --me <agent> [options]",
		[]string{"ready-file", "accept-existing-wake", "refuse-unverified-wake", "repair-lineage"},
		"Background waker: injects terminal notification when messages arrive.",
		"Run as background job before starting CLI: amq wake --me claude --interrupt-cmd none &",
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
		"  Owner-bound co-op wakes retry after the prior command exits or times",
		"  out; retries can repeat arbitrary injector-side effects. Standalone",
		"  ownerless wakes execute the injector once per physical inbox cohort.",
		"  Example: amq wake --me orchestrator --inject-via /path/to/ghostty-bridge \\",
		"    --inject-arg exec --inject-arg \"$TERMINAL_ID\"",
		"  Trust boundary: --inject-via executes local code, and the payload can",
		"  contain sanitized but message-derived header content.",
		"",
		"Input deferral (default on): wake samples terminal input only after",
		"  a message is pending, then injects after a short quiet window.",
		"  Collision reduction only: it cannot detect permission/approval dialogs.",
		"  A pause longer than --input-quiet-for can still inject while a prompt",
		"  is being composed. If input remains active through --input-max-hold,",
		"  wake emits the notice out-of-band and skips synthetic input. If input",
		"  sampling is unavailable, injection remains best-effort. Interrupt",
		"  messages bypass deferral.",
		"  Atime sampling uses stdin (when a TTY) for cross-platform fidelity;",
		"  Linux tty atime is updated at ~8s granularity, so it cannot establish",
		"  a precise 1200ms idle window. On Linux this heuristic is advisory.",
		"",
		"Interrupt notices (default on): urgent messages tagged with label \"interrupt\"",
		"  trigger an interrupt notice. Ctrl+C injection is opt-in with",
		"  --interrupt-cmd ctrl-c; it sends real SIGINT to the foreground process",
		"  group and can interrupt or crash the agent.",
		"",
		"Safety: raw, paste, --inject-cmd, --inject-via, and opt-in interrupt Ctrl+C",
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
	requestedInjectMode := injectMode

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

	requestedOwner, err := wakeOwnerFromEnv()
	if err != nil {
		return err
	}
	if repairGeneration != "" && requestedOwner != nil {
		return fmt.Errorf("owner-bearing wake state requires 'amq wake recover-owner --me %s'", me)
	}
	var ownerObservation wakeOwnerObservation
	if requestedOwner != nil {
		ownerObservation, err = observeLiveWakeOwner(
			*requestedOwner,
			"inspect requested wake owner before lock acquisition",
		)
		if err != nil {
			return err
		}
		defer func() {
			if err := ownerObservation.Close(); err != nil {
				returnErr = errors.Join(
					returnErr,
					fmt.Errorf("requested wake owner observation failed: %w", err),
				)
			}
		}()
	}

	var initialNotifierStatus string
	var initialNotifierMode string
	var initialNotifierReason string

	// Verify TIOCSTI is available (skip in inject-via mode — uses external command instead).
	// Linux's legacy_tiocsti sysctl is advisory: a readable zero degrades this
	// wake to a useful non-input notifier, while absence, read errors, and every
	// other value remain unknown and do not block startup.
	if injectVia == "" && injectMode != wakeInjectModeNone {
		if !wakeTIOCSTIAvailable() {
			return errors.New("TIOCSTI not available on this platform; use tmux send-keys or terminal-specific injection")
		}

		if tiocstiLegacyDisabledHint() {
			initialNotifierStatus = wakeInjectorUnsupportedStatus
			initialNotifierMode = effectiveInjectMode(&wakeConfig{me: me, injectMode: injectMode})
			initialNotifierReason = wakeInjectorUnsupportedReason(
				initialNotifierMode,
				fmt.Errorf("%s is 0", tiocstiLegacySysctlPath),
			)
			injectMode = wakeInjectModeNone
		} else if !wakeInputIsTTY() {
			// Verify we have a real TTY when synthetic input remains enabled.
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
		value, err := newWakeTarget(root, me, injectVia, []string(injectArgFlags))
		if err != nil {
			return err
		}
		value.Owner = requestedOwner
		if err := validateWakeTarget(value, root, me); err != nil {
			return err
		}
		injectVia = value.InjectVia
		target = &value
		if repairGeneration != "" {
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
	activeAgentDir := repairAgentDir
	if activeAgentDir == nil {
		activeAgentDir, err = openWakeAgentDir(root, me)
		if err != nil {
			return err
		}
		defer func() { _ = activeAgentDir.Close() }()
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
	resumeEligible := wakeResumeStartupEligible(
		requestedOwner,
		repairLineage != nil,
		*injectCmdFlag,
		interruptKey,
		lockWakeMode,
	)
	reloadTransportEligible := resumeEligible && target == nil
	var cleanup func()
	var repairFloorAuthority wakeRepairFloorAuthority
	for {
		if requestedOwner != nil {
			select {
			case <-ownerObservation.Done():
				return fmt.Errorf("requested wake owner observation ended before lock acquisition")
			default:
			}
		}
		options := wakeLockAcquireOptions{
			acceptExistingValid:     acceptExistingWake,
			refuseUnverifiedGeneric: *refuseUnverifiedWakeFlag,
			target:                  target,
			wakeMode:                lockWakeMode,
			requestedOwner:          requestedOwner,
			repairLineage:           repairLineage,
			resumeEligible:          resumeEligible,
		}
		if repairLineage != nil {
			options.repairFloorAuthority = &repairFloorAuthority
		}
		cleanup, err = acquireWakeLockWithOptionsInDir(activeAgentDir, root, me, options)
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
		var snapshotChanged *wakeSnapshotReadChangedError
		var boundInconclusive *wakeStateBoundInconclusiveError
		if acceptExistingWake && (errors.As(err, &snapshotChanged) || errors.As(err, &boundInconclusive)) {
			if !waitForWakePreparedRetry(acceptExistingDeadline) {
				return fmt.Errorf(
					"wake lock did not stabilize within %s: %w",
					wakeReadyTimeout,
					err,
				)
			}
			continue
		}
		var alreadyRunning *wakeAlreadyRunningError
		if acceptExistingWake && errors.As(err, &alreadyRunning) {
			if requestedOwner != nil {
				select {
				case <-ownerObservation.Done():
					return fmt.Errorf("requested wake owner observation ended before existing-wake readiness")
				default:
				}
			}
			publication, err := writeWakeReadyFileForPreparedWakeInDir(
				activeAgentDir,
				root,
				me,
				readyFile,
				alreadyRunning.Inspection,
				acceptExistingDeadline,
			)
			if err != nil {
				return err
			}
			defer func() { _ = publication.Close() }()
			cleanupReady := func(cause error) error {
				return errors.Join(cause, publication.removeIfUnchanged())
			}
			afterExistingWakeReadyPublication()
			if requestedOwner != nil {
				ready, readyErr := validateWakeReadyFileAgainstOwnerInDir(
					activeAgentDir,
					root,
					me,
					readyFile,
					requestedOwner,
				)
				if readyErr != nil {
					return cleanupReady(readyErr)
				}
				if !ready {
					return cleanupReady(fmt.Errorf("existing wake readiness disappeared before owner validation"))
				}
			}
			if err := validateCanonicalWakeAgentDir(activeAgentDir); err != nil {
				return cleanupReady(err)
			}
			if *baselineExistingFlag {
				_ = writeStderr("warning: reusing existing amq wake; this launch did not re-baseline it, so pending backlog may still notify\n")
			}
			return nil
		}
		return err
	}
	defer cleanup()
	controlStop := privateStop
	if requestedOwner != nil {
		controlStop = mergeWakeStopChannels(controlStop, ownerObservation.Done())
	}
	var currentWake wakeLockInspection
	if err := activeAgentDir.withFD(func(dirfd int) error {
		currentWake = inspectWakeLockAt(dirfd, activeAgentDir, root, me)
		return nil
	}); err != nil {
		return err
	}
	if currentWake.Lock.ControlSocket != "" {
		var controlCleanup func()
		var stop <-chan struct{}
		var markStopped func()
		var controlErr error
		controlCleanup, stop, markStopped, controlErr = startWakeControlListenerInDir(
			activeAgentDir, root, me, currentWake.Lock,
		)
		if controlErr != nil {
			return controlErr
		}
		defer controlCleanup()
		defer markStopped()
		if stop != nil {
			controlStop = mergeWakeStopChannels(controlStop, stop)
		}
	}

	if injectVia != "" {
		if err := validateResolvedWakeInjectViaPath(injectVia); err != nil {
			return err
		}
	}

	if err := setWakeNotifierStatusInDir(
		activeAgentDir,
		me,
		initialNotifierStatus,
		initialNotifierMode,
		initialNotifierReason,
	); err != nil {
		return fmt.Errorf("record wake notifier status: %w", err)
	}
	if initialNotifierStatus == wakeInjectorUnsupportedStatus {
		_ = writeStderr("amq wake: warning: %s\n", initialNotifierReason)
	}
	var terminalAuthority *wakeTerminalAuthority
	effectiveMode := effectiveInjectMode(&wakeConfig{me: me, injectMode: injectMode})
	if requestedOwner != nil &&
		injectVia == "" &&
		(effectiveMode == wakeInjectModeRaw || effectiveMode == wakeInjectModePaste) {
		terminalAuthority, err = bindWakeTerminalAuthorityForWake(currentWake, controlStop)
		if err != nil {
			return err
		}
		defer func() { _ = terminalAuthority.Close() }()
	}
	cfg := wakeConfig{
		me:                  me,
		root:                root,
		session:             resolveSessionName(root),
		injectCmd:           *injectCmdFlag,
		injectVia:           injectVia,
		injectArgs:          []string(injectArgFlags),
		wakeOwner:           requestedOwner,
		injectTimeout:       *injectTimeoutFlag,
		bell:                *bellFlag,
		debounce:            *debounceFlag,
		previewLen:          *previewLenFlag,
		strict:              common.Strict,
		fallbackWarn:        true,
		injectMode:          injectMode,
		requestedInjectMode: requestedInjectMode,
		debug:               *debugFlag,
		deferWhileInput:     *deferWhileInputFlag,
		inputQuietFor:       *inputQuietForFlag,
		inputPollInterval:   *inputPollIntervalFlag,
		inputMaxHold:        *inputMaxHoldFlag,
		interrupt:           *interruptFlag,
		interruptLabel:      interruptLabel,
		interruptPriority:   interruptPriority,
		interruptKey:        interruptKey,
		interruptNotice:     strings.TrimSpace(*interruptNoticeFlag),
		interruptCooldown:   *interruptCooldownFlag,
		controlStop:         controlStop,
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeTerminalGeneration(root, me)
		},
		terminalGeneration: currentWake.Lock.Generation,
		terminalTTY:        currentWake.Lock.TTY,
		baselineRequested:  *baselineExistingFlag || repairLineage != nil,
		baselineInherited:  repairLineage != nil,
		retainedAgent:      activeAgentDir,
		recordNotifierStatus: func(status, mode, reason string) error {
			return setWakeNotifierStatusInDir(activeAgentDir, me, status, mode, reason)
		},
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
			if err := writeWakePreparedFileInDir(activeAgentDir, root, me, currentWake); err != nil {
				return err
			}
			return writeWakeReadyFileAgainstOwnerInDir(
				activeAgentDir,
				root,
				me,
				readyFile,
				currentWake,
				requestedOwner,
			)
		},
	}
	if attention != nil {
		cfg.attentionWrite = attention.Write
		cfg.attentionIsTTY = func() bool {
			return wakeAttentionFileIsTerminal(attention)
		}
	}
	if terminalAuthority != nil {
		cfg.beforeTerminalWrite = terminalAuthority.BeforeWrite
		cfg.terminalWrite = terminalAuthority.Inject
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
			return writeWakeRepairFloorInDir(activeAgentDir, root, floor)
		}
	}

	// Reload transport is additive capability. If this process cannot confirm
	// the freshly published lock identity, keep the ordinary notifier running
	// without opening the unadvertised endpoint.
	if reloadTransportEligible && currentWake.IdentityConfirmed {
		if requestedOwner == nil {
			return fmt.Errorf("wake reload transport requires an exact owner")
		}
		select {
		case <-ownerObservation.Done():
			return fmt.Errorf("requested wake owner observation ended before reload transport startup")
		default:
		}
		stopReloadTransport, startErr := startWakeReloadTransportForWake(
			activeAgentDir,
			root,
			me,
			currentWake,
			*requestedOwner,
		)
		if startErr != nil {
			var unavailable *wakeReloadTransportUnavailableError
			if !errors.As(startErr, &unavailable) {
				return startErr
			}
			_ = writeStderr(
				"amq wake: reload transport unavailable: %v; continuing without reload transport\n",
				startErr,
			)
		} else {
			if stopReloadTransport == nil {
				return fmt.Errorf("wake reload transport returned no cleanup")
			}
			defer stopReloadTransport()
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

func snapshotWakeExistingMessagesForConfig(
	cfg *wakeConfig,
) (map[string]wakeFileIdentity, error) {
	if retained, ok := cfg.retainedInbox.(*wakeInboxDir); ok {
		return retained.SnapshotMessageIdentities()
	}
	return snapshotWakeExistingMessages(cfg.root, cfg.me)
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
	retained, retainedBaseline := cfg.retainedInbox.(*wakeInboxDir)
	if retainedBaseline {
		if err := retained.ValidateCanonical(); err != nil {
			return fmt.Errorf("validate retained wake inbox before baseline snapshot: %w", err)
		}
	}
	// Individual local-filesystem calls are intentionally not cancellable. Coop
	// has an outer readiness timeout; standalone wake can wait on a stuck scan.
	baseline, err := snapshotWakeExistingMessagesForConfig(cfg)
	if err != nil {
		return fmt.Errorf("snapshot existing wake messages: %w", err)
	}
	if retainedBaseline {
		if err := retained.ValidateCanonical(); err != nil {
			return fmt.Errorf("validate retained wake inbox after baseline snapshot: %w", err)
		}
	}
	cfg.baselineExisting = baseline

	var markerName string
	if retainedBaseline {
		markerName, err = retained.CreateBaselineBarrier()
		if err != nil {
			return err
		}
		defer func() { _ = retained.UnlinkBaselineBarrier(markerName) }()
		if err := retained.ValidateCanonical(); err != nil {
			return fmt.Errorf("validate retained wake inbox after baseline barrier: %w", err)
		}
	} else {
		marker, createErr := os.CreateTemp(inboxNew, ".wake-baseline-barrier-")
		if createErr != nil {
			return fmt.Errorf("create wake baseline barrier: %w", createErr)
		}
		markerPath := marker.Name()
		markerName = filepath.Base(markerPath)
		if err := marker.Close(); err != nil {
			_ = os.Remove(markerPath)
			return fmt.Errorf("close wake baseline barrier: %w", err)
		}
		// A crash can leave this hidden marker behind; message scans ignore it.
		defer func() { _ = os.Remove(markerPath) }()
	}

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
			if filepath.Base(event.Name) == markerName &&
				event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
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
			if retainedBaseline {
				if err := retained.ValidateCanonical(); err != nil {
					return fmt.Errorf("validate retained wake inbox before baseline completion: %w", err)
				}
			}
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

type wakeFailureDisposition uint8

const (
	// Unknown failures retry by default. An admitted wake may abandon its
	// durable inbox obligation only after proven ownership transfer.
	wakeFailureRetry wakeFailureDisposition = iota
	wakeFailureDegrade
	wakeFailureFatal
)

type wakeOwnershipLossError struct {
	reason string
}

type wakeUnreadableGenerationNoticeState struct {
	consecutiveFailures uint
	statusActive        bool
	attentionDelivered  bool
}

const wakeUnreadableGenerationNoticeThreshold = 5

func (state *wakeUnreadableGenerationNoticeState) resetWithoutStatusWrite() {
	if state == nil {
		return
	}
	*state = wakeUnreadableGenerationNoticeState{}
}

func (err *wakeOwnershipLossError) Error() string {
	return err.reason
}

func newWakeOwnershipLoss(reason string) error {
	return &wakeOwnershipLossError{reason: reason}
}

func classifyWakeFailure(err error) wakeFailureDisposition {
	if err == nil {
		return wakeFailureRetry
	}
	var ownershipLoss *wakeOwnershipLossError
	if errors.As(err, &ownershipLoss) ||
		(isWakeTerminalAuthorityLoss(err) &&
			!isWakeTerminalForegroundPGRPChanged(err) &&
			!isWakeTerminalControlStopped(err)) {
		return wakeFailureFatal
	}
	var unsupported *wakeInjectorUnsupportedError
	if isWakeInputDemotionBlocked(err) ||
		isWakeTerminalProgressUncertain(err) ||
		errors.As(err, &unsupported) {
		return wakeFailureDegrade
	}
	return wakeFailureRetry
}

func (state *wakeUnreadableGenerationNoticeState) observe(
	cfg *wakeConfig,
) {
	if state == nil || cfg == nil {
		return
	}
	if cfg.inspectTerminalGeneration == nil ||
		cfg.injectMode == wakeInjectModeNone ||
		cfg.inputRecoveryRequired {
		state.resetWithoutStatusWrite()
		return
	}
	inspection := cfg.inspectTerminalGeneration()
	if inspection.Exists && inspection.fileInfo == nil {
		state.consecutiveFailures++
		// This threshold counts the loop's fixed maintenance observations. It
		// gates operator visibility only; delivery authorization and retry
		// scheduling remain owned by the per-write validation path.
		if state.consecutiveFailures < wakeUnreadableGenerationNoticeThreshold {
			return
		}
		if !state.statusActive {
			if err := persistWakeNotifierStatus(
				cfg,
				"degraded",
				effectiveInjectMode(cfg),
				"wake lock unreadable",
			); err != nil {
				_ = writeWakeDiagnostic(
					cfg,
					"amq wake: record unreadable wake-lock status: %v; retrying\n",
					err,
				)
			} else {
				state.statusActive = true
			}
		}
		if state.attentionDelivered {
			return
		}
		if err := emitWakeAttention(cfg, wakePayload{
			text:       "wake lock unreadable; injection paused; will resume automatically",
			provenance: wakePayloadSystemFixed,
		}); err != nil {
			_ = writeWakeDiagnostic(
				cfg,
				"amq wake: emit unreadable wake-lock attention: %v; retrying\n",
				err,
			)
			return
		}
		state.attentionDelivered = true
		return
	}

	if !inspection.Exists {
		state.resetWithoutStatusWrite()
		return
	}
	state.consecutiveFailures = 0
	state.attentionDelivered = false
	if !state.statusActive {
		return
	}
	if err := persistWakeNotifierStatus(
		cfg,
		"",
		effectiveInjectMode(cfg),
		"",
	); err != nil {
		_ = writeWakeDiagnostic(
			cfg,
			"amq wake: clear recovered wake-lock status: %v; retrying\n",
			err,
		)
	} else {
		state.statusActive = false
	}
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
		return "", fmt.Errorf("invalid interrupt-cmd %q (use ctrl-c or none)", raw)
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

var waitWakeRetry = func(
	controlStop <-chan struct{},
	signals <-chan os.Signal,
	delay time.Duration,
) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-controlStop:
		return false
	case <-signals:
		return false
	case <-timer.C:
		return true
	}
}

func runWakeLoop(cfg wakeConfig) error {
	// Register shutdown handling before any watcher setup, baseline work, or
	// readiness callback so an early parent death cannot be lost.
	signal.Ignore(syscall.SIGTTOU, syscall.SIGTSTP, syscall.SIGTTIN)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	select {
	case <-cfg.controlStop:
		return nil
	case <-sigCh:
		return nil
	default:
	}

	inboxNew := fsq.AgentInboxNew(cfg.root, cfg.me)

	ordinaryRecoverable := cfg.retainedInbox == nil
	var ordinaryAgentDir *wakeAgentDir
	ownsOrdinaryAgentDir := false
	if retained, ok := cfg.retainedAgent.(*wakeAgentDir); ok {
		ordinaryAgentDir = retained
	}
	var ordinaryInboxDir *wakeInboxDir
	var startupBaselineWatcher wakeEventWatcher
	var watcher wakeEventWatcher
	defer func() {
		if startupBaselineWatcher != nil {
			_ = startupBaselineWatcher.Close()
		}
		if watcher != nil {
			_ = watcher.Close()
		}
		_ = ordinaryInboxDir.Close()
		if ownsOrdinaryAgentDir && ordinaryAgentDir != nil {
			_ = ordinaryAgentDir.Close()
		}
	}()
	if ordinaryRecoverable {
		if ordinaryAgentDir == nil {
			// Direct loop tests and legacy in-process callers do not acquire the
			// wake lock first. Provision only for those unowned invocations.
			if err := os.MkdirAll(inboxNew, 0o700); err != nil {
				return err
			}
			var err error
			ordinaryAgentDir, err = openWakeAgentDir(cfg.root, cfg.me)
			if err != nil {
				return err
			}
			ownsOrdinaryAgentDir = true
		} else if err := validateCanonicalWakeAgentDir(ordinaryAgentDir); err != nil {
			return fmt.Errorf("validate acquired wake agent authority: %w", err)
		}
		if cfg.touchPresence == nil {
			cfg.touchPresence = func() error {
				return touchWakePresenceInDir(ordinaryAgentDir, cfg.me)
			}
		}
	} else if _, ok := cfg.retainedInbox.(*wakeInboxDir); !ok {
		if err := os.MkdirAll(inboxNew, 0o700); err != nil {
			return err
		}
	}

	openMainWatcher := func() error {
		if ordinaryRecoverable {
			inboxDir, err := openWakeRepairInboxDir(ordinaryAgentDir)
			if err != nil {
				if validateErr := validateCanonicalWakeAgentDir(ordinaryAgentDir); validateErr != nil {
					return errors.Join(
						fmt.Errorf("open acquired wake inbox capability: %w", err),
						fmt.Errorf(
							"validate acquired wake agent authority after inbox failure: %w",
							validateErr,
						),
					)
				}
				return fmt.Errorf(
					"open acquired wake inbox capability: %w",
					err,
				)
			}
			nextWatcher, err := newWakeInboxEventWatcher(inboxDir)
			if err != nil {
				return errors.Join(err, inboxDir.Close())
			}
			ordinaryInboxDir = inboxDir
			watcher = nextWatcher
			cfg.retainedInbox = inboxDir
			return nil
		}
		if retained, ok := cfg.retainedInbox.(*wakeInboxDir); ok {
			nextWatcher, err := newWakeInboxEventWatcher(retained)
			if err != nil {
				if validateErr := retained.ValidateCanonical(); validateErr != nil {
					return errors.Join(
						err,
						fmt.Errorf(
							"validate retained wake inbox authority after watcher failure: %w",
							validateErr,
						),
					)
				}
				return err
			}
			watcher = nextWatcher
			return nil
		}

		nextWatcher, err := newWakePathEventWatcher(inboxNew)
		if err == nil {
			watcher = nextWatcher
		}
		return err
	}
	armMainWatcher := func() (bool, error) {
		for failures := uint(1); ; failures++ {
			err := openMainWatcher()
			if err == nil {
				return true, nil
			}
			if classifyWakeFailure(err) == wakeFailureFatal {
				return false, err
			}
			delay := wakeStartupRetryBackoff(failures)
			_ = writeWakeDiagnostic(
				&cfg,
				"amq wake: create wake inbox watcher: %v; retrying in %s\n",
				err,
				delay,
			)
			if !waitWakeRetry(cfg.controlStop, sigCh, delay) {
				return false, nil
			}
		}
	}
	armed, err := armMainWatcher()
	if err != nil {
		return err
	}
	if !armed {
		return nil
	}
	if cfg.baselineRequested && !cfg.baselineInherited {
		retained, ok := cfg.retainedInbox.(*wakeInboxDir)
		if !ok {
			return fmt.Errorf("wake baseline requires a retained inbox capability")
		}
		var failures uint
		historyUncertain := false
		for {
			var prepareErr error
			startupBaselineWatcher, prepareErr = newWakeBaselineEventWatcher(retained.path)
			if prepareErr == nil {
				prepareErr = retained.ValidateCanonical()
			}
			if prepareErr == nil {
				// The startup boundary is watcher installation, not lock
				// acquisition; messages delivered in between are intentionally
				// treated as startup backlog.
				prepareErr = prepareWakeBaselineEvents(
					&cfg,
					startupBaselineWatcher.Events(),
					startupBaselineWatcher.Errors(),
					inboxNew,
				)
			}
			if startupBaselineWatcher != nil {
				closeErr := startupBaselineWatcher.Close()
				startupBaselineWatcher = nil
				if prepareErr == nil && closeErr != nil {
					_ = writeWakeDiagnostic(
						&cfg,
						"amq wake: close startup baseline watcher: %v; continuing\n",
						closeErr,
					)
				}
			}
			if prepareErr == nil {
				if historyUncertain {
					// The original watcher lost event history. Reaching a new
					// barrier proves readiness, but treating all backlog as new
					// is the only lossless delivery fallback.
					cfg.baselineExisting = nil
				}
				break
			}
			cfg.baselineExisting = nil
			if err := retained.ValidateCanonical(); err != nil {
				validationErr := fmt.Errorf(
					"validate startup baseline watcher authority: %w",
					err,
				)
				if classifyWakeFailure(validationErr) == wakeFailureFatal {
					return validationErr
				}
				prepareErr = errors.Join(prepareErr, validationErr)
			}
			historyUncertain = true
			failures++
			delay := wakeStartupRetryBackoff(failures)
			_ = writeWakeDiagnostic(
				&cfg,
				"amq wake: startup baseline watcher failed: %v; retrying in %s\n",
				prepareErr,
				delay,
			)
			if !waitWakeRetry(cfg.controlStop, sigCh, delay) {
				return nil
			}
		}
	} else if err := prepareWakeBaselineEvents(
		&cfg,
		watcher.Events(),
		watcher.Errors(),
		inboxNew,
	); err != nil {
		return err
	}
	// A separate baseline watcher can take long enough for the main watcher to
	// fail before admission. Rearm it and discard startup tombstones so messages
	// from the uncertain interval are delivered instead of publishing false
	// readiness with an already-dead watcher.
	for {
		if err := pendingWakeWatcherError(watcher); err == nil {
			break
		} else {
			cfg.baselineExisting = nil
			_ = watcher.Close()
			watcher = nil
			if ordinaryRecoverable {
				_ = ordinaryInboxDir.Close()
				ordinaryInboxDir = nil
				cfg.retainedInbox = nil
			}
			_ = writeWakeDiagnostic(
				&cfg,
				"amq wake: main watcher failed before admission: %v; rearming\n",
				err,
			)
		}
		armed, err := armMainWatcher()
		if err != nil {
			return err
		}
		if !armed {
			return nil
		}
	}
	if retained, ok := cfg.retainedInbox.(*wakeInboxDir); ok {
		if err := retained.ValidateCanonical(); err != nil {
			return fmt.Errorf("validate wake inbox before admission publication: %w", err)
		}
	}
	if ordinaryAgentDir != nil {
		if err := validateCanonicalWakeAgentDir(ordinaryAgentDir); err != nil {
			return fmt.Errorf("validate wake agent before admission publication: %w", err)
		}
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
	case <-sigCh:
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
	case <-sigCh:
		return nil
	default:
	}

	// Debounce timer
	var debounceTimer *time.Timer
	pendingNotify := false
	var terminalAuthorityRetryTimer *time.Timer
	var terminalAuthorityRetryC <-chan time.Time
	var inboxScanRetryTimer *time.Timer
	var inboxScanRetryC <-chan time.Time
	var inboxScanFailures uint
	var doorbellTimer *time.Timer
	var doorbellTimerC <-chan time.Time
	defer func() {
		if terminalAuthorityRetryTimer != nil {
			terminalAuthorityRetryTimer.Stop()
		}
		if inboxScanRetryTimer != nil {
			inboxScanRetryTimer.Stop()
		}
		if doorbellTimer != nil {
			doorbellTimer.Stop()
		}
	}()

	clearTerminalAuthorityRetry := func() {
		terminalAuthorityRetryC = nil
		if terminalAuthorityRetryTimer == nil {
			return
		}
		if !terminalAuthorityRetryTimer.Stop() {
			select {
			case <-terminalAuthorityRetryTimer.C:
			default:
			}
		}
	}
	scheduleTerminalAuthorityRetry := func() {
		if terminalAuthorityRetryTimer == nil {
			terminalAuthorityRetryTimer = time.NewTimer(wakeTerminalAuthorityRetryDelay)
		} else {
			if !terminalAuthorityRetryTimer.Stop() {
				select {
				case <-terminalAuthorityRetryTimer.C:
				default:
				}
			}
			terminalAuthorityRetryTimer.Reset(wakeTerminalAuthorityRetryDelay)
		}
		terminalAuthorityRetryC = terminalAuthorityRetryTimer.C
	}
	clearInboxScanRetry := func() {
		inboxScanRetryC = nil
		if inboxScanRetryTimer == nil {
			return
		}
		if !inboxScanRetryTimer.Stop() {
			select {
			case <-inboxScanRetryTimer.C:
			default:
			}
		}
	}
	scheduleInboxScanRetry := func(delay time.Duration) {
		if inboxScanRetryTimer == nil {
			inboxScanRetryTimer = time.NewTimer(delay)
		} else {
			if !inboxScanRetryTimer.Stop() {
				select {
				case <-inboxScanRetryTimer.C:
				default:
				}
			}
			inboxScanRetryTimer.Reset(delay)
		}
		inboxScanRetryC = inboxScanRetryTimer.C
	}
	clearDoorbellDeadline := func() {
		doorbellTimerC = nil
		if doorbellTimer != nil && !doorbellTimer.Stop() {
			select {
			case <-doorbellTimer.C:
			default:
			}
		}
	}
	scheduleDoorbellDeadline := func() {
		if terminalAuthorityRetryC != nil ||
			inboxScanRetryC != nil {
			clearDoorbellDeadline()
			return
		}
		deadline, ok := cfg.doorbell.nextDeadline()
		if !ok {
			clearDoorbellDeadline()
			return
		}
		delay := deadline.Sub(cfg.wakeDoorbellNow())
		if delay < 0 {
			delay = 0
		}
		if doorbellTimer == nil {
			doorbellTimer = time.NewTimer(delay)
		} else {
			if !doorbellTimer.Stop() {
				select {
				case <-doorbellTimer.C:
				default:
				}
			}
			doorbellTimer.Reset(delay)
		}
		doorbellTimerC = doorbellTimer.C
	}
	retryWatcher := func(context string, cause error) {
		cfg.baselineExisting = nil
		if watcher != nil {
			_ = watcher.Close()
			watcher = nil
		}
		if ordinaryRecoverable {
			_ = ordinaryInboxDir.Close()
			ordinaryInboxDir = nil
			cfg.retainedInbox = nil
		}
		pendingNotify = true
		clearTerminalAuthorityRetry()
		inboxScanFailures++
		scheduleInboxScanRetry(wakeInboxScanRetryBackoff(inboxScanFailures))
		clearDoorbellDeadline()
		if cause == nil {
			_ = writeWakeDiagnostic(&cfg, "amq wake: %s; retrying watcher setup\n", context)
			return
		}
		_ = writeWakeDiagnostic(&cfg, "amq wake: %s: %v; retrying watcher setup\n", context, cause)
	}
	scheduleWatcherRebindFailure := func(context string, cause error) {
		pendingNotify = true
		inboxScanFailures++
		scheduleInboxScanRetry(wakeInboxScanRetryBackoff(inboxScanFailures))
		clearDoorbellDeadline()
		_ = writeWakeDiagnostic(
			&cfg,
			"amq wake: %s: %v; retrying watcher setup\n",
			context,
			cause,
		)
	}
	rebindWatcher := func() (bool, error) {
		if watcher != nil {
			return true, nil
		}
		if ordinaryRecoverable {
			if err := validateCanonicalWakeAgentDir(ordinaryAgentDir); err != nil {
				return false, failWakeOnWatcherError(
					&cfg,
					"ordinary wake agent authority changed while rearming watcher",
					err,
				)
			}
			inboxDir, nextWatcher, err := openWatchedWakeInboxDir(ordinaryAgentDir)
			if err != nil {
				scheduleWatcherRebindFailure(
					"ordinary wake inbox is unavailable while rearming watcher",
					err,
				)
				return false, nil
			}
			ordinaryInboxDir = inboxDir
			watcher = nextWatcher
			cfg.retainedInbox = inboxDir
			return true, nil
		}

		if retained, ok := cfg.retainedInbox.(*wakeInboxDir); ok {
			if err := retained.ValidateCanonical(); err != nil {
				return false, failWakeOnWatcherError(
					&cfg,
					"retained wake inbox authority changed while rearming watcher",
					err,
				)
			}
			nextWatcher, err := newWakeInboxEventWatcher(retained)
			if err != nil {
				scheduleWatcherRebindFailure(
					"retained wake inbox watcher is unavailable while rearming",
					err,
				)
				return false, nil
			}
			watcher = nextWatcher
			return true, nil
		}

		nextWatcher, err := newWakePathEventWatcher(inboxNew)
		if err != nil {
			scheduleWatcherRebindFailure(
				"wake inbox watcher is unavailable while rearming",
				err,
			)
			return false, nil
		}
		watcher = nextWatcher
		return true, nil
	}
	enterRetainedInputRecovery := func(cause error) error {
		markWakeInputRecoveryRequired(&cfg, cause)
		cfg.doorbell.retainRecoveryRequired(cfg.wakeDoorbellNow())
		pendingNotify = false
		clearTerminalAuthorityRetry()
		if err := emitWakeAttention(&cfg, wakePayload{
			text:       wakeInputRecoveryNotice,
			provenance: wakePayloadSystemFixed,
		}); err == nil {
			cfg.doorbell.recordRecoveryAttentionDelivered()
		}
		scheduleDoorbellDeadline()
		return nil
	}
	var unreadableGenerationNotice wakeUnreadableGenerationNoticeState
	attemptNotification := func() error {
		if terminalAuthorityRetryC != nil || inboxScanRetryC != nil {
			return nil
		}
		err := notifyNewMessages(&cfg)
		var scanErr *wakeInboxScanError
		if errors.As(err, &scanErr) {
			pendingNotify = true
			clearTerminalAuthorityRetry()
			inboxScanFailures++
			scheduleInboxScanRetry(wakeInboxScanRetryBackoff(inboxScanFailures))
			clearDoorbellDeadline()
			_ = writeWakeDiagnostic(&cfg, "amq wake: notify error: %v\n", err)
			return nil
		}
		clearInboxScanRetry()
		inboxScanFailures = 0
		disposition := classifyWakeFailure(err)
		if disposition == wakeFailureFatal {
			return err
		}
		if isWakeTerminalForegroundPGRPChanged(err) {
			pendingNotify = true
			scheduleTerminalAuthorityRetry()
			clearDoorbellDeadline()
			if cfg.debug {
				_ = writeWakeDiagnostic(
					&cfg,
					"amq wake [debug]: holding notification until foreground process group is restored: %v\n",
					err,
				)
			}
			return nil
		}
		if isWakeTerminalPartialProgress(err) {
			pendingNotify = true
			scheduleTerminalAuthorityRetry()
			clearDoorbellDeadline()
			if cfg.debug {
				_ = writeWakeDiagnostic(
					&cfg,
					"amq wake [debug]: holding partial terminal delivery for exact suffix retry: %v\n",
					err,
				)
			}
			return nil
		}
		var attentionErr *wakeAttentionDeliveryError
		if errors.As(err, &attentionErr) &&
			!isWakeTerminalAuthorityLoss(err) {
			pendingNotify = false
			clearTerminalAuthorityRetry()
			scheduleDoorbellDeadline()
			return nil
		}
		if disposition == wakeFailureDegrade {
			return enterRetainedInputRecovery(err)
		}
		pendingNotify = false
		clearTerminalAuthorityRetry()
		scheduleDoorbellDeadline()
		if err == nil {
			return nil
		}
		_ = writeWakeDiagnostic(&cfg, "amq wake: notify error: %v\n", err)
		return nil
	}
	emitScanFallback := func() error {
		if err := emitWakeAttention(&cfg, wakePayload{
			text:       "AMQ messages may be pending; run amq drain --include-body",
			provenance: wakePayloadSystemFixed,
		}); err != nil {
			return err
		}
		pendingNotify = false
		clearInboxScanRetry()
		return nil
	}

	// Recheck non-invasive injection preconditions and silently reconcile
	// durable pending work every 30s. Independent doorbell deadlines keep
	// re-notifying unread cohorts, with separate input and attention cadences.
	maintenanceTicks := cfg.maintenanceTicks
	var maintenanceTicker *time.Ticker
	if maintenanceTicks == nil {
		maintenanceTicker = time.NewTicker(30 * time.Second)
		maintenanceTicks = maintenanceTicker.C
		defer maintenanceTicker.Stop()
	}
	maintenanceOutputs := cfg.maintenanceOutputs
	if maintenanceOutputs == nil {
		maintenanceOutputs = []*os.File{os.Stdout, os.Stderr}
	}
	preconditionCheck := cfg.preconditionCheck
	if preconditionCheck == nil {
		preconditionCheck = func(cfg *wakeConfig) error {
			return wakeInjectionPreconditionCheck(cfg, controllingTerminalOpenable)
		}
	}

	// Touch presence immediately so `amq who` shows agent as active
	if cfg.touchPresence != nil {
		_ = cfg.touchPresence()
	} else {
		_ = presence.Touch(cfg.root, cfg.me)
	}

	// Notify if messages already exist
	if err := attemptNotification(); err != nil {
		return err
	}

	for {
		var debounceC <-chan time.Time
		if debounceTimer != nil {
			debounceC = debounceTimer.C
		}
		var watcherEvents <-chan fsnotify.Event
		var watcherErrors <-chan error
		if watcher != nil {
			watcherEvents = watcher.Events()
			watcherErrors = watcher.Errors()
		}

		select {
		case <-cfg.controlStop:
			return nil
		case <-sigCh:
			// Clean exit on SIGHUP/SIGTERM
			return nil

		case event, ok := <-watcherEvents:
			if !ok {
				retryWatcher("wake inbox watcher closed", nil)
				continue
			}
			invalidateWakeBaselineEvent(&cfg, event)
			// Creates/additions can accelerate delivery; removals are durable
			// progress and must rearm the generation without waiting for the
			// maintenance reconciliation fallback.
			if event.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write|fsnotify.Remove) == 0 {
				continue
			}
			// Skip non-.md files
			if !strings.HasSuffix(event.Name, ".md") {
				continue
			}

			// Start or reset debounce timer
			pendingNotify = true
			if cfg.onPendingNotify != nil {
				cfg.onPendingNotify()
			}
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

		case err, ok := <-watcherErrors:
			if !ok {
				retryWatcher("wake inbox watcher closed", nil)
				continue
			}
			retryWatcher("wake inbox watcher failed", err)
			continue

		case <-debounceC:
			if !pendingNotify {
				continue
			}

			// Collect and notify
			if err := attemptNotification(); err != nil {
				return err
			}

		case <-terminalAuthorityRetryC:
			terminalAuthorityRetryC = nil
			if !pendingNotify {
				continue
			}
			if err := attemptNotification(); err != nil {
				return err
			}

		case <-inboxScanRetryC:
			inboxScanRetryC = nil
			rebound, err := rebindWatcher()
			if err != nil {
				if classifyWakeFailure(err) == wakeFailureFatal {
					return err
				}
				scheduleWatcherRebindFailure(
					"wake inbox watcher rearm failed",
					err,
				)
				continue
			}
			if !rebound {
				continue
			}
			if err := attemptNotification(); err != nil {
				return err
			}

		case <-doorbellTimerC:
			doorbellTimerC = nil
			if err := attemptNotification(); err != nil {
				return err
			}

		case <-maintenanceTicks:
			if err := maintainWakeOutputBounds(maintenanceOutputs...); err != nil {
				_ = writeWakeDiagnostic(
					&cfg,
					"amq wake: maintain output bound: %v; continuing without truncation\n",
					err,
				)
			}
			pendingInputWork := !cfg.inputRecoveryRequired &&
				(cfg.doorbell.pendingInput() || cfg.inputDelivery.pending())
			hadPendingInputWork := pendingNotify || pendingInputWork
			if pendingInputWork {
				if err := attemptNotification(); err != nil {
					return err
				}
			}

			// Keep presence alive so `amq who` reports the agent as active
			if cfg.touchPresence != nil {
				_ = cfg.touchPresence()
			} else {
				_ = presence.Touch(cfg.root, cfg.me)
			}
			if err := retryPendingWakeNotifierStatus(&cfg); err != nil {
				_ = writeWakeDiagnostic(
					&cfg,
					"amq wake: persist notifier status: %v; retrying\n",
					err,
				)
			}
			if cfg.inputRecoveryRequired {
				unreadableGenerationNotice.resetWithoutStatusWrite()
				scheduleDoorbellDeadline()
				continue
			}

			inputModeBeforeCheck := cfg.injectMode
			inputEnabledBeforeCheck := inputModeBeforeCheck != wakeInjectModeNone
			inputStateBeforeCheck := cfg.inputDelivery
			doorbellBeforeCheck := cfg.doorbell
			preconditionErr := preconditionCheck(&cfg)
			if inputEnabledBeforeCheck &&
				cfg.injectMode == wakeInjectModeNone &&
				inputStateBeforeCheck.blocksDemotion() {
				cfg.injectMode = inputModeBeforeCheck
				cfg.inputDelivery = inputStateBeforeCheck
				cfg.doorbell = doorbellBeforeCheck
				preconditionErr = errors.Join(
					preconditionErr,
					&wakeInputDemotionBlockedError{
						err: errors.New("maintenance attempted output-only demotion before terminal input completed"),
					},
				)
			}
			if isWakeInputDemotionBlocked(preconditionErr) {
				unreadableGenerationNotice.resetWithoutStatusWrite()
				if err := enterRetainedInputRecovery(preconditionErr); err != nil {
					return err
				}
				continue
			}
			if !inputEnabledBeforeCheck && cfg.injectMode != wakeInjectModeNone {
				cfg.doorbell.makeDue(cfg.wakeDoorbellNow())
				pendingNotify = true
				if err := attemptNotification(); err != nil {
					return err
				}
			}
			if cfg.injectMode == wakeInjectModeNone {
				if inputEnabledBeforeCheck {
					if err := retireWakeInputState(
						&cfg,
						errors.New("maintenance transferred pending work to output-only delivery"),
					); err != nil {
						cfg.injectMode = inputModeBeforeCheck
						preconditionErr = errors.Join(preconditionErr, err)
						scheduleDoorbellDeadline()
						return preconditionErr
					}
				} else {
					clearWakeInputState(&cfg)
				}
				clearTerminalAuthorityRetry()
				if inputEnabledBeforeCheck && hadPendingInputWork {
					pendingNotify = true
					if preconditionErr != nil && inboxScanRetryC != nil {
						preconditionErr = errors.Join(
							preconditionErr,
							emitScanFallback(),
						)
					} else {
						if err := attemptNotification(); err != nil {
							return err
						}
						if preconditionErr != nil && pendingNotify && inboxScanRetryC != nil {
							preconditionErr = errors.Join(
								preconditionErr,
								emitScanFallback(),
							)
						}
					}
				} else if inputEnabledBeforeCheck {
					pendingNotify = false
				}
			}
			scheduleDoorbellDeadline()
			if preconditionErr != nil {
				if classifyWakeFailure(preconditionErr) == wakeFailureFatal {
					return preconditionErr
				}
				_ = writeWakeDiagnostic(
					&cfg,
					"amq wake: maintenance precondition failed: %v; retrying\n",
					preconditionErr,
				)
			}
			unreadableGenerationNotice.observe(&cfg)
		}
	}
}

func wakeInboxScanRetryBackoff(failures uint) time.Duration {
	return cappedExponentialBackoff(failures, wakeInboxScanRetryBase, wakeInboxScanRetryMax)
}

func wakeStartupRetryBackoff(failures uint) time.Duration {
	// Keep several retry opportunities inside the coop parent's readiness
	// budget. Deriving the ceiling prevents startup backoff from drifting past
	// the timeout that supervises this child.
	const readinessSlices = 10
	return cappedExponentialBackoff(
		failures,
		wakeInboxScanRetryBase,
		wakeReadyTimeout/readinessSlices,
	)
}

func wakeInjectionPreconditionCheck(
	cfg *wakeConfig,
	controllingTerminalOpenableFn func() bool,
) error {
	if cfg.injectMode == wakeInjectModeNone {
		if !cfg.legacyTIOCSTIDemoted ||
			cfg.requestedInjectMode == "" ||
			cfg.requestedInjectMode == wakeInjectModeNone ||
			tiocstiLegacyDisabledHint() {
			return nil
		}
		if !controllingTerminalOpenableFn() {
			return errors.New("controlling terminal is not yet openable while restoring TIOCSTI input")
		}
		cfg.injectMode = cfg.requestedInjectMode
		cfg.legacyTIOCSTIDemoted = false
		cfg.fallbackWarn = true
		if err := persistWakeNotifierStatus(
			cfg,
			"",
			effectiveInjectMode(cfg),
			"",
		); err != nil {
			return fmt.Errorf("clear restored injector status: %w", err)
		}
		return nil
	}
	if cfg.injectVia != "" {
		if cfg.wakeOwner != nil {
			if err := wakeOwnerHealthCheck(*cfg.wakeOwner); err != nil {
				return err
			}
		}
		return nil
	}

	// A running TIOCSTI wake was not conclusively disabled at bind time.
	// Re-read the same #302 advisory capability signal so a later transition
	// to disabled is surfaced and safely demoted without issuing an ioctl.
	if tiocstiLegacyDisabledHint() {
		mode := effectiveInjectMode(cfg)
		capabilityErr := fmt.Errorf(
			"%s is 0 (observed after wake binding)",
			tiocstiLegacySysctlPath,
		)
		reason := wakeInjectorUnsupportedReason(
			mode,
			capabilityErr,
		)
		demotionErr := disableWakeInputForLegacyTIOCSTI(
			cfg,
			newWakeInjectorUnsupportedError(capabilityErr),
		)
		cfg.fallbackWarn = false
		var statusErr error
		if err := persistWakeNotifierStatus(
			cfg,
			wakeInjectorUnsupportedStatus,
			mode,
			reason,
		); err != nil {
			statusErr = fmt.Errorf(
				"record changed injector capability after safety demotion: %w",
				err,
			)
		}
		_ = writeWakeDiagnostic(cfg, "amq wake: warning: injector capability changed since binding: %s\n", reason)
		return errors.Join(demotionErr, statusErr)
	}
	if !controllingTerminalOpenableFn() {
		return errors.New("controlling terminal is no longer openable; TIOCSTI injectability was not tested")
	}
	return nil
}

func observeLiveWakeOwner(owner wakeOwner, context string) (wakeOwnerObservation, error) {
	if err := validateAuthoritativeWakeOwner(owner); err != nil {
		return wakeOwnerObservation{}, fmt.Errorf("%s: %w", context, err)
	}
	observation, err := observeAuthoritativeWakeOwner(owner)
	if err != nil {
		closeErr := observation.Close()
		return wakeOwnerObservation{}, errors.Join(
			fmt.Errorf("%s: %w", context, err),
			closeErr,
		)
	}
	if observation.State != wakeOwnerSame {
		closeErr := observation.Close()
		return wakeOwnerObservation{}, errors.Join(
			fmt.Errorf(
				"%s: owner is %s: %s",
				context,
				observation.State,
				observation.Reason,
			),
			closeErr,
		)
	}
	if observation.Done() == nil {
		closeErr := observation.Close()
		return wakeOwnerObservation{}, errors.Join(
			fmt.Errorf("%s: live owner observation has no lifetime signal", context),
			closeErr,
		)
	}
	select {
	case <-observation.Done():
		closeErr := observation.Close()
		return wakeOwnerObservation{}, errors.Join(
			fmt.Errorf("%s: owner exited during observation", context),
			closeErr,
		)
	default:
	}
	return observation, nil
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
		return newWakeOwnershipLoss(
			fmt.Sprintf("inject-via wake owner pid %d is not running", owner.PID),
		)
	}
	if owner.ProcessStart != "" {
		if proc.StartToken == "" {
			if proc.InspectError != nil {
				return fmt.Errorf(
					"inject-via wake owner process start unavailable for pid %d: %w",
					owner.PID,
					proc.InspectError,
				)
			}
			return fmt.Errorf(
				"inject-via wake owner process start unavailable for pid %d",
				owner.PID,
			)
		}
		if proc.StartToken != owner.ProcessStart {
			return newWakeOwnershipLoss(
				fmt.Sprintf("inject-via wake owner process start changed for pid %d", owner.PID),
			)
		}
	}
	if owner.BootID != "" {
		switch compareWakeBootID(owner.BootID, proc) {
		case bootIDMismatch:
			return newWakeOwnershipLoss(
				fmt.Sprintf("inject-via wake owner boot id changed for pid %d", owner.PID),
			)
		case bootIDUnknown:
			if proc.InspectError != nil {
				return fmt.Errorf(
					"inject-via wake owner boot id unavailable or incomparable for pid %d: %w",
					owner.PID,
					proc.InspectError,
				)
			}
			return fmt.Errorf(
				"inject-via wake owner boot id unavailable or incomparable for pid %d",
				owner.PID,
			)
		}
	}
	if owner.SessionID != 0 {
		sid, err := getWakeProcessSID(owner.PID)
		if err != nil {
			return fmt.Errorf("inject-via wake owner session unavailable for pid %d: %w", owner.PID, err)
		}
		if sid != owner.SessionID {
			return newWakeOwnershipLoss(
				fmt.Sprintf("inject-via wake owner session changed for pid %d", owner.PID),
			)
		}
	}
	return nil
}

func controllingTerminalOpenable() bool {
	// This verifies only that /dev/tty can be opened. TIOCSTI injectability
	// cannot be established without an ioctl, and this periodic check never
	// injects.
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
	return currentTTYPath(tty)
}
