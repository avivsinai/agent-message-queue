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

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

var (
	wakeTerminateGrace       = 100 * time.Millisecond
	wakeTerminateKillConfirm = 3 * time.Second
	// wakeTerminateGracefulExitConfirm bounds how long a retire waits for the
	// exact signaled process to finish a graceful exit whose lock teardown
	// already outlived wakeTerminateGrace. It is a shutdown bound, not a
	// kill-reap bound, even though both are 3 s.
	wakeTerminateGracefulExitConfirm = 3 * time.Second
	wakeBaselineTimeout              = 5 * time.Second
	wakeBaselineSettle               = 50 * time.Millisecond
	wakeTerminalAuthorityRetryDelay  = 250 * time.Millisecond
	wakeInboxScanRetryBase           = 250 * time.Millisecond
	wakeInboxScanRetryMax            = 30 * time.Second
	getWakeProcessSID                = unix.Getsid
	wakeTIOCSTIAvailable             = func() bool { return tiocsti.Available() }
	wakeInputIsTTY                   = func() bool { return tiocsti.IsTTY() }
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
	debug                   bool
	requestedOwner          *wakeOwner
	repairLineage           *wakeRepairLineage
	repairFloorAuthority    *wakeRepairFloorAuthority
	// resumeEligible is startup policy, not authority. newWakeLock still requires
	// the complete owner/process/control/image advertisement below.
	resumeEligible      bool
	resumeImageEvidence *wakeImageEvidenceV1
}

func debugWakeNoLockShadowReplacementAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	kind string,
	enabled bool,
) error {
	if !enabled {
		return nil
	}
	const (
		absent      = "absent"
		unavailable = "unavailable"
	)
	targetDigest := absent
	target, targetExists, targetErr := readWakeTargetAt(dirfd, agentDir, root, me)
	if targetErr != nil {
		targetDigest = unavailable
	} else if targetExists {
		digest, err := wakeTargetDigest(target)
		if err != nil {
			targetDigest = unavailable
		} else {
			targetDigest = digest
		}
	}
	stateDigest := absent
	state, stateExists, stateErr := readWakeStateSnapshotAt(dirfd, agentDir)
	if stateErr != nil {
		stateDigest = unavailable
	} else if stateExists && state.State.Target.TargetDigest != "" {
		stateDigest = state.State.Target.TargetDigest
	} else if stateExists {
		stateDigest = unavailable
	}
	if !targetExists && !stateExists && targetErr == nil && stateErr == nil {
		return nil
	}
	return writeWakeDiagnostic(
		nil,
		"amq wake [debug]: %s publication superseding no-lock wake shadow target_digest=%s state_target_digest=%s\n",
		kind,
		targetDigest,
		stateDigest,
	)
}

type wakeLockCreatingError struct{}

func (err *wakeLockCreatingError) Error() string {
	return "wake lock is being created (retry shortly)"
}

func childRepairSource(lineage *wakeRepairLineage) wakeRepairHandoffSource {
	source := wakeRepairHandoffSource{
		schema:               wakeRepairHandoffSchema,
		root:                 lineage.source.Root,
		rootIdentity:         lineage.source.RootIdentity,
		agent:                lineage.source.Agent,
		sourceGeneration:     lineage.source.DeadGeneration,
		sourceTargetDigest:   lineage.source.SourceTargetDigest,
		sourceFloorDigest:    lineage.source.SourceFloorDigest,
		bootID:               lineage.source.BootID,
		agentDirDevice:       lineage.source.AgentDirDevice,
		agentDirInode:        lineage.source.AgentDirInode,
		inboxParentDirDevice: lineage.source.InboxParentDirDevice,
		inboxParentDirInode:  lineage.source.InboxParentDirInode,
		inboxDirDevice:       lineage.source.InboxDirDevice,
		inboxDirInode:        lineage.source.InboxDirInode,
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

func waitForWakeRepairChildExit(waiter *wakeProcessWaiter) error {
	return waiter.waitForExit(wakeProcessExitTimeout)
}

func supersedeUnverifiedGenericWakeAt(
	scope *wakeMutationScope,
	expected wakeLockInspection,
) error {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return err
	}
	current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
	if !sameWakeLockGeneration(expected, current) {
		return fmt.Errorf("unverified wake changed before supersession; retry")
	}
	if current.Status != wakeLockUnverified ||
		classifyPersistedWakeClaim(current) != wakeClaimGeneric {
		return fmt.Errorf(
			"wake state for %s is not an unverified ownerless generic claim; use %s for an owner-bound claim",
			expected.Agent,
			wakeRecoverOwnerCommand(expected.Root, expected.Agent),
		)
	}
	if err := removeWakeLockIfUnchangedGuardedAt(scope, current); err != nil {
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
		return nil, fmt.Errorf("owner-bearing wake state requires %s", wakeRecoverOwnerCommand(root, me))
	}
	if options.target != nil && options.target.Owner != nil {
		return acquireAuthoritativeWakeLockWithOptionsInDir(agentDir, root, me, options)
	}

	for {
		var replace wakeLockInspection
		var created wakeLockInspection
		quarantined := false
		err := withWakeMutationScopeInDir(agentDir, wakeGuardWait, func(scope *wakeMutationScope) error {
			dirfd, scopedAgentDir, err := scope.location()
			if err != nil {
				return err
			}
			agentDir = scopedAgentDir
			inspection := inspectWakeLockAt(dirfd, agentDir, root, me)
			if options.repairLineage != nil && inspection.Exists {
				return fmt.Errorf("wake lock changed before repair acquisition")
			}
			if inspection.Status == wakeLockUnverified && wakeLockHasOwnerMarkers(inspection) {
				return fmt.Errorf(
					"wake state for %s is unverified: %s; run %s",
					me,
					inspection.Reason,
					wakeRecoverOwnerCommand(root, me),
				)
			}
			if wakeLockEligibleForQuarantine(inspection, time.Now()) {
				if _, err := quarantineMalformedWakeLockAt(dirfd, agentDir, inspection); err != nil {
					return err
				}
				quarantined = true
				return nil
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
					return fmt.Errorf("wake handle %s has legacy owner-bearing state; run %s", me, wakeRecoverOwnerCommand(root, me))
				}
			}
			if inspection.Exists {
				replaceNeeded, settleErr := settleExistingGenericWakeLockAt(
					scope, dirfd, agentDir, me, inspection, options,
				)
				if settleErr != nil {
					return settleErr
				}
				if replaceNeeded {
					replace = inspection
					return nil
				}
			}
			if !inspection.Exists && options.target == nil && options.repairLineage == nil {
				orphan, exists, readErr := readWakeTargetSnapshotAt(dirfd, agentDir, root, me)
				// Parse errors retain an exact snapshot; quarantineWakeTargetAt independently revalidates inode and raw bytes.
				if readErr != nil && (orphan.FileInfo == nil || orphan.Raw == nil) {
					return fmt.Errorf("orphan wake target is unverified before targetless acquisition: %w", readErr)
				}
				if readErr == nil && exists && orphan.Target.Owner != nil {
					return fmt.Errorf("wake handle %s has an owner-bearing orphan target; run %s", me, wakeRecoverOwnerCommand(root, me))
				}
				if readErr != nil && malformedWakeTargetLooksOwnerShaped(orphan.Raw) {
					return fmt.Errorf("wake handle %s has an unverified owner-shaped orphan target; inspect the target because automatic owner recovery is unavailable", me)
				}
				if exists {
					if _, err := quarantineWakeTargetAt(dirfd, agentDir, orphan); err != nil {
						return err
					}
					quarantined = true
					return nil
				}
			}
			if orphan, exists, readErr := readWakeTargetAt(dirfd, agentDir, root, me); readErr != nil {
				return fmt.Errorf("wake target for %s is unverified: %w", me, readErr)
			} else if exists && orphan.Owner != nil {
				return fmt.Errorf("wake handle %s has an owner-bearing orphan target; run %s", me, wakeRecoverOwnerCommand(root, me))
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

			var createErr error
			created, createErr = createGenericWakeLockAt(scope, dirfd, agentDir, root, me, options)
			return createErr
		})
		if err != nil {
			return nil, err
		}
		if quarantined {
			continue
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
			if cleanupErr := withWakeMutationScopeInDir(agentDir, wakeGuardWait, func(scope *wakeMutationScope) error {
				return cleanupGenericWakeGenerationAt(
					scope,
					root,
					me,
					created,
					options,
				)
			}); cleanupErr != nil {
				_ = writeWakeLogLine("amq wake: cleanup failed: %v\n", cleanupErr)
			}
		}
		return cleanup, nil
	}
}

// createGenericWakeLockAt stages target metadata, then creates and verifies
// the generic lock inside the caller's mutation scope.
func createGenericWakeLockAt(
	scope *wakeMutationScope,
	dirfd int,
	agentDir *wakeAgentDir,
	root, me string,
	options wakeLockAcquireOptions,
) (wakeLockInspection, error) {
	// Stage target metadata first. The lock is the transaction commit point.
	if options.target != nil {
		if err := debugWakeNoLockShadowReplacementAt(
			dirfd, agentDir, root, me, "generic", options.debug,
		); err != nil {
			return wakeLockInspection{}, fmt.Errorf("write no-lock wake shadow diagnostic: %w", err)
		}
		if err := writeWakeTargetGuardedAt(scope, root, me, *options.target); err != nil {
			return wakeLockInspection{}, err
		}
	} else {
		// Revalidate canonical identity before the targetless
		// blind-unlink. A directory swap leaves the retained dirfd
		// pointing at the detached OLD directory; unlinking there
		// misses the canonical namespace. Refuse.
		if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
			return wakeLockInspection{}, err
		}
		_, targetExists, err := readWakeTargetAt(dirfd, agentDir, root, me)
		if err != nil {
			return wakeLockInspection{}, fmt.Errorf("orphan wake target is unverified before targetless acquisition: %w", err)
		}
		if targetExists {
			return wakeLockInspection{}, fmt.Errorf("orphan wake target is preserved because it may be an uncommitted bound-claim shadow")
		}
		if err := removeWakeTargetGuardedAt(scope); err != nil {
			return wakeLockInspection{}, err
		}
	}

	lock, err := newWakeLock(root, me, options)
	if err != nil {
		return wakeLockInspection{}, err
	}
	if options.target != nil {
		if err := publishWakeStateAndBindLockAt(scope, root, me, &lock); err != nil {
			return wakeLockInspection{}, fmt.Errorf("publish wake state before generic wake lock: %w", err)
		}
	}
	if options.repairLineage != nil {
		err = createWakeRepairLockAt(
			scope,
			root,
			me,
			options.repairLineage.source.RootIdentity,
			lock,
		)
	} else {
		err = createWakeLockAt(scope, root, me, lock)
	}
	if err != nil {
		return wakeLockInspection{}, err
	}
	created := inspectWakeLockAt(dirfd, agentDir, root, me)
	if !created.Exists || created.Lock.Generation != lock.Generation {
		return wakeLockInspection{}, fmt.Errorf("failed to verify created wake lock generation")
	}
	if options.target != nil {
		if err := validateWakeBoundStateAt(dirfd, agentDir, root, me, created.Lock); err != nil {
			return wakeLockInspection{}, fmt.Errorf("verify created generic wake binding: %w", err)
		}
	}
	return created, nil
}

