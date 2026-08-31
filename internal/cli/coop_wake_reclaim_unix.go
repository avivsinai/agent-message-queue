//go:build darwin || linux

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/term"
)

func prepareCoopWakeLock(root, agent string, yes bool, remedy string) error {
	agentDir, err := openExistingCoopWakeAgentDir(root, agent)
	if err != nil {
		return err
	}
	if agentDir == nil {
		return nil
	}
	defer func() { _ = agentDir.Close() }()

	var inspection wakeLockInspection
	if err := agentDir.withFD(func(dirfd int) error {
		inspection = inspectWakeLockAt(dirfd, agentDir, root, agent)
		return nil
	}); err != nil {
		return err
	}
	if !inspection.Exists {
		return nil
	}
	// A live process that affirmative process inspection proves is not this wake
	// must never be signaled or have its lock removed by a coop retry.
	if inspection.Process.Running && wakeProcessProvenNotWake(inspection.Process) {
		return fmt.Errorf("wake lock for %s names a running process proven not to be this wake; refusing to signal or remove it; lock: %s; root: %s; reason: %s", agent, inspection.LockPath, inspection.Root, inspection.Reason)
	}
	if inspection.Status == wakeLockStale {
		// This returns before stale owner-bound recovery advice is rendered below.
		if err := removeCoopWakeLockIfUnchangedInDir(agentDir, inspection); err != nil {
			return fmt.Errorf("remove exact stale wake lock: %w", err)
		}
		return nil
	}

	// Wake acquisition already performs exact-identity retirement for this
	// designed automatic case. Do not turn it into an interactive preflight.
	if isLiveRawOrphan(inspection) &&
		wakeLockNeedsReplacement(inspection) {
		return nil
	}

	if isLiveRawOrphan(inspection) {
		terminal := wakeLockTerminalAttachment(inspection)
		state := coopWakeTerminalState(terminal)
		proceed, err := confirmCoopBadWake(
			inspection,
			state,
			coopWakeTTYDisplay(inspection),
			yes,
			coopWakeRemedy(inspection, state, remedy),
		)
		if err != nil || !proceed {
			return err
		}
		retired, err := terminateAndRemoveOrphanedWakeLockInDirWithRawConsent(agentDir, inspection, true, nil)
		if err != nil {
			return fmt.Errorf("retire identity-confirmed live raw orphan: %w", err)
		}
		if !retired {
			return fmt.Errorf("wake changed before live raw orphan retirement; retry")
		}
		if terminal != wakeTerminalGone {
			_ = writeStderr(
				"warning: took over blocking wake for %s (pid %d on %s): %s\n",
				agent,
				inspection.PID,
				coopWakeTTYDisplay(inspection),
				state,
			)
		}
		return nil
	}

	if confirmedLiveWake(inspection) {
		switch classifyPersistedWakeClaim(inspection) {
		case wakeClaimAuthoritative:
			if inspection.Lock.Owner == nil {
				return coopWakeStartupConflictError(
					inspection,
					errors.New("authoritative wake owner is missing"),
				)
			}
			ownerObservation, err := observeAuthoritativeWakeOwner(*inspection.Lock.Owner)
			if err != nil {
				return coopWakeStartupConflictError(
					inspection,
					errors.Join(err, ownerObservation.Close()),
				)
			}
			ownerState := ownerObservation.State
			if err := ownerObservation.Close(); err != nil {
				return coopWakeStartupConflictError(inspection, err)
			}
			if ownerState == wakeOwnerDead {
				break
			}
			// Owner-bound claims belong to a specific coop session. A new
			// session cannot reuse or replace one while its wake is live.
			return coopWakeStartupConflictError(inspection, nil)
		case wakeClaimGeneric:
			replaceNeeded, err := wakeLockReplacementNeeded(inspection)
			if err != nil {
				return err
			}
			if !replaceNeeded {
				return coopWakeStartupConflictError(inspection, nil)
			}
		}
	}

	if inspection.Status != wakeLockUnverified {
		// Valid non-orphans and creating locks retain their existing fail-closed
		// behavior in the wake acquisition path.
		return nil
	}
	state := "cannot confirm whether it is running"
	proceed, err := confirmCoopBadWake(
		inspection,
		state,
		coopWakeTTYDisplay(inspection),
		yes,
		coopWakeRemedy(inspection, state, remedy),
	)
	if err != nil || !proceed {
		return err
	}

	if err := removeCoopWakeLockIfUnchangedInDir(agentDir, inspection); err != nil {
		return fmt.Errorf("remove exact unverified wake lock: %w", err)
	}
	_ = writeStderr(
		"warning: superseded unidentified wake helper for %s (pid %d on %s) without signaling it; fresh wake is starting, but duplicate notifications may continue until the old helper exits; stop that helper if duplicates persist\n",
		agent,
		inspection.PID,
		coopWakeTTYDisplay(inspection),
	)
	return nil
}

