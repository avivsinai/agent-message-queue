//go:build darwin || linux

package cli

import (
	"bytes"
	"fmt"
)

// reconcileBoundWakePreparedProjectionAt closes the one recoverable P2b
// publication gap: legacy prepared committed for this exact bound generation,
// while the supported canonical document still has only an absent or stale
// prepared projection. The caller holds the retained directory's lifecycle
// guard. Every other divergence remains for the existing fail-closed gate.
func reconcileBoundWakePreparedProjectionAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	bound, err := wakeLockInspectionStateBound(inspection)
	if err != nil {
		return newWakeStateBoundInconclusiveError(err)
	}
	if !bound {
		return nil
	}
	current := inspectWakeLockAt(dirfd, agentDir, inspection.Root, inspection.Agent)
	if !sameWakeLockGeneration(inspection, current) {
		return newWakeStateBoundInconclusiveError(
			newWakeSnapshotReadChangedError(fmt.Errorf("wake lock changed before prepared state reconciliation")),
		)
	}
	legacy, preparedErr, legacyErr := readWakeStateLegacyPairAt(
		dirfd,
		agentDir,
		inspection.Root,
		inspection.Agent,
	)
	if legacyErr != nil || preparedErr != nil ||
		!legacy.TargetPresent || !legacy.PreparedPresent {
		return nil
	}
	marker := legacy.Prepared.Marker
	if marker.Schema != wakeReadySchema ||
		marker.Generation != current.Lock.Generation ||
		marker.TargetDigest != current.Lock.TargetDigest ||
		validateWakeTargetMatchesLock(current.Lock, legacy.Target.Target) != nil {
		return nil
	}

	state, stateExists, stateErr := readWakeStateSnapshotAt(dirfd, agentDir)
	if stateErr != nil || !stateExists {
		return nil
	}
	targetOnlyState := state.State
	targetOnlyState.Prepared = nil
	targetOnlyLegacy := legacy
	targetOnlyLegacy.Prepared = wakeGenerationFileSnapshot{}
	targetOnlyLegacy.PreparedPresent = false
	if err := validateWakeStateAgainstLegacy(targetOnlyState, targetOnlyLegacy.legacy()); err != nil {
		return nil
	}
	if state.State.Target.TargetDigest != current.Lock.TargetDigest ||
		state.State.Target.TargetDigest != current.Lock.StateDigest {
		return nil
	}
	switch classifyWakeStatePrepared(
		state.State.Prepared,
		current.Lock.Generation,
		current.Lock.TargetDigest,
	) {
	case wakeStatePreparedAbsent, wakeStatePreparedStale:
	default:
		return nil
	}

	_, err = publishWakeStateValidatedAt(
		dirfd,
		agentDir,
		inspection.Root,
		inspection.Agent,
		legacy,
		func() error {
			confirmed := inspectWakeLockAt(
				dirfd,
				agentDir,
				inspection.Root,
				inspection.Agent,
			)
			if !sameWakeLockGeneration(current, confirmed) {
				return newWakeSnapshotReadChangedError(
					fmt.Errorf("wake lock changed before prepared state install"),
				)
			}
			currentState, exists, err := readWakeStateRawSnapshotAt(dirfd, agentDir)
			if err != nil {
				return err
			}
			if !exists || state.FileInfo == nil || currentState.FileInfo == nil ||
				!sameWakeFileIdentity(state.FileInfo, currentState.FileInfo) ||
				!bytes.Equal(state.Raw, currentState.Raw) {
				return newWakeSnapshotReadChangedError(
					fmt.Errorf("wake state changed before prepared state install"),
				)
			}
			return nil
		},
	)
	if err != nil {
		return newWakeStateBoundInconclusiveError(err)
	}
	confirmed := inspectWakeLockAt(dirfd, agentDir, inspection.Root, inspection.Agent)
	if !sameWakeLockGeneration(current, confirmed) {
		return newWakeStateBoundInconclusiveError(
			newWakeSnapshotReadChangedError(fmt.Errorf("wake lock changed during prepared state reconciliation")),
		)
	}
	return nil
}

// validateBoundWakeMutationAt is the fail-closed gate for lifecycle mutations
// derived from an exact lock observation. It may write the prepared projection
// to reconcile a committed legacy marker, so callers must already be in a
// mutating context and hold this retained directory's lifecycle guard. Unbound
// P2a claims retain their legacy behavior; a bound claim must complete the same
// state selection and closing lock reinspection as read-side decisions before
// it can authorize a stop, release, recovery, or rollback mutation.
//
// Some lifecycle paths intentionally invoke this gate more than once. Each
// invocation re-reads the legacy pair and state snapshot; PR2 will consolidate
// that accepted correctness-first cost around retained wake authority.
func validateBoundWakeMutationAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	bound, err := wakeLockInspectionStateBound(inspection)
	if err != nil {
		return newWakeStateBoundInconclusiveError(err)
	}
	if !bound {
		return nil
	}
	if err := reconcileBoundWakePreparedProjectionAt(dirfd, agentDir, inspection); err != nil {
		return err
	}
	_, err = readWakeStateSelectionForInspectionAt(
		dirfd,
		agentDir,
		inspection.Root,
		inspection.Agent,
		inspection,
	)
	return err
}