// settleExistingGenericWakeLockAt clears a stale or unverified generic lock
// inside the caller's mutation scope, or refuses. It returns true when a valid
// lock must be terminated and replaced after the scope is released.
func settleExistingGenericWakeLockAt(
	scope *wakeMutationScope,
	dirfd int,
	agentDir *wakeAgentDir,
	me string,
	inspection wakeLockInspection,
	options wakeLockAcquireOptions,
) (bool, error) {
	switch inspection.Status {
	case wakeLockStale:
		if err := reconcileBoundWakePreparedProjectionAt(scope, inspection); err != nil {
			return false, err
		}
		if err := validateWakeLockStaleRemovalAt(dirfd, agentDir, inspection); err != nil {
			return false, err
		}
		if err := removeWakeLockIfUnchangedGuardedAt(scope, inspection); err != nil {
			return false, err
		}
	case wakeLockCreating:
		return false, &wakeLockCreatingError{}
	case wakeLockValid:
		if options.acceptExistingValid {
			if options.requestedOwner != nil &&
				(inspection.Lock.WakeMode != wakeOwnerWakeMode ||
					!sameWakeOwner(inspection.Lock.Owner, options.requestedOwner)) {
				return false, fmt.Errorf(
					"existing wake for %s is not bound to the requested exact owner; refusing readiness reuse",
					me,
				)
			}
			if err := requireWakeLockUsableAt(
				dirfd, agentDir, inspection, options.wakeMode, options.target,
			); err != nil {
				return false, err
			}
			return false, wakeLockAlreadyRunningError(me, inspection)
		}
		replaceNeeded, replaceErr := wakeLockReplacementNeededAt(
			dirfd, agentDir, inspection,
		)
		if replaceErr != nil {
			return false, replaceErr
		}
		if replaceNeeded {
			return true, nil
		}
		return false, wakeLockAlreadyRunningError(me, inspection)
	case wakeLockUnverified:
		if options.refuseUnverifiedGeneric {
			return false, fmt.Errorf(
				"wake lock for %s cannot be verified; lock: %s; root: %s; retry coop exec with -y",
				me,
				inspection.LockPath,
				inspection.Root,
			)
		}
		if err := supersedeUnverifiedGenericWakeAt(scope, inspection); err != nil {
			return false, err
		}
	}
	return false, nil
}

func cleanupGenericWakeGenerationAt(
	scope *wakeMutationScope,
	root string,
	me string,
	created wakeLockInspection,
	options wakeLockAcquireOptions,
) error {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return err
	}
	current := inspectWakeLockAt(dirfd, agentDir, root, me)
	if !sameWakeLockGeneration(created, current) || !currentWakeLockMatches(current.Lock) {
		return nil
	}
	if err := reconcileBoundWakePreparedProjectionAt(scope, current); err != nil {
		return err
	}

	preparedSnapshot, preparedSnapshotErr := freezeGenericWakePreparedCleanupAt(
		dirfd,
		agentDir,
		root,
		me,
		current,
	)
	removal := removeWakeLockIfUnchangedGuardedAtOutcome(
		scope,
		current,
		wakeLockRemoval{unlink: scope.unlinkWakeLockForCleanup},
	)
	committed, lockRemovalErr := removal.Committed, removal.Err
	blockingRemovalErr, diagnosticRemovalErr := splitWakeSelfUpgradeDiagnosticResidue(lockRemovalErr)
	if blockingRemovalErr != nil {
		return errors.Join(
			preparedSnapshotErr,
			fmt.Errorf("remove exact generic wake lock: %w", lockRemovalErr),
		)
	}
	if !committed {
		return preparedSnapshotErr
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
			scope,
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
		stateRefreshErr = reconcileWakeStateAfterLegacyMutationAt(scope, root, me)
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
		diagnosticRemovalErr,
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
	// Revalidate canonical identity before the blind floor removal. A
	// directory swap leaves the retained dirfd pointing at the detached OLD
	// directory; removing there misses the canonical namespace. Refuse.
	if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
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
	if options.resumeImageEvidence != nil {
		evidence := *options.resumeImageEvidence
		if err := validateWakeImageEvidence(evidence); err != nil {
			return wakeLock{}, fmt.Errorf("wake resume image evidence is invalid: %w", err)
		}
		lock.RunningImageEvidence = &evidence
		lock.ImagePath = evidence.ExecutionPath
		lock.ImageVersion = evidence.EmbeddedVersion
	} else if runtime.GOOS == "darwin" {
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
	lock.MachineID = currentWakeMachineID()
	if proc := inspectWakeProcess(os.Getpid()); proc.Running {
		lock.ProcessStart = proc.StartToken
		lock.BootID = proc.BootID
		lock.Executable = proc.Executable
		lock.Args = proc.Args
	}
	if options.resumeEligible && options.target == nil && options.requestedOwner != nil && options.repairLineage == nil {
		// Resume metadata is additive capability. Missing or inconsistent evidence
		// keeps the ordinary notifier usable without advertising agent-safe reload.
		candidate := lock
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
			configureWakeRestartAdvertisementPlatform(&candidate, root, me)
			candidate.RunningImageEvidence = evidence
			candidate.Args = append([]string(nil), os.Args...)
			candidate.ImagePath = evidence.ExecutionPath
			candidate.ImageVersion = evidence.EmbeddedVersion
			if validateWakeResumeAdvertisement(candidate, root, me) == nil {
				lock = candidate
			}
		}
	}
	return lock, nil
}

func wakeLockReplacementNeeded(inspection wakeLockInspection) (bool, error) {
	if err := validateWakeLockOwnerlessMutation(inspection); err != nil {
		return false, err
	}
	return wakeLockNeedsReplacement(inspection), nil
}

func wakeLockReplacementNeededAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (bool, error) {
	if err := validateWakeLockOwnerlessMutationAt(dirfd, agentDir, inspection); err != nil {
		return false, err
	}
	return wakeLockNeedsReplacement(inspection), nil
}

func validateWakeLockOwnerlessMutation(inspection wakeLockInspection) error {
	if _, err := readWakeStateSelectionForInspection(
		inspection.Root,
		inspection.Agent,
		inspection,
	); err != nil {
		return err
	}
	if err := validateGenericWakeLifecycleTransition(inspection, wakeGenericRequestMutate); err != nil {
		return err
	}
	if inspection.Lock.TargetDigest != "" {
		target, exists, err := readWakeTarget(inspection.Root, inspection.Agent)
		if err != nil {
			return fmt.Errorf("wake target is unverified before ownerless mutation: %w", err)
		}
		if exists && target.Owner != nil {
			return fmt.Errorf("owner-bearing wake state requires %s", wakeRecoverOwnerCommand(inspection.Root, inspection.Agent))
		}
	}
	return nil
}