func openExistingCoopWakeAgentDir(root, agent string) (*wakeAgentDir, error) {
	agentDir, err := openWakeDirectory(fsq.AgentBase(root, agent), "wake agent directory")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return agentDir, err
}

func removeCoopWakeLockIfUnchangedInDir(
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
) error {
	// The retained descriptor is the authority for this exact cleanup. This
	// may create the permanent guard in that retained directory, never through
	// the canonical successor pathname.
	return withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
		if !sameWakeLockGeneration(expected, current) {
			return nil
		}
		return removeWakeLockIfUnchangedGuardedAt(scope, current)
	})
}

func sameWakeLockGenerationForRetainedTermination(
	first, second wakeLockInspection,
) bool {
	if sameWakeLockGeneration(first, second) {
		return true
	}
	return first.Exists && second.Exists &&
		first.fileInfo != nil && second.fileInfo != nil &&
		os.SameFile(first.fileInfo, second.fileInfo) &&
		bytes.Equal(first.raw, second.raw) &&
		(first.Lock.Generation == "" || second.Lock.Generation == "" ||
			first.Lock.Generation == second.Lock.Generation)
}

// Termination cleanup may hold a directory that is now detached from the
// canonical mailbox. Validate the retained generation, but never use that
// capability to select a replacement or publish success.
func validateWakeLockOwnerlessMutationAtForTermination(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	bound, err := wakeLockInspectionStateBound(inspection)
	if err != nil {
		return newWakeStateBoundInconclusiveError(err)
	}
	relation, err := retainedWakeAgentDirRelation(agentDir)
	if err != nil {
		return newWakeStateBoundInconclusiveError(err)
	}
	switch relation {
	case wakeAgentDirCanonical, wakeAgentDirDetached:
	case wakeAgentDirInconclusive:
		return newWakeStateBoundInconclusiveError(
			fmt.Errorf("wake agent directory relation is inconclusive before ownerless mutation"),
		)
	default:
		return newWakeStateBoundInconclusiveError(
			fmt.Errorf("unknown wake agent directory relation %d", relation),
		)
	}
	if bound && relation == wakeAgentDirDetached {
		if err := validateWakeLockOwnerlessBoundStateInRetainedDirAt(
			dirfd, agentDir, inspection,
		); err != nil {
			return err
		}
	} else if relation == wakeAgentDirDetached {
		if _, err := readWakeStateSelectionAtRetained(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
		); err != nil {
			return err
		}
	} else if _, err := readWakeStateSelectionForInspectionAt(
		dirfd,
		agentDir,
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
		target, exists, err := readWakeTargetAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
		)
		if err != nil {
			return fmt.Errorf("wake target is unverified before ownerless mutation: %w", err)
		}
		if exists && target.Owner != nil {
			return fmt.Errorf("owner-bearing wake state requires 'amq wake recover-owner --me %s'", inspection.Agent)
		}
	}
	return nil
}

func validateWakeLockStaleRemovalAtForTermination(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	relation, err := retainedWakeAgentDirRelation(agentDir)
	if err != nil {
		return newWakeStateBoundInconclusiveError(err)
	}
	switch relation {
	case wakeAgentDirCanonical:
		if _, err := readWakeStateSelectionForInspectionAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
			inspection,
		); err != nil {
			return err
		}
	case wakeAgentDirDetached:
	case wakeAgentDirInconclusive:
		return newWakeStateBoundInconclusiveError(
			fmt.Errorf("wake agent directory relation is inconclusive before stale removal"),
		)
	default:
		return newWakeStateBoundInconclusiveError(
			fmt.Errorf("unknown wake agent directory relation %d", relation),
		)
	}
	if wakeLockHasOwnerMarkers(inspection) {
		return fmt.Errorf("owner-bound wake claims require 'amq wake recover-owner --me %s'", inspection.Agent)
	}
	if err := validateWakeLockRepairable(inspection); err == nil {
		return nil
	} else if inspection.Status != wakeLockStale {
		return err
	}
	return nil
}

