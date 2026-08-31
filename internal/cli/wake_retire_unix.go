//go:build darwin || linux

package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/cli/wakemutation"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

var afterWakeRetireLockRemoval = func() {}
var afterWakeRetireArtifactSnapshot = func() {}
var afterWakeRetireValidation = func() {}

var wakeRetireUnlinkStateAt wakemutation.UnlinkAtFunc = wakeUnlinkAt

type wakeRetireArtifactSnapshot struct {
	Target             wakeTargetSnapshot
	State              wakeStateFileSnapshot
	StateWasPresent    bool
	StateMatchesTarget bool
	StateSnapshotErr   error
}

type wakeRetireCleanupOutcome struct {
	Residue []string
	Err     error
}

type wakeRetireResult struct {
	Status string `json:"status"`
	Agent  string `json:"agent"`
	Root   string `json:"root"`
	Lock   string `json:"lock"`
	Target string `json:"target,omitempty"`
	PID    int    `json:"pid,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func runWakeRetire(args []string) error {
	fs := flag.NewFlagSet("wake retire", flag.ContinueOnError)
	common := addCommonFlags(fs)
	injectViaFlag := fs.String("inject-via", "", "Expected external injection executable")
	ifGenerationFlag := fs.String("if-generation", "", "Retire only if the current lock generation still matches this exact value")
	retryUntilFlag := fs.String("retry-until", wakeRetryUntilDrained, "Expected doorbell acknowledgement: drained or injected")
	var injectArgFlags multiStringFlag
	fs.Var(&injectArgFlags, "inject-arg", "Expected fixed injection argument (repeatable)")
	usage := usageWithFlags(fs, "amq wake retire --me <agent> --inject-via <path> [options]",
		"Stop an identity-confirmed live inject-via wake or remove its exactly-bound proven-stale lock.",
		"",
		"The expected executable and ordered arguments must exactly match the saved target.",
		"Pass --if-generation with the generation from amq wake check so a replacement published after that check is refused.",
		"Retirement preserves the mailbox, removes the exact saved target and coupled state projection, and never stops raw wakes.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	if strings.TrimSpace(*injectViaFlag) == "" {
		return UsageError("--inject-via is required")
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	root := resolveRoot(common.Root)
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	requested, err := newWakeTarget(root, me, *injectViaFlag, []string(injectArgFlags))
	if err != nil {
		return UsageError("--inject-via: %v", err)
	}
	retryUntil, err := normalizeWakeRetryUntil(*retryUntilFlag)
	if err != nil {
		return UsageError("%v", err)
	}
	if retryUntil == wakeRetryUntilInjected {
		requested.RetryUntil = retryUntil
	}
	result, retireErr := retireWakeIfGeneration(root, me, requested, strings.TrimSpace(*ifGenerationFlag))
	if common.JSON {
		if err := writeJSON(os.Stdout, result); err != nil {
			return err
		}
		return retireErr
	}
	line := fmt.Sprintf("wake retire: %s agent=%s root=%s", result.Status, result.Agent, result.Root)
	if result.PID != 0 {
		line += fmt.Sprintf(" pid=%d", result.PID)
	}
	if result.Reason != "" {
		line += " reason=" + result.Reason
	}
	if err := writeStdoutLine(line); err != nil {
		return err
	}
	return retireErr
}

func retireWake(root, me string, requested wakeTarget) (wakeRetireResult, error) {
	return retireWakeIfGeneration(root, me, requested, "")
}

func retireWakeIfGeneration(root, me string, requested wakeTarget, ifGeneration string) (wakeRetireResult, error) {
	result := wakeRetireResult{
		Status: "refused",
		Agent:  me,
		Root:   canonicalWakeRoot(root),
		Lock:   filepath.Join(fsq.AgentBase(root, me), wakeLockFileName),
		Target: wakeTargetPath(root, me),
	}
	refuse := func(reason string) (wakeRetireResult, error) {
		result.Status = "refused"
		err := withWakeDiagnostic(errors.New(reason), result.Root, result.Agent)
		result.Reason = reason
		return result, err
	}
	inspection := inspectWakeLock(root, me)
	if !inspection.Exists {
		return refuse("no wake lock present; wake process absence cannot be proven")
	}
	result.PID = inspection.PID
	if ifGeneration != "" && inspection.Lock.Generation != ifGeneration {
		return refuse("wake generation changed before retirement")
	}
	if wakeLockHasOwnerMarkers(inspection) {
		return refuse(fmt.Sprintf("owner-bound wake claims require %s", wakeRecoverOwnerCommand(root, me)))
	}
	agentDir, err := openExistingCoopWakeAgentDir(root, me)
	if err != nil {
		return refuse(err.Error())
	}
	if agentDir == nil {
		return refuse("wake agent directory disappeared before retirement")
	}
	defer func() { _ = agentDir.Close() }()
	if inspection.Status == wakeLockStale ||
		(inspection.Status == wakeLockValid && inspection.Lock.WakeMode == wakeTargetInjectVia) {
		guardMissing, guardProbeErr := wakeRetireLifecycleGuardMissingAt(agentDir)
		if guardProbeErr == nil && guardMissing {
			if err := withWakeMutationScopeNoWaitInDir(agentDir, func(scope *wakeMutationScope) error {
				_, err := snapshotWakeRetireArtifactsAt(scope, inspection, requested)
				return err
			}); err != nil {
				return refuse(err.Error())
			}
		}
	}

	switch inspection.Status {
	case wakeLockValid:
		if !inspection.IdentityConfirmed {
			return refuse("wake process identity is not confirmed")
		}
		if inspection.Lock.WakeMode != wakeTargetInjectVia {
			return refuse("live raw wake retirement is owned by its terminal or supervisor")
		}

		var confirmed wakeLockInspection
		var artifacts wakeRetireArtifactSnapshot
		if err := withExistingWakeMutationScopeNoWaitInDir(agentDir, func(scope *wakeMutationScope) error {
			dirfd, scopedAgentDir, err := scope.location()
			if err != nil {
				return err
			}
			agentDir = scopedAgentDir
			confirmed = inspectWakeLockAt(dirfd, agentDir, root, me)
			if !sameWakeLockInspection(inspection, confirmed) || !confirmed.IdentityConfirmed {
				return errors.New("wake lock changed before retirement")
			}
			if ifGeneration != "" && confirmed.Lock.Generation != ifGeneration {
				return errors.New("wake generation changed before retirement")
			}
			var snapshotErr error
			artifacts, snapshotErr = snapshotWakeRetireArtifactsAt(scope, confirmed, requested)
			return snapshotErr
		}); err != nil {
			return refuse(err.Error())
		}

		afterWakeRetireValidation()
		var commit wakeLockRemovalOutcome
		commit.Committed, commit.Err = terminateAndRemoveOrphanedWakeLockInDirWithRawConsent(
			agentDir,
			confirmed,
			false,
			&requested,
		)
		if !commit.Committed {
			if commit.Err != nil {
				return refuse(commit.Err.Error())
			}
			return refuse("wake lock or process identity changed before retirement")
		}
		afterWakeRetireLockRemoval()
		cleanup := preserveWakeRetireArtifacts(artifacts, nil)
		if commit.Err == nil {
			cleanup = wakeRetireCleanupOutcome{}
			if err := withExistingWakeMutationScopeNoWaitInDir(agentDir, func(scope *wakeMutationScope) error {
				cleanup = removeWakeRetireArtifactsAt(scope, confirmed, artifacts)
				return nil
			}); err != nil {
				cleanup = preserveWakeRetireArtifacts(artifacts, err)
			}
		}
		return finishWakeRetirement(
			result,
			"live inject-via wake stopped; mailbox preserved",
			cleanup,
			commit,
		)

	case wakeLockStale:
		var commit wakeLockRemovalOutcome
		var cleanup wakeRetireCleanupOutcome
		var artifacts wakeRetireArtifactSnapshot
		err := withExistingWakeMutationScopeNoWaitInDir(agentDir, func(scope *wakeMutationScope) error {
			dirfd, scopedAgentDir, err := scope.location()
			if err != nil {
				return err
			}
			current := inspectWakeLockAt(dirfd, scopedAgentDir, root, me)
			if !sameWakeLockGeneration(inspection, current) || current.Status != wakeLockStale {
				return errors.New("wake lock changed before retirement")
			}
			if ifGeneration != "" && current.Lock.Generation != ifGeneration {
				return errors.New("wake generation changed before retirement")
			}
			if err := validateWakeLockStaleRemovalAt(dirfd, scopedAgentDir, current); err != nil {
				return err
			}
			artifacts, err = snapshotWakeRetireArtifactsAt(scope, current, requested)
			if err != nil {
				return err
			}
			afterWakeRetireValidation()
			if err := requireExistingWakeTargetMatchesAt(dirfd, scopedAgentDir, current, requested); err != nil {
				return err
			}
			commit = removeWakeLockIfUnchangedGuardedAtDurableOutcome(
				scope,
				current,
				scope.unlinkWakeLockForRetire,
			)
			if !commit.Committed {
				return commit.Err
			}
			afterWakeRetireLockRemoval()
			if commit.Err != nil {
				cleanup = preserveWakeRetireArtifacts(artifacts, nil)
				return nil
			}
			cleanup = removeWakeRetireArtifactsAt(scope, current, artifacts)
			return nil
		})
		if !commit.Committed {
			if err == nil {
				err = errors.New("wake lock disappeared before exact retirement commit")
			}
			return refuse(err.Error())
		}
		return finishWakeRetirement(
			result,
			"exactly-bound proven-stale wake lock removed; mailbox preserved",
			cleanup,
			commit,
		)

	case wakeLockCreating:
		return refuse("wake lock is being created; retry shortly")
	case wakeLockUnverified:
		return refuse("wake lock is unverified; refusing retirement: " + inspection.Reason)
	default:
		return refuse(fmt.Sprintf("wake lock status %q cannot be retired", inspection.Status))
	}
}

func snapshotWakeRetireArtifactsAt(
	scope *wakeMutationScope,
	inspection wakeLockInspection,
	requested wakeTarget,
) (wakeRetireArtifactSnapshot, error) {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return wakeRetireArtifactSnapshot{}, err
	}
	bound, err := wakeLockInspectionStateBound(inspection)
	if err != nil {
		return wakeRetireArtifactSnapshot{}, newWakeStateBoundInconclusiveError(err)
	}
	if bound {
		if err := validateBoundWakeMutationAt(scope, inspection); err != nil {
			return wakeRetireArtifactSnapshot{}, err
		}
	}
	persisted, exists, err := readWakeTargetSnapshotAt(
		dirfd,
		agentDir,
		inspection.Root,
		inspection.Agent,
	)
	if err != nil {
		return wakeRetireArtifactSnapshot{}, err
	}
	if !exists {
		return wakeRetireArtifactSnapshot{}, withWakeDiagnostic(
			errors.New("no saved inject-via wake target; refusing retirement"),
			inspection.Root,
			inspection.Agent,
		)
	}
	if persisted.Target.Owner != nil {
		return wakeRetireArtifactSnapshot{}, fmt.Errorf("owner-bearing wake state requires %s", wakeRecoverOwnerCommand(inspection.Root, inspection.Agent))
	}
	if err := validateWakeTarget(persisted.Target, inspection.Root, inspection.Agent); err != nil {
		return wakeRetireArtifactSnapshot{}, err
	}
	if err := validateWakeTargetMatchesLock(inspection.Lock, persisted.Target); err != nil {
		return wakeRetireArtifactSnapshot{}, err
	}
	if !sameWakeInjectorIdentity(persisted.Target, requested) {
		return wakeRetireArtifactSnapshot{}, errors.New("saved wake target uses a different injector identity or retry acknowledgement policy")
	}
	artifacts := wakeRetireArtifactSnapshot{Target: persisted}
	state, stateExists, stateErr := readWakeStateRawSnapshotAt(dirfd, agentDir)
	artifacts.State = state
	artifacts.StateWasPresent = stateExists
	if stateErr != nil {
		if bound {
			return wakeRetireArtifactSnapshot{}, fmt.Errorf("snapshot bound wake state before retirement: %w", stateErr)
		}
		artifacts.StateSnapshotErr = stateErr
		return artifacts, nil
	}
	if !stateExists {
		if bound {
			return wakeRetireArtifactSnapshot{}, errors.New("bound wake state is missing")
		}
		return artifacts, nil
	}
	decoded, decodeErr := decodeWakeState(state.Raw)
	if decodeErr != nil {
		if bound {
			return wakeRetireArtifactSnapshot{}, fmt.Errorf("snapshot bound wake state before retirement: %w", decodeErr)
		}
		artifacts.StateSnapshotErr = decodeErr
		return artifacts, nil
	}
	targetDigest, err := wakeTargetDigest(persisted.Target)
	if err != nil {
		return wakeRetireArtifactSnapshot{}, err
	}
	artifacts.StateMatchesTarget = decoded.Target.TargetDigest == targetDigest
	if bound && !artifacts.StateMatchesTarget {
		return wakeRetireArtifactSnapshot{}, errors.New("bound wake state target digest does not match retired target")
	}
	return artifacts, nil
}

func removeWakeRetireArtifactsAt(
	scope *wakeMutationScope,
	retired wakeLockInspection,
	expected wakeRetireArtifactSnapshot,
) wakeRetireCleanupOutcome {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return preserveWakeRetireArtifacts(expected, err)
	}
	if err := validateWakeStateRetainedAgentDirAt(dirfd, agentDir); err != nil {
		return preserveWakeRetireArtifacts(expected, err)
	}
	if err := validateWakeRetireCleanupSnapshotAt(
		dirfd, agentDir, retired, expected, "after retirement",
	); err != nil {
		return preserveWakeRetireArtifacts(expected, err)
	}
	afterWakeRetireArtifactSnapshot()
	if err := validateWakeRetireCleanupSnapshotAt(
		dirfd, agentDir, retired, expected, "during retired artifact cleanup",
	); err != nil {
		return preserveWakeRetireArtifacts(expected, err)
	}
	removedTarget, err := removeWakeTargetIfSnapshotMatchesAt(
		scope,
		retired.Root,
		retired.Agent,
		expected.Target,
	)
	if err != nil {
		return preserveWakeRetireArtifacts(expected, err)
	}
	if !removedTarget {
		return preserveWakeRetireArtifacts(
			expected,
			errors.New("retired wake target disappeared before exact cleanup"),
		)
	}
	if expected.StateWasPresent && expected.StateMatchesTarget {
		removedState, err := removeWakeRetireStateIfSnapshotMatchesAt(
			scope, expected.State,
		)
		if err != nil {
			return wakeRetireCleanupOutcome{Residue: []string{wakeStateFileName}, Err: err}
		}
		if !removedState {
			return wakeRetireCleanupOutcome{
				Residue: []string{wakeStateFileName},
				Err:     errors.New("corresponding wake state disappeared before exact cleanup"),
			}
		}
	} else if expected.StateWasPresent {
		return wakeRetireCleanupOutcome{
			Residue: []string{wakeStateFileName},
			Err:     expected.StateSnapshotErr,
		}
	}
	if err := removeWakeSelfUpgradeAttemptAt(dirfd); err != nil {
		return wakeRetireCleanupOutcome{
			Residue: []string{wakeSelfUpgradeAttemptFileName},
			Err:     fmt.Errorf("remove retired wake self-upgrade attempt: %w", err),
		}
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return wakeRetireCleanupOutcome{
			Residue: []string{"wake artifact durability"},
			Err:     fmt.Errorf("sync retired wake artifact removal: %w", err),
		}
	}
	return wakeRetireCleanupOutcome{}
}

func validateWakeRetireCleanupSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
	retired wakeLockInspection,
	expected wakeRetireArtifactSnapshot,
	replacementWhen string,
) error {
	if replacement := inspectWakeLockAt(
		dirfd, agentDir, retired.Root, retired.Agent,
	); replacement.Exists {
		return fmt.Errorf(
			"replacement wake lock appeared %s; preserving retirement artifacts",
			replacementWhen,
		)
	}
	return validateWakeRetireArtifactSnapshotsAt(dirfd, agentDir, retired, expected)
}

func validateWakeRetireArtifactSnapshotsAt(
	dirfd int,
	agentDir *wakeAgentDir,
	retired wakeLockInspection,
	expected wakeRetireArtifactSnapshot,
) error {
	currentTarget, exists, err := readWakeTargetSnapshotAt(
		dirfd, agentDir, retired.Root, retired.Agent,
	)
	if err != nil {
		return fmt.Errorf("re-read retired wake target: %w", err)
	}
	if !exists || !sameWakeRetireTargetSnapshot(expected.Target, currentTarget) {
		return errors.New("retired wake target changed before cleanup; preserving retirement artifacts")
	}
	currentState, stateExists, stateErr := readWakeStateRawSnapshotAtWithCanonicalValidation(
		dirfd, agentDir, false,
	)
	if expected.StateWasPresent {
		if expected.State.FileInfo == nil {
			return nil
		}
		if stateErr != nil {
			return fmt.Errorf("re-read retired wake state: %w", stateErr)
		}
		if !stateExists || !sameWakeRetireStateSnapshot(expected.State, currentState) {
			return errors.New("retired wake state changed before cleanup; preserving retirement artifacts")
		}
		return nil
	}
	if stateErr != nil {
		return fmt.Errorf("verify retired wake state absence: %w", stateErr)
	}
	if stateExists {
		return errors.New("wake state appeared after retirement validation; preserving retirement artifacts")
	}
	return nil
}

func removeWakeRetireStateIfSnapshotMatchesAt(
	scope *wakeMutationScope,
	expected wakeStateFileSnapshot,
) (bool, error) {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return false, err
	}
	if err := validateWakeStateRetainedAgentDirAt(dirfd, agentDir); err != nil {
		return false, err
	}
	var targetInfo unix.Stat_t
	if err := unix.Fstatat(dirfd, wakeTargetFileName, &targetInfo, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return false, errors.New("wake target reappeared before state cleanup; preserving wake state")
	} else if err != unix.ENOENT {
		return false, fmt.Errorf("verify wake target absence before state cleanup: %w", err)
	}
	current, exists, err := readWakeStateRawSnapshotAtWithCanonicalValidation(
		dirfd, agentDir, false,
	)
	if err != nil {
		return false, fmt.Errorf("re-read corresponding wake state before removal: %w", err)
	}
	if !exists {
		return false, nil
	}
	if !sameWakeRetireStateSnapshot(expected, current) {
		return false, errors.New("wake state changed before removal; preserving replacement")
	}
	if err := scope.unlinkAtWith(wakeRetireUnlinkStateAt, wakeStateFileName, 0); err != nil {
		if err == unix.ENOENT {
			return false, nil
		}
		return false, fmt.Errorf("remove corresponding wake state: %w", err)
	}
	return true, nil
}

func finishWakeRetirement(
	result wakeRetireResult,
	successReason string,
	cleanup wakeRetireCleanupOutcome,
	commit wakeLockRemovalOutcome,
) (wakeRetireResult, error) {
	residue := append([]string(nil), cleanup.Residue...)
	lockResidues := wakeLockRemovalResiduesFromError(commit.Err)
	if commit.Err != nil && len(lockResidues) == 0 {
		lockResidues = []wakeLockRemovalResidue{wakeLockResidueCleanup}
	}
	for _, lockResidue := range lockResidues {
		residue = appendWakeRetireResidue(residue, string(lockResidue))
	}
	if len(residue) == 0 {
		result.Status = "retired"
		result.Reason = successReason + "; exact saved target and corresponding state removed"
		return result, nil
	}
	result.Status = "retired_with_residue"
	result.Reason = successReason + "; retirement succeeded; preserved residue: " +
		strings.Join(residue, ", ") + "; automatic convergence will run on next acquisition"
	detailErr := errors.Join(cleanup.Err, commit.Err)
	if detailErr != nil {
		result.Reason += "; cleanup detail: " + detailErr.Error()
	}
	return result, nil
}

func preserveWakeRetireArtifacts(
	expected wakeRetireArtifactSnapshot,
	err error,
) wakeRetireCleanupOutcome {
	return wakeRetireCleanupOutcome{Residue: wakeRetireExpectedResidue(expected), Err: err}
}

func wakeRetireExpectedResidue(expected wakeRetireArtifactSnapshot) []string {
	residue := []string{wakeTargetFileName}
	if expected.StateWasPresent {
		residue = append(residue, wakeStateFileName)
	}
	return residue
}

func appendWakeRetireResidue(residue []string, name string) []string {
	for _, existing := range residue {
		if existing == name {
			return residue
		}
	}
	return append(residue, name)
}

func sameWakeRetireTargetSnapshot(first, second wakeTargetSnapshot) bool {
	return first.FileInfo != nil && second.FileInfo != nil &&
		sameWakeFileIdentity(first.FileInfo, second.FileInfo) &&
		bytes.Equal(first.Raw, second.Raw) &&
		sameWakeTarget(first.Target, second.Target)
}

func sameWakeRetireStateSnapshot(first, second wakeStateFileSnapshot) bool {
	return first.FileInfo != nil && second.FileInfo != nil &&
		sameWakeFileIdentity(first.FileInfo, second.FileInfo) &&
		bytes.Equal(first.Raw, second.Raw)
}

func validateWakeLockOwnerlessMutationAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	bound, err := wakeLockInspectionStateBound(inspection)
	if err != nil {
		return newWakeStateBoundInconclusiveError(err)
	}
	relation, err := retainedWakeAgentDirRelationAt(agentDir, dirfd)
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
	if bound {
		if relation == wakeAgentDirDetached {
			return validateWakeLockOwnerlessBoundStateInRetainedDirAt(
				dirfd, agentDir, inspection,
			)
		}
		if _, err := readWakeStateSelectionForInspectionAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
			inspection,
		); err != nil {
			return err
		}
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
			return fmt.Errorf("owner-bearing wake state requires %s", wakeRecoverOwnerCommand(inspection.Root, inspection.Agent))
		}
	}
	return nil
}

func validateWakeLockOwnerlessBoundStateInRetainedDirAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	if agentDir == nil || agentDir.file == nil || dirfd != int(agentDir.file.Fd()) {
		return newWakeStateBoundInconclusiveError(
			fmt.Errorf("bound wake state directory capability is missing"),
		)
	}
	target, targetExists, err := readWakeTargetSnapshotAt(
		dirfd, agentDir, inspection.Root, inspection.Agent,
	)
	if err != nil || !targetExists {
		if err == nil {
			err = errors.New("bound wake target is missing")
		}
		return newWakeStateBoundInconclusiveError(err)
	}
	if err := validateWakeTarget(target.Target, inspection.Root, inspection.Agent); err != nil {
		return newWakeStateBoundInconclusiveError(err)
	}
	prepared, preparedExists, preparedErr := readWakeGenerationFileSnapshotAt(
		dirfd, agentDir, wakePreparedFileName, "wake prepared marker",
	)
	if preparedErr != nil {
		return newWakeStateBoundInconclusiveError(preparedErr)
	}
	stateSnapshot, stateExists, err := readWakeRetainedStateSnapshotAt(dirfd, agentDir)
	if err != nil || !stateExists {
		if err == nil {
			err = errors.New("bound wake state is missing")
		}
		return newWakeStateBoundInconclusiveError(err)
	}
	legacy := wakeStateLegacy{
		Target:    &target.Target,
		TargetRaw: target.Raw,
	}
	if preparedExists {
		legacy.Prepared = &wakeStateLegacyPrepared{
			Schema:       prepared.Marker.Schema,
			Generation:   prepared.Marker.Generation,
			TargetDigest: prepared.Marker.TargetDigest,
		}
		legacy.PreparedRaw = prepared.Raw
	}
	if err := validateWakeStateAgainstLegacy(stateSnapshot.State, legacy); err != nil {
		return newWakeStateBoundInconclusiveError(err)
	}
	if stateSnapshot.State.Target.TargetDigest != inspection.Lock.TargetDigest ||
		stateSnapshot.State.Target.TargetDigest != inspection.Lock.StateDigest {
		return newWakeStateBoundInconclusiveError(
			fmt.Errorf("bound wake state target digest does not match wake lock"),
		)
	}
	confirmed := inspectWakeLockAt(
		dirfd, agentDir, inspection.Root, inspection.Agent,
	)
	if !sameWakeLockGeneration(inspection, confirmed) {
		return newWakeStateBoundInconclusiveError(
			newWakeSnapshotReadChangedError(
				fmt.Errorf("wake lock changed during retained bound state selection"),
			),
		)
	}
	return nil
}

func readWakeTargetForRetainedInspectionAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (wakeTarget, bool, error) {
	bound, err := wakeLockInspectionStateBound(inspection)
	if err != nil {
		return wakeTarget{}, false, newWakeStateBoundInconclusiveError(err)
	}
	relation, err := retainedWakeAgentDirRelationAt(agentDir, dirfd)
	if err != nil {
		return wakeTarget{}, false, newWakeStateBoundInconclusiveError(err)
	}
	switch relation {
	case wakeAgentDirCanonical, wakeAgentDirDetached:
	case wakeAgentDirInconclusive:
		return wakeTarget{}, false, newWakeStateBoundInconclusiveError(
			fmt.Errorf("wake agent directory relation is inconclusive before retained target read"),
		)
	default:
		return wakeTarget{}, false, newWakeStateBoundInconclusiveError(
			fmt.Errorf("unknown wake agent directory relation %d", relation),
		)
	}
	if !bound || relation == wakeAgentDirCanonical {
		return readWakeTargetFromStateForInspectionAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
			inspection,
		)
	}
	if err := validateWakeLockOwnerlessBoundStateInRetainedDirAt(
		dirfd, agentDir, inspection,
	); err != nil {
		return wakeTarget{}, false, err
	}
	target, exists, err := readWakeTargetSnapshotAt(
		dirfd, agentDir, inspection.Root, inspection.Agent,
	)
	return target.Target, exists, err
}

func readWakeRetainedStateSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
) (wakeStateFileSnapshot, bool, error) {
	path := filepath.Join(agentDir.path, wakeStateFileName)
	open := func() (*os.File, error) {
		fd, err := unix.Openat(
			dirfd,
			wakeStateFileName,
			unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0,
		)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	file, err := open()
	if err != nil {
		if err == unix.ENOENT {
			return wakeStateFileSnapshot{}, false, nil
		}
		return wakeStateFileSnapshot{}, true, fmt.Errorf("open retained wake state: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return wakeStateFileSnapshot{}, true, fmt.Errorf("stat retained wake state: %w", err)
	}
	if err := validateWakeStateFileInfo(path, info); err != nil {
		return wakeStateFileSnapshot{}, true, err
	}
	raw, err := readWakeMetadata(file, "wake state", path)
	if err != nil {
		return wakeStateFileSnapshot{}, true, err
	}
	pathFile, err := open()
	if err != nil {
		return wakeStateFileSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("retained wake state changed while reopening: %w", err),
		)
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil || !sameWakeFileIdentity(info, pathInfo) {
		if statErr == nil {
			statErr = errors.New("file identity changed")
		}
		return wakeStateFileSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("retained wake state changed while opening: %w", statErr),
		)
	}
	state, err := decodeWakeState(raw)
	if err != nil {
		return wakeStateFileSnapshot{}, true, err
	}
	return wakeStateFileSnapshot{Raw: bytes.Clone(raw), FileInfo: info, State: state}, true, nil
}

func validateWakeLockStaleRemovalAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	bound, err := wakeLockInspectionStateBound(inspection)
	if err != nil {
		return newWakeStateBoundInconclusiveError(err)
	}
	if bound {
		if _, err := readWakeStateSelectionForInspectionAt(
			dirfd,
			agentDir,
			inspection.Root,
			inspection.Agent,
			inspection,
		); err != nil {
			return err
		}
		if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
			return newWakeStateBoundInconclusiveError(err)
		}
	}
	if wakeLockHasOwnerMarkers(inspection) {
		return fmt.Errorf("owner-bound wake claims require %s", wakeRecoverOwnerCommand(inspection.Root, inspection.Agent))
	}
	if err := validateWakeLockRepairable(inspection); err == nil {
		return nil
	} else if inspection.Status != wakeLockStale {
		return err
	}
	return nil
}

func wakeRetireLifecycleGuardMissingAt(agentDir *wakeAgentDir) (bool, error) {
	return wakeLifecycleGuardMissingAt(agentDir)
}

func requireExistingWakeTargetMatchesAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	requested wakeTarget,
) error {
	persisted, exists, err := readWakeTargetAt(
		dirfd,
		agentDir,
		inspection.Root,
		inspection.Agent,
	)
	if err != nil {
		return err
	}
	if !exists {
		return withWakeDiagnostic(
			errors.New("no saved inject-via wake target; refusing retirement"),
			inspection.Root,
			inspection.Agent,
		)
	}
	if persisted.Owner != nil {
		return fmt.Errorf("owner-bearing wake state requires %s", wakeRecoverOwnerCommand(inspection.Root, inspection.Agent))
	}
	if err := validateWakeTarget(persisted, inspection.Root, inspection.Agent); err != nil {
		return err
	}
	if err := validateWakeTargetMatchesLock(inspection.Lock, persisted); err != nil {
		return err
	}
	if !sameWakeInjectorIdentity(persisted, requested) {
		return errors.New("saved wake target uses a different injector identity or retry acknowledgement policy")
	}
	return nil
}