func wakeLockNeedsReplacement(inspection wakeLockInspection) bool {
	if !inspection.IdentityConfirmed {
		return false
	}

	// Process is a confirmed matching amq wake. If its TTY disappeared, stop
	// that orphan before taking over; never signal an unconfirmed PID. A bound
	// external injector is the exception: supervisors deliberately detach it so
	// it can outlive a short launcher, and its target—not its TTY—is the usable
	// notification path. Session-sharing remains a separate replacement signal.
	if wakeLockTerminalGone(inspection) {
		return !wakeLockUsesBoundExternalInjector(inspection)
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
	currentTTY := getCurrentTTY()
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

func requireWakeLockUsableAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	requiredMode string,
	requestedTarget *wakeTarget,
) error {
	return requireWakeLockUsableWithTargetReader(
		inspection,
		requiredMode,
		requestedTarget,
		func() (wakeTarget, bool, error) {
			return readWakeTargetForRetainedInspectionAt(dirfd, agentDir, inspection)
		},
	)
}

func requireWakeLockUsableWithTargetReader(
	inspection wakeLockInspection,
	requiredMode string,
	requestedTarget *wakeTarget,
	readTarget func() (wakeTarget, bool, error),
) error {
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
		persistedTarget, exists, err := readTarget()
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
	if wakeLockUsesBoundExternalInjector(inspection) || wakeArgsUseInjectVia(inspection.Process.Args) {
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

func wakeLockUsesBoundExternalInjector(inspection wakeLockInspection) bool {
	return (inspection.Lock.WakeMode == wakeTargetInjectVia ||
		inspection.Lock.WakeMode == wakeOwnerWakeMode) &&
		inspection.Lock.TargetDigest != ""
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
	return processAliveByCapability(pid)
}

type wakeLoopFunc func(wakeConfig) error

func runWake(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "check":
			return runWakeCheck(args[1:])
		case "config":
			return runWakeConfig(args[1:])
		case "repair":
			return runWakeRepair(args[1:])
		case "restart":
			return runWakeRestart(args[1:])
		case "retire":
			return runWakeRetire(args[1:])
		case "recover-owner":
			return runWakeRecoverOwner(args[1:])
		case "dialog":
			return runWakeDialog(args[1:], os.Stdin)
		case "install-dialog-hook":
			return runWakeInstallDialogHook(args[1:])
		case "uninstall-dialog-hook":
			return runWakeUninstallDialogHook(args[1:])
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
		Lock:   filepath.Join(fsq.AgentBase(root, me), wakeLockFileName),
		Target: wakeTargetPath(root, me),
	}
	if err := os.MkdirAll(fsq.AgentBase(root, me), 0o700); err != nil {
		result.Status = "error"
		err = withWakeDiagnostic(err, result.Root, result.Agent)
		result.Reason = err.Error()
		return result, err
	}
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		result.Status = "error"
		err = withWakeDiagnostic(err, result.Root, result.Agent)
		result.Reason = err.Error()
		return result, err
	}
	defer func() { _ = agentDir.Close() }()

	prep := wakeRepairPreparation{result: &result}
	defer func() { _ = prep.inboxDir.Close() }()
	prepareErr := withWakeMutationScopeInDir(agentDir, wakeGuardWait, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		return prep.prepareAt(scope, dirfd, agentDir, root, me)
	})
	if prepareErr != nil {
		prepareErr = withWakeDiagnostic(prepareErr, result.Root, result.Agent)
		result.Reason = prepareErr.Error()
		return result, prepareErr
	}

	// Spawning and the private prepared handshake happen without the lifecycle
	// guard. The retained directory capability remains open across the wait.
	target, lineage := prep.target, prep.lineage
	child, startErr := startWakeFromTarget(agentDir, prep.inboxDir, root, me, target, lineage)
	if startErr != nil {
		if child != nil {
			_ = cleanupFailedWakeRepairChild(agentDir, root, me, child)
		}
		result.RepairAvailable = false
		result.Status = "error"
		startErr = withWakeDiagnostic(startErr, result.Root, result.Agent)
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
			winnerErr = fmt.Errorf("%s (cleanup: %v)", result.Reason, cleanupErr)
		} else {
			winnerErr = errors.New(result.Reason)
		}
		winnerErr = withWakeDiagnostic(winnerErr, result.Root, result.Agent)
		result.Reason = winnerErr.Error()
		return result, winnerErr
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
			err = fmt.Errorf("%s (cleanup: %v)", result.Reason, cleanupErr)
		} else {
			err = errors.New(result.Reason)
		}
		err = withWakeDiagnostic(err, result.Root, result.Agent)
		result.Reason = err.Error()
		return result, err
	}
	result.Status = "repaired"
	result.PID = winner.PID
	return result, nil
}

// wakeRepairPreparation carries what repairWake proves inside its mutation
// scope to the unguarded child start. repairWake defers the inbox release.
type wakeRepairPreparation struct {
	result   *wakeRepairResult
	target   wakeTarget
	lineage  wakeRepairLineage
	inboxDir *wakeInboxDir
}