func confirmCoopBadWake(
	inspection wakeLockInspection,
	state string,
	tty string,
	yes bool,
	remedy string,
) (bool, error) {
	_ = writeStderr(
		"wake for %s is blocking startup and cannot notify you\n"+
			"  lock:    %s\n"+
			"  pid:     %d\n"+
			"  tty:     %s\n"+
			"  age:     %s\n"+
			"  state:   %s\n"+
			"  root:    %s\n",
		inspection.Agent,
		inspection.LockPath,
		inspection.PID,
		tty,
		formatCoopWakeLockAge(inspection.Lock.Started, time.Now()),
		state,
		inspection.Root,
	)
	ok, err := consentToClearWake(yes, remedy)
	if err != nil {
		return false, fmt.Errorf("confirm wake cleanup: %w", err)
	}
	if !ok {
		return false, errors.New("wake cleanup declined")
	}
	return true, nil
}

// consentToClearWake reports whether the caller has authorised clearing a
// blocking wake. Headless callers must pass -y; a prompt nobody can answer
// is never shown, and never silently answered.
func consentToClearWake(yes bool, remedy string) (bool, error) {
	if yes {
		return true, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		_ = writeStderr("amq coop exec: %s\n", remedy)
		return false, nil
	}
	return confirmPrompt("Clear it and start a fresh wake?")
}

func coopWakeTerminalState(terminal wakeTerminalAttachment) string {
	switch terminal {
	case wakeTerminalGone:
		return "running, but its terminal is gone"
	case wakeTerminalAttached:
		return "running, and still attached to a terminal — this may be a live session in another window"
	default:
		return "running; cannot determine whether its terminal is still attached"
	}
}

func coopWakeTTYDisplay(inspection wakeLockInspection) string {
	tty := strings.TrimSpace(inspection.Lock.TTY)
	if tty == "" {
		return "unknown"
	}
	return tty
}

// resolveMissingWakeLockAfterTermination handles the irreversible half of a
// takeover: the exact old helper's lock disappeared during termination, but
// the process path could not prove that the helper exited. With no lock left to
// preserve, proceeding is safer than returning a false retry; warn that the old
// helper may still emit duplicate notifications.
func resolveMissingWakeLockAfterTermination(
	inspection wakeLockInspection,
	terminationErr error,
) (bool, error) {
	agentDir, err := openExistingCoopWakeAgentDir(inspection.Root, inspection.Agent)
	if err != nil {
		return false, err
	}
	if agentDir == nil {
		return resolveMissingWakeLockAfterTerminationFromInspection(
			inspection,
			wakeLockInspection{},
			terminationErr,
		)
	}
	defer func() { _ = agentDir.Close() }()
	return resolveMissingWakeLockAfterTerminationInDir(agentDir, inspection, terminationErr)
}

func resolveMissingWakeLockAfterTerminationInDir(
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	terminationErr error,
) (bool, error) {
	var current wakeLockInspection
	if err := agentDir.withFD(func(dirfd int) error {
		if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
			return err
		}
		current = inspectWakeLockAt(dirfd, agentDir, inspection.Root, inspection.Agent)
		return validateWakeStateAgentDirAt(dirfd, agentDir)
	}); err != nil {
		relation, relationErr := retainedWakeAgentDirRelation(agentDir)
		if relationErr != nil {
			return false, errors.Join(err, relationErr)
		}
		switch relation {
		case wakeAgentDirDetached:
			return false, nil
		case wakeAgentDirCanonical:
			return false, err
		case wakeAgentDirInconclusive:
			return false, errors.Join(err, fmt.Errorf("wake agent directory relation is inconclusive after termination"))
		default:
			return false, errors.Join(err, fmt.Errorf("unknown wake agent directory relation %d", relation))
		}
	}
	return resolveMissingWakeLockAfterTerminationFromInspection(inspection, current, terminationErr)
}