func (p *wakeRepairPreparation) prepareAt(
	scope *wakeMutationScope,
	dirfd int,
	agentDir *wakeAgentDir,
	root, me string,
) error {
	result := p.result
	var repairFloor wakeRepairFloor
	var unverifiedSupersession wakeLockInspection
	var err error
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
				"unverified wake is not an ownerless generic claim; use %s for an owner-bound claim",
				wakeRecoverOwnerCommand(root, me),
			)
			return errors.New(result.Reason)
		}
		unverifiedSupersession = inspection
	default:
		result.Status = "refused"
		result.Reason = fmt.Sprintf("wake lock status %q is not repairable", inspection.Status)
		return errors.New(result.Reason)
	}
	if err := reconcileBoundWakePreparedProjectionAt(scope, inspection); err != nil {
		result.Status = "refused"
		result.PID = inspection.PID
		result.Reason = err.Error()
		return err
	}

	var exists bool
	selection, err := readWakeStateSelectionForInspectionAt(
		dirfd, agentDir, root, me, inspection,
	)
	p.target, exists = selection.Target, selection.TargetPresent
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
	if err := validateWakeTarget(p.target, root, me); err != nil {
		result.Status = "refused"
		result.Reason = err.Error()
		return err
	}
	if p.target.Owner != nil {
		result.Status = "refused"
		result.PID = inspection.PID
		result.Reason = fmt.Sprintf("owner-bearing wake state requires %s", wakeRecoverOwnerCommand(root, me))
		return errors.New(result.Reason)
	}
	if err := validateWakeTargetMatchesLock(inspection.Lock, p.target); err != nil {
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
	if err := validateWakeRepairFloor(repairFloor, root, me, inspection.Lock, p.target); err != nil {
		result.Status = "refused"
		result.Reason = err.Error()
		return err
	}
	if err := validateWakeRepairFloorCurrentBoot(repairFloor); err != nil {
		result.Status = "refused"
		result.Reason = err.Error()
		return err
	}
	p.inboxDir, err = openWakeRepairInboxDir(agentDir)
	if err != nil {
		result.Status = "refused"
		result.Reason = err.Error()
		return err
	}
	handoffSource, err := newWakeRepairHandoffSource(
		repairFloor,
		p.target,
		agentDir,
		p.inboxDir,
	)
	if err != nil {
		result.Status = "refused"
		result.Reason = err.Error()
		return err
	}
	p.lineage = wakeRepairLineage{
		source: wakeRepairSource{
			Root:                 handoffSource.Root(),
			RootIdentity:         handoffSource.RootIdentity(),
			Agent:                handoffSource.Agent(),
			DeadGeneration:       handoffSource.SourceGeneration(),
			BootID:               handoffSource.BootID(),
			Owner:                handoffSource.Owner(),
			SourceTargetDigest:   handoffSource.SourceTargetDigest(),
			SourceFloorDigest:    handoffSource.SourceFloorDigest(),
			AgentDirDevice:       handoffSource.agentDirDevice,
			AgentDirInode:        handoffSource.agentDirInode,
			InboxParentDirDevice: handoffSource.inboxParentDirDevice,
			InboxParentDirInode:  handoffSource.inboxParentDirInode,
			InboxDirDevice:       handoffSource.inboxDirDevice,
			InboxDirInode:        handoffSource.inboxDirInode,
		},
		floor: repairFloor,
	}
	result.RepairAvailable = true
	var removeErr error
	if unverifiedSupersession.Exists {
		removeErr = supersedeUnverifiedGenericWakeAt(scope, unverifiedSupersession)
	} else {
		removeErr = removeWakeLockIfUnchangedGuardedAt(scope, inspection)
	}
	if removeErr != nil {
		result.Status = "refused"
		result.RepairAvailable = false
		result.PID = inspectWakeLockAt(dirfd, agentDir, root, me).PID
		var detachedCleanup *wakeDetachedCleanupOnlyError
		if errors.As(removeErr, &detachedCleanup) {
			result.Reason = removeErr.Error()
			return removeErr
		}
		result.Reason = "wake lock changed before repair"
		return errors.New(result.Reason)
	}
	return nil
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
	err := withWakeLifecycleGuardInDir(agentDir, wakeGuardWait, func(dirfd int) error {
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
	metadataErr := withWakeMutationScopeInDir(agentDir, wakeGuardWait, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
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
		if err := removeWakeLockIfUnchangedGuardedAt(scope, current); err != nil {
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
		source.inboxParentDirDevice != lineage.source.InboxParentDirDevice ||
		source.inboxParentDirInode != lineage.source.InboxParentDirInode ||
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
	capability, err := prepareWakeRepairChildCapabilityPlatform(cmd)
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
		if err := unix.Ftruncate(int(output.Fd()), 0); err != nil {
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
			if err := unix.Ftruncate(fd, 0); err != nil {
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
	if retryUntil, _ := normalizeWakeRetryUntil(target.RetryUntil); retryUntil == wakeRetryUntilInjected {
		args = append(args, "--retry-until", retryUntil)
	}
	for _, arg := range target.InjectArgs {
		args = append(args, "--inject-arg", arg)
	}
	if readyPath != "" {
		args = append(args, "--ready-file", readyPath)
	}
	return args
}

func cleanupWakeResumeStageBeforeLock(
	cleanupStage func() error,
	cleanupLock func(),
	retainLock *bool,
) error {
	if retainLock == nil {
		return fmt.Errorf("wake restart cleanup retention state is missing")
	}
	if cleanupStage != nil {
		if err := cleanupStage(); err != nil {
			*retainLock = true
			return fmt.Errorf("cleanup resumed wake stage before lock release: %w", err)
		}
	}
	if cleanupLock != nil && !*retainLock {
		cleanupLock()
	}
	return nil
}

func newWakeRestartCleanupRetention(bootstrap *wakeResumeBootstrap) *bool {
	retain := bootstrap != nil
	return &retain
}

func commitWakeRestartReadiness(
	agentDir *wakeAgentDir,
	root, me string,
	current wakeLockInspection,
	bootstrap wakeResumeBootstrap,
	retainLock *bool,
	cleanupPrevious func(wakeResumeBootstrap) error,
) error {
	if retainLock == nil {
		return fmt.Errorf("wake restart cleanup retention state is missing")
	}
	if cleanupPrevious == nil {
		return fmt.Errorf("wake restart previous-stage cleanup is missing")
	}
	if err := cleanupPrevious(bootstrap); err != nil {
		*retainLock = true
		return fmt.Errorf("cleanup previous wake restart stage before readiness commit: %w", err)
	}
	if err := consumeWakeRestartAfterPrepared(agentDir, root, me, current, bootstrap); err != nil {
		*retainLock = true
		return err
	}
	*retainLock = false
	return nil
}

// wakeStart is the state of one wake startup, from flag parsing to the
// handoff into the event loop. Its methods are the startup phases;
// runWakeWithLoop owns every deferred release, so a phase return never
// closes a capability, stops a listener, or releases the lock. The one
// exception is reuseExistingWake, which closes its readiness publication on
// return; its caller then returns at once.
type wakeStart struct {
	args  []string
	flags *wakeStartFlags

	resumeBootstrap      *wakeResumeBootstrap
	resumePreflight      *wakeResumeBootstrap
	resumeContext        *wakeResumeBootstrap
	restartSignals       chan os.Signal
	privateStop          <-chan struct{}
	attention            *os.File
	startupHold          <-chan struct{}
	repairHandoff        *wakeRepairChildHandoff
	repairHandoffPresent bool

	injectMode          string
	harness             string
	requestedInjectMode string
	retryUntil          string
	flagSettings        wakeSettings
	explicitSettings    []string
	me                  string
	root                string
	injectVia           string
	readyFile           string
	repairGeneration    string
	interruptKey        string

	resumeImageEvidence *wakeImageEvidenceV1
	cleanupResumeStage  func() error
	// A resumed generation keeps its lock until the previous stage is reclaimed
	// and the exact restart record is consumed at the readiness boundary.
	retainLockOnRestartCleanupError *bool

	requestedOwner   *wakeOwner
	ownerObservation wakeOwnerObservation

	initialNotifierStatus string
	initialNotifierMode   string
	initialNotifierReason string

	target         *wakeTarget
	repairLineage  *wakeRepairLineage
	repairAgentDir *wakeAgentDir
	repairInboxDir *wakeInboxDir
	activeAgentDir *wakeAgentDir

	// cleanup releases the acquired lock. The deferred stage cleanup reads it
	// when runWakeWithLoop returns, after acquireLock has set it.
	cleanup                 func()
	resumeEligible          bool
	reloadTransportEligible bool
	// The lock options hold this field's address and onBaselineReady writes
	// through it, so the generation cleanup sees the published floor.
	repairFloorAuthority wakeRepairFloorAuthority

	controlStop                   <-chan struct{}
	currentWake                   wakeLockInspection
	startupSettings               wakeSettingsStartup
	machineSettingsPath           string
	pendingStartupSettingsApplied *wakeSettingsAppliedStatus
	selfUpgrade                   wakeSelfUpgradeState
	terminalAuthority             *wakeTerminalAuthority
}

type wakeStartFlags struct {
	fs                   *flag.FlagSet
	common               *commonFlags
	settings             *wakeSettings
	injectCmd            *string
	injectVia            *string
	injectArgs           multiStringFlag
	retryUntil           *string
	injectMode           *string
	harness              *string
	interruptCmd         *string
	readyFile            *string
	debug                *bool
	acceptExistingWake   *bool
	refuseUnverifiedWake *bool
	repairLineage        *string
	baselineExisting     *bool
	noSelfUpgrade        *bool
	resumePreflight      *bool
	usage                func()
}

func newWakeStartFlags() *wakeStartFlags {
	fs := flag.NewFlagSet("wake", flag.ContinueOnError)
	f := &wakeStartFlags{fs: fs}
	f.common = addCommonFlags(fs)
	f.settings = registerWakeSettingsFlags(fs)
	f.injectCmd = fs.String("inject-cmd", "", "Command to inject (power user mode)")
	f.injectVia = fs.String("inject-via", "", "External executable for injection (payload appended as last arg, bypasses TTY requirement)")
	fs.Var(&f.injectArgs, "inject-arg", "Argument for --inject-via before the payload (repeatable)")
	f.retryUntil = fs.String("retry-until", wakeRetryUntilDrained, "Doorbell acknowledgement: drained or injected")
	f.injectMode = fs.String("inject-mode", wakeInjectModeAuto, "Injection mode: auto, raw, paste, none (auto detects CLI type)")
	f.harness = fs.String("harness", "", "Program the wake types into (claude, codex, agy, pi, cursor, grok, or other for generic behavior); amq coop exec sets it. Without it, a handle containing claude or codex picks raw typing")
	f.interruptCmd = fs.String("interrupt-cmd", "none", "Interrupt command to inject: none (default) or ctrl-c (sends real SIGINT to the foreground process group and can interrupt or crash the agent)")
	f.readyFile = fs.String("ready-file", "", "Internal: write this file after wake lock acquisition")
	f.debug = fs.Bool("debug", false, "Log injection diagnostics to stderr")
	f.acceptExistingWake = fs.Bool("accept-existing-wake", false, "Internal: allow a usable existing wake to satisfy readiness")
	f.refuseUnverifiedWake = fs.Bool("refuse-unverified-wake", false, "Internal: refuse unverified wake locks instead of superseding them")
	f.repairLineage = fs.String("repair-lineage", "", "Internal: inherit the suppression floor from an exact dead wake generation")
	f.baselineExisting = fs.Bool("baseline-existing", false, "Ignore messages already waiting when this wake starts")
	f.noSelfUpgrade = fs.Bool("no-self-upgrade", false, "Disable automatic replacement by a newer installed AMQ image")
	f.resumePreflight = fs.Bool(wakeResumePreflightFlag, false, "Internal: validate an exact wake resume without starting")

	f.usage = usageWithHiddenFlags(fs, "amq wake --me <agent> [options]",
		[]string{"ready-file", "accept-existing-wake", "refuse-unverified-wake", "repair-lineage", wakeResumePreflightFlag},
		wakeUsageNotes...)
	return f
}

var wakeUsageNotes = []string{
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
	"  --retry-until drained (default) reannounces until inbox progress.",
	"  --retry-until injected stops reannouncing an unchanged cohort after",
	"  the injector proves acceptance (stderr AMQ_INJECT_PROGRESS=accepted,",
	"  exit 0) or, for marker-less legacy injectors, after a bare exit 0.",
	"  AMQ_INJECT_PROGRESS=deferred retains the cohort and retries through",
	"  the wake loop; failed and uncertain outcomes are terminal for that",
	"  unchanged cohort and are never silently replayed — a new inbox",
	"  change re-arms delivery. Standalone ownerless wakes execute the",
	"  injector once per physical inbox cohort.",
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
	"Hold by priority (default off): --hold-normal and --hold-low delay the",
	"  first doorbell for routine mail. Its deadline is the inbox file's mtime",
	"  plus its priority's hold; missing priority counts as normal. A later",
	"  message can only pull the deadline earlier. A future-dated mtime gets",
	"  no hold. Urgent mail skips the hold, even behind a parked cohort.",
	"  Ordinary urgent mail still follows debounce and input deferral; mail",
	"  with the configured interrupt label uses the interrupt path. After the",
	"  first attempt, the configured retry policy applies. A drain can take held",
	"  mail, up to its limit (default 20, filename order). The hold never acks",
	"  or deletes mail or guarantees consumption time.",
	"  Example: --hold-normal 5m --hold-low 30m; use --priority urgent for",
	"  time-critical unblocking requests or verdicts.",
	"",
	"Settings (hold, debounce, preview, bell, input deferral, interrupt notice,",
	"  inject timeout, self-upgrade, held-doorbell notice) live in",
	"  <root>/agents/<me>/.wake.settings,",
	"  over the machine file ~/.amq/wake.settings (every wake of this user on this",
	"  machine), over the built-in defaults, key by key. Explicit settings flags on",
	"  a fresh start write their keys to the agent file. A resume (self-upgrade or",
	"  restart) reads the file and ignores settings flags in argv; if the file is",
	"  absent it writes them once. Repair, coop exec and keepalive pass no settings",
	"  flags and use the stored files. A refused machine file never stops a start;",
	"  the wake runs without it. Change running wakes with amq wake config",
	"  (--machine for all).",
	"",
	"Interrupt notices (default on): urgent messages tagged with label \"interrupt\"",
	"  trigger an interrupt notice. Ctrl+C injection is opt-in with",
	"  --interrupt-cmd ctrl-c; it sends real SIGINT to the foreground process",
	"  group and can interrupt or crash the agent.",
	"",
	"Safety: raw, paste, --inject-cmd, --inject-via, and opt-in interrupt Ctrl+C",
	"  can activate a focused permission/approval dialog. With the Claude Code",
	"  dialog hook (amq wake install-dialog-hook), the wake writes no input while",
	"  AskUserQuestion, ExitPlanMode, or a permission dialog is open, and rings",
	"  about 2s after it closes; after a permission dialog it rings when the turn",
	"  ends or the person types. A marker older than 10m gets attention output,",
	"  still no input. --harness codex and --harness cursor guard their approval",
	"  prompts until the turn ends. Dialogs without a hook (workspace trust,",
	"  pickers, MCP elicitation, Cursor prompts before a tool call) are not",
	"  guarded. amq wake dialog is the hook receiver; amq wake",
	"  uninstall-dialog-hook removes the hooks. Use none when AMQ must enforce",
	"  zero synthetic input; stderr output may scribble until redraw.",
	"",
	"Self-upgrade: eligible co-op wakes observe their stable launch symlink and",
	"  replace the running image only with a strictly newer installed AMQ while",
	"  preserving PID, terminal ownership, and unread work. Disable at start with",
	"  --no-self-upgrade or AMQ_WAKE_NO_SELF_UPGRADE=1, or live with amq wake config",
	"  --self-upgrade=false (--machine for every wake). A live true does not enable",
	"  a wake started with --no-self-upgrade; restart it to enable.",
	"",
	"EXPERIMENTAL: Uses TIOCSTI ioctl (macOS/Linux). May not work on all systems.",
}

func runWakeWithLoop(args []string, loop wakeLoopFunc) (returnErr error) {
	// The short-lived companion launcher owns the startup stderr reader. If its
	// detached drain process later disappears, established wakes must receive
	// EPIPE from diagnostic writes instead of being terminated by SIGPIPE.
	restoreBrokenPipe := ignoreWakeBrokenPipe()
	defer restoreBrokenPipe()

	s := &wakeStart{args: args}
	var err error
	s.resumeBootstrap, err = wakeResumeBootstrapFromEnv()
	if err != nil {
		return err
	}
	s.resumePreflight, err = wakeResumePreflightFromEnv()
	if err != nil {
		return err
	}
	if s.resumeBootstrap != nil && s.resumePreflight != nil {
		return fmt.Errorf("wake resume bootstrap and preflight cannot be combined")
	}
	// Install restart delivery before a resumable generation can be published.
	// SIGUSR1's default disposition is termination, so advertising first would
	// create a small but real kill window.
	s.restartSignals = make(chan os.Signal, 1)
	signal.Notify(s.restartSignals, syscall.SIGUSR1)
	defer signal.Stop(s.restartSignals)
	privateStop, cleanupPrivateStop, err := authoritativeWakePrivateStopFromEnv()
	if err != nil {
		return err
	}
	defer cleanupPrivateStop()
	s.attention, err = wakeAttentionFromEnv()
	if err != nil {
		return err
	}
	s.startupHold, err = wakeStartupHoldFromEnv()
	if err != nil {
		return err
	}
	if s.attention != nil {
		defer func() { _ = s.attention.Close() }()
	}
	s.repairHandoff, s.repairHandoffPresent, err = wakeRepairChildHandoffFromEnv()
	if err != nil {
		return err
	}
	if s.repairHandoff != nil {
		defer func() { _ = s.repairHandoff.Close() }()
	}
	repairPrivateStop, cleanupRepairPrivateStop, err := wakeRepairChildStopFromEnv()
	if err != nil {
		return err
	}
	defer cleanupRepairPrivateStop()
	s.privateStop = mergeWakeStopChannels(privateStop, repairPrivateStop)

	s.flags = newWakeStartFlags()
	if handled, err := parseFlags(s.flags.fs, args, s.flags.usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := s.validateFlags(); err != nil {
		return err
	}
	if err := s.prepareResume(); err != nil {
		return err
	}
	resumeStageCleanupTransferred := false
	if s.cleanupResumeStage != nil {
		defer func() {
			if !resumeStageCleanupTransferred {
				returnErr = errors.Join(returnErr, s.cleanupResumeStage())
			}
		}()
	}

	s.requestedOwner, err = wakeOwnerFromEnv()
	if err != nil {
		return err
	}
	if s.resumePreflight != nil {
		return s.runResumePreflight()
	}
	if s.repairGeneration != "" && s.requestedOwner != nil {
		return fmt.Errorf("owner-bearing wake state requires %s", wakeRecoverOwnerCommand(s.root, s.me))
	}
	if s.requestedOwner != nil {
		s.ownerObservation, err = observeLiveWakeOwner(
			*s.requestedOwner,
			"inspect requested wake owner before lock acquisition",
		)
		if err != nil {
			return err
		}
		defer func() {
			if err := s.ownerObservation.Close(); err != nil {
				returnErr = errors.Join(
					returnErr,
					fmt.Errorf("requested wake owner observation failed: %w", err),
				)
			}
		}()
	}

	if err := s.checkTerminalInjection(); err != nil {
		return err
	}
	s.interruptKey, err = parseInterruptKey(*s.flags.interruptCmd)
	if err != nil {
		return UsageError("%v", err)
	}

	if s.injectVia != "" {
		if err := s.bindTarget(); err != nil {
			return err
		}
		if s.repairGeneration != "" {
			source, err := s.takeRepairDirectories()
			if err != nil {
				return err
			}
			defer func() { _ = s.repairAgentDir.Close() }()
			defer func() { _ = s.repairInboxDir.Close() }()
			if err := s.bindRepairLineage(source); err != nil {
				return err
			}
		}
	}
	s.activeAgentDir = s.repairAgentDir
	if s.activeAgentDir == nil {
		s.activeAgentDir, err = openWakeAgentDir(s.root, s.me)
		if err != nil {
			return err
		}
		defer func() { _ = s.activeAgentDir.Close() }()
	}
	defer func() {
		returnErr = errors.Join(
			returnErr,
			cleanupWakeResumeStageBeforeLock(
				s.cleanupResumeStage,
				s.cleanup,
				s.retainLockOnRestartCleanupError,
			),
		)
	}()
	resumeStageCleanupTransferred = true

	if reused, err := s.acquireLock(); reused || err != nil {
		return err
	}
	if err := s.inspectAcquiredWake(); err != nil {
		return err
	}
	if err := s.loadStartupSettings(); err != nil {
		return err
	}
	if s.currentWake.Lock.ControlSocket != "" {
		controlCleanup, stop, markStopped, controlErr := startWakeControlListenerInDirWithRestart(
			s.activeAgentDir, s.root, s.me, s.currentWake.Lock, s.restartSignals,
		)
		if controlErr != nil {
			return controlErr
		}
		defer controlCleanup()
		defer markStopped()
		if stop != nil {
			s.controlStop = mergeWakeStopChannels(s.controlStop, stop)
		}
	}

	if err := s.prepareNotifier(); err != nil {
		return err
	}
	if err := s.bindTerminalAuthority(); err != nil {
		return err
	}
	if s.terminalAuthority != nil {
		defer func() { _ = s.terminalAuthority.Close() }()
	}
	cfg := s.newLoopConfig()

	stopReloadTransport, err := s.startReloadTransport()
	if err != nil {
		return err
	}
	if stopReloadTransport != nil {
		defer stopReloadTransport()
	}

	defer func() {
		_ = setWakeDoorbellStatusInDir(s.activeAgentDir, s.me, false, 0)
	}()
	s.recordStartupSelfUpgrade(&cfg)
	return loop(cfg)
}

func (s *wakeStart) validateFlags() error {
	f := s.flags
	if *f.resumePreflight != (s.resumePreflight != nil) {
		return fmt.Errorf("wake resume preflight flag and environment must be paired")
	}
	injectMode, err := normalizeWakeInjectMode(*f.injectMode)
	if err != nil {
		return UsageError("%v", err)
	}
	s.injectMode = injectMode
	s.requestedInjectMode = injectMode
	if s.harness = strings.TrimSpace(*f.harness); s.harness != "" {
		if err := fsq.ValidateHandle(s.harness); err != nil {
			return UsageError("--harness: %v", err)
		}
	}
	s.retryUntil, err = normalizeWakeRetryUntil(*f.retryUntil)
	if err != nil {
		return UsageError("%v", err)
	}
	s.flagSettings, err = f.settings.validate()
	if err != nil {
		return UsageError("%v", err)
	}
	s.explicitSettings = visitedWakeSettingsKeys(f.fs)

	if err := requireMe(f.common.Me); err != nil {
		return err
	}
	s.me, err = normalizeHandle(f.common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}

	s.root = resolveRoot(f.common.Root)
	if err := validateKnownHandles(s.root, f.common.Strict, s.me); err != nil {
		return err
	}
	s.injectVia = strings.TrimSpace(*f.injectVia)
	if *f.injectVia != "" && s.injectVia == "" {
		return UsageError("--inject-via must not be blank")
	}
	if s.injectMode == wakeInjectModeNone && s.injectVia != "" {
		return UsageError("--inject-via cannot be used with --inject-mode none")
	}
	if s.injectMode == wakeInjectModeNone && len(f.injectArgs) > 0 {
		return UsageError("--inject-arg cannot be used with --inject-mode none")
	}
	if s.injectMode == wakeInjectModeNone && *f.injectCmd != "" {
		return UsageError("--inject-cmd cannot be used with --inject-mode none")
	}
	if s.injectVia == "" && len(f.injectArgs) > 0 {
		return UsageError("--inject-arg requires --inject-via")
	}
	if s.retryUntil == wakeRetryUntilInjected && s.injectVia == "" {
		return UsageError("--retry-until injected requires --inject-via")
	}
	s.readyFile = strings.TrimSpace(*f.readyFile)
	if *f.readyFile != "" && s.readyFile == "" {
		return UsageError("--ready-file must not be blank")
	}
	s.repairGeneration = strings.TrimSpace(*f.repairLineage)
	if *f.repairLineage != "" && s.repairGeneration == "" {
		return UsageError("--repair-lineage must not be blank")
	}
	if s.repairGeneration != "" && s.injectVia == "" {
		return UsageError("--repair-lineage requires --inject-via")
	}
	if s.repairGeneration != "" && *f.baselineExisting {
		return UsageError("--repair-lineage cannot be combined with --baseline-existing")
	}
	if s.repairGeneration != "" && !s.repairHandoffPresent {
		return fmt.Errorf("wake repair requires a private source/admission handoff")
	}
	if s.repairGeneration == "" && s.repairHandoffPresent {
		return fmt.Errorf("wake repair handoff requires --repair-lineage")
	}
	return nil
}

// prepareResume applies a resume bootstrap or preflight. On success with a
// bootstrap it sets cleanupResumeStage; runWakeWithLoop defers that call.
func (s *wakeStart) prepareResume() error {
	s.resumeContext = s.resumeBootstrap
	if s.resumeContext == nil {
		s.resumeContext = s.resumePreflight
	}
	s.retainLockOnRestartCleanupError = newWakeRestartCleanupRetention(s.resumeBootstrap)
	if s.resumeContext == nil {
		return nil
	}
	if s.repairGeneration != "" || s.repairHandoffPresent {
		return fmt.Errorf("wake resume cannot inherit repair state")
	}
	// Readiness files and startup baselines are one-shot launch inputs. A
	// self-resume republishes readiness for its new generation and treats all
	// durable inbox/new entries as pending.
	s.readyFile = ""
	*s.flags.acceptExistingWake = false
	*s.flags.baselineExisting = false
	if s.resumeBootstrap != nil || s.resumeContext.BoundImage != nil {
		verified, verifyErr := verifyWakeResumeBoundImage(*s.resumeContext)
		if verifyErr != nil {
			return verifyErr
		}
		s.resumeImageEvidence = &verified
	}
	if s.resumeBootstrap != nil {
		cleanupBootstrap := *s.resumeBootstrap
		cleanupBootstrap.BoundImage = s.resumeImageEvidence
		s.cleanupResumeStage = func() error {
			return cleanupWakeResumeBoundImage(cleanupBootstrap)
		}
	}
	return nil
}

func (s *wakeStart) runResumePreflight() error {
	if s.requestedOwner == nil {
		return fmt.Errorf("wake resume preflight requires the exact coop owner environment")
	}
	args := s.args
	if len(args) == 0 || args[len(args)-1] != "--"+wakeResumePreflightFlag {
		return fmt.Errorf("wake resume preflight must be the final internal argument")
	}
	preservedArgv := append([]string{os.Args[0], "wake"}, args[:len(args)-1]...)
	if err := validateWakeRestartArgv(preservedArgv, canonicalWakeRoot(s.root), s.me); err != nil {
		return err
	}
	if err := checkWakeSettingsForResume(s.root, s.me); err != nil {
		return err
	}
	return writeStdoutLine(wakeResumePreflightOK)
}

// checkTerminalInjection verifies TIOCSTI is available (skip in inject-via
// mode — uses external command instead). Linux's legacy_tiocsti sysctl is
// advisory: a readable zero degrades this wake to a useful non-input
// notifier, while absence, read errors, and every other value remain unknown
// and do not block startup.
func (s *wakeStart) checkTerminalInjection() error {
	if s.injectVia != "" || s.injectMode == wakeInjectModeNone {
		return nil
	}
	if !wakeTIOCSTIAvailable() {
		return errors.New("TIOCSTI not available on this platform; use tmux send-keys or terminal-specific injection")
	}

	if tiocstiLegacyDisabledHint() {
		s.initialNotifierStatus = wakeInjectorUnsupportedStatus
		s.initialNotifierMode = effectiveInjectMode(&wakeConfig{me: s.me, harness: s.harness, injectMode: s.injectMode})
		s.initialNotifierReason = wakeInjectorUnsupportedReason(
			s.initialNotifierMode,
			fmt.Errorf("%s is 0", tiocstiLegacySysctlPath),
		)
		s.injectMode = wakeInjectModeNone
	} else if !wakeInputIsTTY() {
		// Verify we have a real TTY when synthetic input remains enabled.
		return errors.New("amq wake requires a real terminal (run in foreground or as background job in same terminal, or use --inject-via for external injection)")
	}
	return nil
}

func (s *wakeStart) bindTarget() error {
	value, err := newWakeTarget(s.root, s.me, s.injectVia, []string(s.flags.injectArgs))
	if err != nil {
		return err
	}
	if s.retryUntil == wakeRetryUntilInjected {
		value.RetryUntil = s.retryUntil
	}
	value.Owner = s.requestedOwner
	if err := validateWakeTarget(value, s.root, s.me); err != nil {
		return err
	}
	s.injectVia = value.InjectVia
	s.target = &value
	return nil
}

// takeRepairDirectories receives the repair source and, on success, sets the
// retained repair directories; runWakeWithLoop defers their release.
func (s *wakeStart) takeRepairDirectories() (wakeRepairHandoffSource, error) {
	source, err := s.repairHandoff.ReceiveSource()
	if err != nil {
		return source, fmt.Errorf("receive wake repair source: %w", err)
	}
	if source.Root() != canonicalWakeRoot(s.root) ||
		source.Agent() != s.me ||
		source.SourceGeneration() != s.repairGeneration {
		return source, fmt.Errorf("wake repair source does not match requested root, agent, and generation")
	}
	repairAgentDir, repairInboxDir, err := s.repairHandoff.TakeRetainedDirectories(source)
	if err != nil {
		return source, err
	}
	s.repairAgentDir = repairAgentDir
	s.repairInboxDir = repairInboxDir
	return source, nil
}

func (s *wakeStart) bindRepairLineage(source wakeRepairHandoffSource) error {
	root, me := s.root, s.me
	value := *s.target
	var persisted wakeTarget
	var floor wakeRepairFloor
	err := withWakeLifecycleGuardInDir(s.repairAgentDir, wakeGuardWait, func(dirfd int) error {
		if err := revalidateWakeRepairRootIdentity(root, source.RootIdentity()); err != nil {
			return err
		}
		var exists bool
		var err error
		persisted, exists, err = readWakeTargetAt(dirfd, s.repairAgentDir, root, me)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("wake repair target is missing")
		}
		floor, exists, err = readWakeRepairFloorAt(dirfd, s.repairAgentDir)
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
	s.repairLineage = &wakeRepairLineage{
		source: wakeRepairSource{
			Root:                 source.Root(),
			RootIdentity:         source.RootIdentity(),
			Agent:                source.Agent(),
			DeadGeneration:       source.SourceGeneration(),
			BootID:               source.BootID(),
			Owner:                source.Owner(),
			SourceTargetDigest:   source.SourceTargetDigest(),
			SourceFloorDigest:    source.SourceFloorDigest(),
			AgentDirDevice:       source.agentDirDevice,
			AgentDirInode:        source.agentDirInode,
			InboxParentDirDevice: source.inboxParentDirDevice,
			InboxParentDirInode:  source.inboxParentDirInode,
			InboxDirDevice:       source.inboxDirDevice,
			InboxDirInode:        source.inboxDirInode,
		},
		floor: floor,
	}
	s.target = &persisted
	s.injectVia = persisted.InjectVia
	s.flags.injectArgs = append(multiStringFlag(nil), persisted.InjectArgs...)
	return nil
}

// acquireLock takes the wake lock and sets cleanup. It returns true when an
// existing wake satisfied readiness instead; runWakeWithLoop then returns the
// error without running the loop.
func (s *wakeStart) acquireLock() (bool, error) {
	// Acquire lock to prevent duplicate wake processes
	acceptExistingWake := s.readyFile != "" && *s.flags.acceptExistingWake
	lockWakeMode := s.injectMode
	if s.target != nil {
		lockWakeMode = wakeTargetInjectVia
	} else if lockWakeMode != wakeInjectModeNone {
		lockWakeMode = effectiveInjectMode(&wakeConfig{me: s.me, harness: s.harness, injectMode: lockWakeMode})
	}
	acceptExistingDeadline := time.Now().Add(wakeReadyTimeout)
	s.resumeEligible = s.target == nil && wakeResumeStartupEligible(
		s.requestedOwner,
		s.repairLineage != nil,
		*s.flags.injectCmd,
		s.interruptKey,
		lockWakeMode,
	)
	s.reloadTransportEligible = s.resumeEligible && s.target == nil
	for {
		if s.requestedOwner != nil {
			select {
			case <-s.ownerObservation.Done():
				return false, fmt.Errorf("requested wake owner observation ended before lock acquisition")
			default:
			}
		}
		options := wakeLockAcquireOptions{
			acceptExistingValid:     acceptExistingWake,
			refuseUnverifiedGeneric: *s.flags.refuseUnverifiedWake,
			target:                  s.target,
			wakeMode:                lockWakeMode,
			debug:                   *s.flags.debug,
			requestedOwner:          s.requestedOwner,
			repairLineage:           s.repairLineage,
			resumeEligible:          s.resumeEligible,
			resumeImageEvidence:     s.resumeImageEvidence,
		}
		if s.repairLineage != nil {
			options.repairFloorAuthority = &s.repairFloorAuthority
		}
		var err error
		if s.resumeBootstrap != nil {
			s.cleanup, err = acquireWakeLockAfterResumeInDir(
				s.activeAgentDir,
				s.root,
				s.me,
				options,
				*s.resumeBootstrap,
			)
		} else {
			s.cleanup, err = acquireWakeLockWithOptionsInDir(s.activeAgentDir, s.root, s.me, options)
		}
		if err == nil {
			return false, nil
		}
		if s.resumeBootstrap != nil {
			return false, err
		}
		var creating *wakeLockCreatingError
		if acceptExistingWake && errors.As(err, &creating) {
			if !waitForWakePreparedRetry(acceptExistingDeadline) {
				return false, fmt.Errorf("wake lock did not finish creation within %s", wakeReadyTimeout)
			}
			continue
		}
		var snapshotChanged *wakeSnapshotReadChangedError
		var boundInconclusive *wakeStateBoundInconclusiveError
		if acceptExistingWake && (errors.As(err, &snapshotChanged) || errors.As(err, &boundInconclusive)) {
			if !waitForWakePreparedRetry(acceptExistingDeadline) {
				return false, fmt.Errorf(
					"wake lock did not stabilize within %s: %w",
					wakeReadyTimeout,
					err,
				)
			}
			continue
		}
		var alreadyRunning *wakeAlreadyRunningError
		if acceptExistingWake && errors.As(err, &alreadyRunning) {
			return true, s.reuseExistingWake(alreadyRunning, acceptExistingDeadline)
		}
		return false, err
	}
}

// reuseExistingWake publishes readiness for a usable existing wake. The
// caller returns at once, so its deferred publication close runs before every
// release that runWakeWithLoop defers.
func (s *wakeStart) reuseExistingWake(
	alreadyRunning *wakeAlreadyRunningError,
	acceptExistingDeadline time.Time,
) error {
	if s.requestedOwner != nil {
		select {
		case <-s.ownerObservation.Done():
			return fmt.Errorf("requested wake owner observation ended before existing-wake readiness")
		default:
		}
	}
	publication, err := writeWakeReadyFileForPreparedWakeInDir(
		s.activeAgentDir,
		s.root,
		s.me,
		s.readyFile,
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
	if s.requestedOwner != nil {
		ready, readyErr := validateWakeReadyFileAgainstOwnerInDir(
			s.activeAgentDir,
			s.root,
			s.me,
			s.readyFile,
			s.requestedOwner,
		)
		if readyErr != nil {
			return cleanupReady(readyErr)
		}
		if !ready {
			return cleanupReady(fmt.Errorf("existing wake readiness disappeared before owner validation"))
		}
	}
	if err := validateCanonicalWakeAgentDir(s.activeAgentDir); err != nil {
		return cleanupReady(err)
	}
	if *s.flags.baselineExisting {
		_ = writeStderr("warning: reusing existing amq wake; this launch did not re-baseline it, so pending backlog may still notify\n")
	}
	if len(s.explicitSettings) > 0 {
		_ = writeStderr(
			"warning: reusing existing amq wake; its settings flags were not applied; use amq wake config --root %s --me %s to change the running wake\n",
			shellQuoteArg(s.root),
			shellQuoteArg(s.me),
		)
	}
	return nil
}

func (s *wakeStart) inspectAcquiredWake() error {
	s.controlStop = s.privateStop
	if s.requestedOwner != nil {
		s.controlStop = mergeWakeStopChannels(s.controlStop, s.ownerObservation.Done())
	}
	return s.activeAgentDir.withFD(func(dirfd int) error {
		s.currentWake = inspectWakeLockAt(dirfd, s.activeAgentDir, s.root, s.me)
		return nil
	})
}

func (s *wakeStart) loadStartupSettings() error {
	// The file owns settings. Only a fresh start that holds the lock stores
	// its explicit settings flags; every other start reads the file.
	startupSettings, err := loadWakeSettingsAtStartup(
		s.activeAgentDir,
		s.flagSettings,
		s.explicitSettings,
		s.resumeContext != nil,
	)
	if err != nil {
		return err
	}
	s.startupSettings = startupSettings
	s.machineSettingsPath, err = machineWakeSettingsPath()
	if err != nil {
		s.machineSettingsPath = wakeMachineSettingsDisplayPath
	}
	if startupSettings.layers.agentErr != nil {
		_ = writeStderr(
			"amq wake: %s refused: %v; running without it\n",
			wakeSettingsFileName,
			startupSettings.layers.agentErr,
		)
	}
	if startupSettings.layers.machineErr != nil {
		_ = writeStderr(
			"amq wake: %s refused: %v; running without the machine settings\n",
			s.machineSettingsPath,
			startupSettings.layers.machineErr,
		)
	}
	if startupSettings.layers.machineMerge != nil {
		_ = writeStderr(
			"amq wake: %s refused: %v; running without the machine settings\n",
			s.machineSettingsPath,
			startupSettings.layers.machineMerge,
		)
	}
	if startupSettings.unseeded != nil {
		_ = writeStderr(
			"amq wake: %v; running with the settings flags in memory\n",
			startupSettings.unseeded,
		)
	}
	// Publish the startup status before the rest of startup, so wake config
	// does not read this generation as an image without live settings. A
	// failure is retried by the loop's reload. An unseeded resume publishes
	// nothing for the absent file; the loop publishes once its seed lands.
	if startupSettings.unseeded == nil {
		if err := recordWakeSettingsApplied(
			s.activeAgentDir,
			s.currentWake,
			startupSettings.applied,
			wakeSettingsGuardTimeout,
		); err != nil {
			_ = writeStderr(
				"amq wake: record %s: %v; retrying\n",
				wakeSettingsAppliedFileName,
				err,
			)
			s.pendingStartupSettingsApplied = &s.startupSettings.applied
		}
	}
	return nil
}

func (s *wakeStart) prepareNotifier() error {
	if s.injectVia != "" {
		if err := validateResolvedWakeInjectViaPath(s.injectVia); err != nil {
			return err
		}
	}
	var runningImageEvidence wakeImageEvidenceV1
	if s.currentWake.Lock.RunningImageEvidence != nil {
		runningImageEvidence = *s.currentWake.Lock.RunningImageEvidence
	}
	s.selfUpgrade = constrainWakeSelfUpgradeEligibility(
		captureWakeSelfUpgradeStartupState(
			os.Args[0],
			!*s.flags.noSelfUpgrade && !wakeSelfUpgradeDisabledByEnv(),
			runningImageEvidence,
		),
		s.resumeEligible,
	)
	if err := loadWakeSelfUpgradeAttemptAtStartup(
		&s.selfUpgrade,
		s.activeAgentDir,
		s.currentWake,
		runningImageEvidence,
	); err != nil {
		s.selfUpgrade.Eligible = false
		s.selfUpgrade.Reason = "wake self-upgrade attempt state is unavailable: " + err.Error()
	}
	if strings.Contains(s.selfUpgrade.Reason, "pinned resolved image") {
		s.selfUpgrade.Reason += fmt.Sprintf(
			"; request a safe refresh with amq wake restart --root %s --me %s",
			shellQuoteArg(s.root),
			shellQuoteArg(s.me),
		)
	}

	if err := setWakeNotifierStatusInDir(
		s.activeAgentDir,
		s.me,
		s.initialNotifierStatus,
		s.initialNotifierMode,
		s.initialNotifierReason,
	); err != nil {
		return fmt.Errorf("record wake notifier status: %w", err)
	}
	if err := setWakeDoorbellStatusInDir(s.activeAgentDir, s.me, false, 0); err != nil {
		return fmt.Errorf("clear wake doorbell status: %w", err)
	}
	if s.initialNotifierStatus == wakeInjectorUnsupportedStatus {
		_ = writeWakeLogLine("amq wake: warning: %s\n", s.initialNotifierReason)
	}
	return nil
}

// bindTerminalAuthority sets terminalAuthority only on success;
// runWakeWithLoop defers its release.
func (s *wakeStart) bindTerminalAuthority() error {
	effectiveMode := effectiveInjectMode(&wakeConfig{me: s.me, harness: s.harness, injectMode: s.injectMode})
	if s.requestedOwner != nil &&
		s.injectVia == "" &&
		(effectiveMode == wakeInjectModeRaw || effectiveMode == wakeInjectModePaste) {
		terminalAuthority, err := bindWakeTerminalAuthorityForWake(s.currentWake, s.controlStop)
		if err != nil {
			return err
		}
		s.terminalAuthority = terminalAuthority
	}
	return nil
}

func (s *wakeStart) newLoopConfig() wakeConfig {
	root, me := s.root, s.me
	activeAgentDir := s.activeAgentDir
	currentWake := s.currentWake
	startupSettings := s.startupSettings
	cfg := wakeConfig{
		me:                  me,
		root:                root,
		session:             resolveSessionName(root),
		injectCmd:           *s.flags.injectCmd,
		injectVia:           s.injectVia,
		injectArgs:          []string(s.flags.injectArgs),
		wakeOwner:           s.requestedOwner,
		settings:            startupSettings.settings,
		strict:              s.flags.common.Strict,
		fallbackWarn:        true,
		injectMode:          s.injectMode,
		harness:             s.harness,
		requestedInjectMode: s.requestedInjectMode,
		retryUntil:          s.retryUntil,
		debug:               *s.flags.debug,
		interruptKey:        s.interruptKey,
		controlStop:         s.controlStop,
		restartSignals:      s.restartSignals,
		inspectTerminalGeneration: func() wakeLockInspection {
			return inspectWakeTerminalGeneration(root, me)
		},
		terminalGeneration:   currentWake.Lock.Generation,
		terminalImageVersion: currentWake.Lock.ImageVersion,
		terminalTTY:          currentWake.Lock.TTY,
		selfUpgrade:          s.selfUpgrade,
		baselineRequested:    *s.flags.baselineExisting || s.repairLineage != nil,
		baselineInherited:    s.repairLineage != nil,
		retainedAgent:        activeAgentDir,
		recordNotifierStatus: func(status, mode, reason string) error {
			return setWakeNotifierStatusInDir(activeAgentDir, me, status, mode, reason)
		},
		recordDoorbellStatus: func(parked bool, attempts uint) error {
			return setWakeDoorbellStatusInDir(activeAgentDir, me, parked, attempts)
		},
		settingsSource:          wakeSettingsSourceInDir(activeAgentDir, currentWake, startupSettings.seed),
		dialogSource:            withWakeStartupHold(wakeDialogSourceInDir(activeAgentDir), s.startupHold),
		dialogHoldReason:        wakeDialogHoldReasonInDir(activeAgentDir),
		dialogHoldDetail:        wakeDialogHoldDetailInDir(activeAgentDir),
		desktopNotify:           wakeDesktopNotifier(),
		startupTrustRelease:     wakeStartupTrustReleaseInDir(activeAgentDir),
		settingsObserved:        startupSettings.observed,
		machineSettingsSource:   readMachineWakeSettings,
		machineSettingsObserved: startupSettings.machineObserved,
		machineSettingsPath:     s.machineSettingsPath,
		settingsLayers:          startupSettings.layers,
		settingsUnseeded:        startupSettings.unseeded != nil,
		pendingSettingsApplied:  s.pendingStartupSettingsApplied,
		recordSettingsApplied: func(status wakeSettingsAppliedStatus) error {
			return recordWakeSettingsApplied(
				activeAgentDir,
				currentWake,
				status,
				wakeLifecycleGuardRetryTimeout,
			)
		},
		onPrepared: s.onPrepared,
	}
	if s.attention != nil {
		attention := s.attention
		cfg.attentionWrite = attention.Write
		cfg.attentionIsTTY = func() bool {
			return wakeAttentionFileIsTerminal(attention)
		}
	}
	if s.terminalAuthority != nil {
		cfg.beforeTerminalWrite = s.terminalAuthority.BeforeWrite
		cfg.terminalWrite = s.terminalAuthority.Inject
	}
	if s.repairInboxDir != nil {
		repairAgentDir := s.repairAgentDir
		cfg.retainedInbox = s.repairInboxDir
		cfg.touchPresence = func() error {
			return touchWakePresenceInDir(repairAgentDir, me)
		}
	}
	if s.repairLineage != nil {
		cfg.baselineExisting = cloneWakeFileIdentities(s.repairLineage.floor.Existing)
	}
	if s.target != nil && s.target.Owner == nil {
		persistedTarget := *s.target
		cfg.onBaselineReady = func(existing map[string]wakeFileIdentity) error {
			return s.publishRepairFloor(persistedTarget, existing)
		}
	}
	return cfg
}

func (s *wakeStart) onPrepared(watcher wakeAdmissionWatcher) error {
	if s.repairLineage != nil {
		return s.prepareRepairChild(watcher)
	}
	if err := writeWakePreparedFileInDir(s.activeAgentDir, s.root, s.me, s.currentWake); err != nil {
		return err
	}
	if s.resumeBootstrap != nil {
		if err := commitWakeRestartReadiness(
			s.activeAgentDir,
			s.root,
			s.me,
			s.currentWake,
			*s.resumeBootstrap,
			s.retainLockOnRestartCleanupError,
			cleanupPreviousWakeResumeBoundImage,
		); err != nil {
			return err
		}
	}
	return writeWakeReadyFileAgainstOwnerInDir(
		s.activeAgentDir,
		s.root,
		s.me,
		s.readyFile,
		s.currentWake,
		s.requestedOwner,
	)
}

func (s *wakeStart) prepareRepairChild(watcher wakeAdmissionWatcher) error {
	root, me := s.root, s.me
	repairAgentDir := s.repairAgentDir
	repairLineage := s.repairLineage
	currentWake := s.currentWake
	if err := writeWakePreparedFileInDir(
		repairAgentDir,
		root,
		me,
		currentWake,
	); err != nil {
		return err
	}
	var prepared wakeRepairHandoffPrepared
	err := withWakeLifecycleGuardInDir(repairAgentDir, wakeGuardWait, func(dirfd int) error {
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
		if !exists || !sameWakeTarget(persisted, *s.target) {
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
	if err := s.repairHandoff.SendPrepared(prepared); err != nil {
		return err
	}
	if err := s.repairHandoff.AwaitAdmitAcknowledgeAndRelease(
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
	case <-s.privateStop:
		return fmt.Errorf("wake repair child stopped before admission completed")
	default:
	}
	return nil
}

func (s *wakeStart) publishRepairFloor(
	persistedTarget wakeTarget,
	existing map[string]wakeFileIdentity,
) error {
	root, me := s.root, s.me
	currentWake := s.currentWake
	if s.repairLineage != nil {
		repairAgentDir := s.repairAgentDir
		floor, err := newInheritedWakeRepairFloor(
			s.repairLineage.source,
			currentWake.Lock,
			persistedTarget,
			existing,
		)
		if err != nil {
			return err
		}
		return withWakeLifecycleGuardInDir(repairAgentDir, wakeGuardWait, func(dirfd int) error {
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
			s.repairFloorAuthority = authority
			return nil
		})
	}
	floor, err := newWakeRepairFloor(root, me, currentWake.Lock, persistedTarget, existing)
	if err != nil {
		return err
	}
	return writeWakeRepairFloorInDir(s.activeAgentDir, root, floor)
}

// startReloadTransport returns the transport's stop function, or nil when
// the transport is ineligible or unavailable. Reload transport is additive
// capability. If this process cannot confirm the freshly published lock
// identity, keep the ordinary notifier running without opening the
// unadvertised endpoint.
func (s *wakeStart) startReloadTransport() (func(), error) {
	if !s.reloadTransportEligible || !s.currentWake.IdentityConfirmed ||
		s.currentWake.Lock.ResumeSignal == wakeResumeSignalUSR1 {
		return nil, nil
	}
	if s.requestedOwner == nil {
		return nil, fmt.Errorf("wake reload transport requires an exact owner")
	}
	select {
	case <-s.ownerObservation.Done():
		return nil, fmt.Errorf("requested wake owner observation ended before reload transport startup")
	default:
	}
	stopReloadTransport, startErr := startWakeReloadTransportInDir(
		s.activeAgentDir,
		s.root,
		s.me,
		s.currentWake,
		*s.requestedOwner,
	)
	if startErr != nil {
		var unavailable *wakeReloadTransportUnavailableError
		if !errors.As(startErr, &unavailable) {
			return nil, startErr
		}
		_ = writeWakeLogLine(
			"amq wake: reload transport unavailable: %v; continuing without reload transport\n",
			startErr,
		)
		return nil, nil
	}
	if stopReloadTransport == nil {
		return nil, fmt.Errorf("wake reload transport returned no cleanup")
	}
	return stopReloadTransport, nil
}

func (s *wakeStart) recordStartupSelfUpgrade(cfg *wakeConfig) {
	selfUpgrade := s.selfUpgrade
	initialSelfUpgrade := selfUpgrade
	initialSelfUpgradeDecision := wakeSelfUpgradeIdleDecision(selfUpgrade)
	if state, decision, vetoed := wakeSelfUpgradeSettingsVeto(
		selfUpgrade,
		cfg.settings,
		cfg.settingsLayers,
	); vetoed {
		cfg.selfUpgrade.settingsVetoed = true
		initialSelfUpgrade = state
		initialSelfUpgradeDecision = decision
	} else if selfUpgrade.Enabled && selfUpgrade.Eligible && selfUpgrade.startupRefusalReason != "" {
		initialSelfUpgradeDecision.Action = wakeSelfUpgradeActionRefusedMemory
		initialSelfUpgradeDecision.Reason = selfUpgrade.startupRefusalReason
	}
	if err := recordWakeSelfUpgradeDecision(
		s.activeAgentDir,
		s.currentWake,
		initialSelfUpgrade,
		initialSelfUpgradeDecision,
	); err != nil {
		_ = writeWakeDiagnostic(
			cfg,
			"amq wake: record self-upgrade startup diagnostic: %v; continuing\n",
			err,
		)
	}
}

func ignoreWakeBrokenPipe() func() {
	wasIgnored := signal.Ignored(syscall.SIGPIPE)
	signal.Ignore(syscall.SIGPIPE)
	return func() {
		if wasIgnored {
			signal.Ignore(syscall.SIGPIPE)
			return
		}
		// Reset alone does not clear os/signal's ignored bit for a signal the Go
		// runtime owns. A temporary notification restores the runtime handler;
		// Stop then returns it to the pre-Ignore, non-notifying state.
		restore := make(chan os.Signal, 1)
		signal.Notify(restore, syscall.SIGPIPE)
		signal.Stop(restore)
	}
}

func snapshotWakeDirEntryInfo(entry os.DirEntry) (os.FileInfo, error) {
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

// prepareWakeBaselineEvents classifies startup backlog after the watcher is
// armed. Linux/inotify provides an ordered marker fence; Darwin/kqueue uses the
// marker plus a quiescence window, with watcher errors handled fail-closed.
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

func classifyWakeFailure(err error) wakeFailureDisposition {
	if err == nil {
		return wakeFailureRetry
	}
	var ownershipLoss *wakeOwnershipLossError
	var canonicalMismatch *wakeInboxCanonicalMismatchError
	if errors.As(err, &ownershipLoss) ||
		errors.As(err, &canonicalMismatch) ||
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
	// Re-read the same TIOCSTI advisory capability signal so a later
	// transition to disabled is surfaced and safely demoted without issuing
	// an ioctl.
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

// wakeChildEnvAllowlist is the set of environment variables the amq wake
// child process is permitted to inherit from its parent. Everything else in
// the parent shell env (secrets, tokens, shell-specific vars) is dropped so a
// compromised or buggy injector child cannot exfiltrate it.
//
// Required OS vars: PATH for exec.LookPath (self-upgrade, restart preflight,
// recursive amq spawn), HOME for os.UserHomeDir config resolution,
// XDG_STATE_HOME for the legacy launch state directory that holds the
// Darwin wake restart stages (restart and self-upgrade staging), TERM_PROGRAM for terminal detection (wake injection),
// and CMUX_SURFACE_ID for cmux surface detection (launch backend).
// All AMQ-internal vars are matched by the AMQ_ / AM_ prefixes
// (session/identity/state, no-update-check, test re-exec helpers) so future
// AMQ vars are covered without enumerating each one.
var wakeChildEnvAllowlistPrefixes = []string{"AMQ_", "AM_"}

var wakeChildEnvAllowlist = map[string]bool{
	"PATH":            true,
	"HOME":            true,
	"XDG_STATE_HOME":  true,
	"TERM_PROGRAM":    true,
	"CMUX_SURFACE_ID": true,
}

// scrubWakeChildEnv filters a parent env slice to the wake-child allowlist
// (7ra). Non-allowlisted vars are dropped before the child is spawned so the
// injector child never sees unrelated parent-shell secrets/tokens. The
// allowlist is matched by key: AMQ_/AM_ prefix OR an explicit allowlist entry
// (PATH, HOME). Callers still set/override specific AMQ vars after.
func scrubWakeChildEnv(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	scrubbed := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue // malformed entry; drop
		}
		if wakeChildEnvAllowlist[key] {
			scrubbed = append(scrubbed, kv)
			continue
		}
		for _, p := range wakeChildEnvAllowlistPrefixes {
			if strings.HasPrefix(key, p) {
				scrubbed = append(scrubbed, kv)
				break
			}
		}
	}
	return scrubbed
}

func wakeCommandEnv(base []string, root string, owner *wakeOwner) ([]string, error) {
	env := scrubWakeChildEnv(base)
	env = setEnvVar(env, envRoot, root)
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