func resolveMissingWakeLockAfterTerminationFromInspection(
	inspection wakeLockInspection,
	current wakeLockInspection,
	terminationErr error,
) (bool, error) {
	if current.Exists && !sameWakeLockGeneration(inspection, current) {
		return false, nil
	}
	if current.Exists {
		return false, terminationErr
	}
	// This PID-based inspection only decides whether to print the warning. Both
	// outcomes proceed identically, so PID reuse cannot authorize an action.
	if inspectWakeIdentity(inspection) != wakeIdentityGoneOrDifferent {
		_ = writeStderr(
			"warning: superseded wake helper for %s (pid %d on %s) after its lock disappeared during termination without a confirmed exit; fresh wake is starting, but duplicate notifications may continue until the old helper exits; stop that helper if duplicates persist\n",
			inspection.Agent,
			inspection.PID,
			coopWakeTTYDisplay(inspection),
		)
	}
	return true, nil
}

func coopWakeRemedy(inspection wakeLockInspection, state, command string) string {
	return fmt.Sprintf("wake for %s is blocking startup and cannot notify you.\n  lock: %s\n  state: %s\n\nRe-run with -y to clear it and start a fresh wake:\n  %s\n\nTo inspect first:\n  %s", inspection.Agent, inspection.LockPath, state, command, doctorRootCommandForOS(inspection.Root, "", runtime.GOOS, "--ops"))
}

func coopWakeRemedyForCommand(root, agent, command string, commandArgs []string) string {
	quoted := []string{"amq", "coop", "exec", "-y", "--root", root, "--me", agent, command}
	quoted = append(quoted, commandArgs...)
	parts := make([]string, 0, len(quoted))
	for _, arg := range quoted {
		parts = append(parts, shellQuoteArg(arg))
	}
	return strings.Join(parts, " ")
}

func coopWakeStartupConflictError(inspection wakeLockInspection, cause error) error {
	var message string
	switch {
	case confirmedLiveWake(inspection):
		started := strings.TrimSpace(inspection.Lock.Started)
		if started == "" {
			started = "unknown"
		}
		message = fmt.Sprintf(
			"wake for %s is owned by a live process\n"+
				"  pid:     %d\n"+
				"  tty:     %s\n"+
				"  started: %s\n"+
				"  root:    %s\n\n"+
				"No AMQ command can safely take over this live wake; use that terminal, "+
				"or stop process %d and retry amq coop exec.",
			inspection.Agent,
			inspection.PID,
			coopWakeTTYDisplay(inspection),
			started,
			inspection.Root,
			inspection.PID,
		)
	case inspection.Status == wakeLockStale:
		repair := doctorRootCommandForOS(
			inspection.Root,
			"",
			runtime.GOOS,
			"--ops",
			"--fix-wake-locks",
		)
		action := "Remove only the proven-stale session lock, then retry:\n  " + repair
		if wakeLockHasOwnerMarkers(inspection) {
			action = "Recover the exact owner-bound claim, then retry:\n  " +
				wakeRecoverOwnerCommand(inspection.Root, inspection.Agent)
		}
		message = fmt.Sprintf(
			"a proven-stale wake lock is blocking startup for %s\n"+
				"  lock:   %s\n"+
				"  pid:    %d\n"+
				"  reason: %s\n"+
				"  root:   %s\n\n%s",
			inspection.Agent,
			inspection.LockPath,
			inspection.PID,
			inspection.Reason,
			inspection.Root,
			action,
		)
	case inspection.Exists:
		message = fmt.Sprintf(
			"wake state for %s could not be verified and was preserved\n"+
				"  lock:   %s\n"+
				"  pid:    %d\n"+
				"  reason: %s\n"+
				"  root:   %s\n\n"+
				"Inspect the exact session before changing it:\n  %s",
			inspection.Agent,
			inspection.LockPath,
			inspection.PID,
			inspection.Reason,
			inspection.Root,
			doctorRootCommandForOS(inspection.Root, "", runtime.GOOS, "--ops"),
		)
	}
	if message == "" {
		if cause == nil {
			return errors.New("wake startup conflict could not be classified")
		}
		return cause
	}
	if cause == nil {
		return errors.New(message)
	}
	return fmt.Errorf("%s\nstartup detail: %w", message, cause)
}

func formatCoopWakeLockAge(started string, now time.Time) string {
	startedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(started))
	if err != nil || startedAt.After(now) {
		return "unknown"
	}
	age := now.Sub(startedAt)
	switch {
	case age >= 24*time.Hour:
		days := int(age / (24 * time.Hour))
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	case age >= time.Hour:
		hours := int(age / time.Hour)
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	case age >= time.Minute:
		minutes := int(age / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	default:
		return "less than a minute"
	}
}
