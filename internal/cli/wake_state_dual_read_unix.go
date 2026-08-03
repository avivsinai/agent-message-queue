//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// afterWakeStateDualReadDocument is a test seam between the shadow-document
// observation and the closing legacy observation.
var afterWakeStateDualReadDocument = func() {}

// afterWakeStateBoundSelection is a test seam after the state and legacy
// snapshots are stable but before the lock binding is confirmed again.
var afterWakeStateBoundSelection = func() {}

type wakeStateReadSelection struct {
	Target          wakeTarget
	TargetPresent   bool
	Prepared        wakeReady
	PreparedPresent bool
	PreparedErr     error
	StatePreferred  bool
	legacy          wakeStateLegacySnapshot
	stateErr        error
}

// wakeStateBoundInconclusiveError marks a bound claim whose state cannot be
// used for a read-side decision. Callers may retry the complete observation,
// but must not reuse any legacy evidence from this failed read.
type wakeStateBoundInconclusiveError struct {
	err error
}

func (err *wakeStateBoundInconclusiveError) Error() string {
	return err.err.Error()
}

func (err *wakeStateBoundInconclusiveError) Unwrap() error {
	return err.err
}

func newWakeStateBoundInconclusiveError(err error) error {
	if err == nil {
		return nil
	}
	return &wakeStateBoundInconclusiveError{err: err}
}

func readWakeStateSelection(root, me string) (wakeStateReadSelection, error) {
	if err := fsq.ValidateHandle(me); err != nil {
		return wakeStateReadSelection{}, err
	}
	agentDir, err := openWakeDirectory(fsq.AgentBase(root, me), "wake agent directory")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return wakeStateReadSelection{}, nil
		}
		return wakeStateReadSelection{}, err
	}
	defer func() { _ = agentDir.Close() }()
	var selection wakeStateReadSelection
	err = agentDir.withFD(func(dirfd int) error {
		var readErr error
		selection, readErr = readWakeStateSelectionAt(dirfd, agentDir, root, me)
		return readErr
	})
	if err != nil {
		return selection, err
	}
	if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
		return wakeStateReadSelection{}, err
	}
	return selection, nil
}

func readWakeStateSelectionAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
) (wakeStateReadSelection, error) {
	before, beforePreparedErr, err := readWakeStateLegacyPairAt(dirfd, agentDir, root, me)
	if err != nil {
		return wakeStateSelectionFromLegacy(before), err
	}
	state, stateExists, stateErr := readWakeStateSnapshotAt(dirfd, agentDir)
	if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
		return wakeStateReadSelection{}, err
	}
	afterWakeStateDualReadDocument()
	after, afterPreparedErr, err := readWakeStateLegacyPairAt(dirfd, agentDir, root, me)
	if err != nil {
		changed := newWakeSnapshotReadChangedError(
			fmt.Errorf("wake legacy state changed during closing observation: %w", err),
		)
		if after.TargetPresent || !before.TargetPresent {
			return wakeStateSelectionFromLegacy(after), changed
		}
		return wakeStateSelectionFromLegacy(before), changed
	}
	if !sameWakeStateLegacySnapshot(before, after) {
		return wakeStateReadSelection{}, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake legacy state changed during state selection"),
		)
	}

	selection := wakeStateSelectionFromLegacy(after)
	selection.PreparedErr = afterPreparedErr
	selection.stateErr = stateErr
	if stateErr != nil || !stateExists || !after.TargetPresent ||
		beforePreparedErr != nil || afterPreparedErr != nil {
		return selection, nil
	}
	if err := validateWakeStateAgainstLegacy(state.State, after.legacy()); err != nil {
		selection.stateErr = err
		return selection, nil
	}
	selection.Target = state.State.Target.wakeTarget()
	selection.TargetPresent = true
	selection.PreparedPresent = state.State.Prepared != nil
	if state.State.Prepared != nil {
		selection.Prepared = wakeReady{
			Schema:       state.State.Prepared.Schema,
			Generation:   state.State.Prepared.Generation,
			TargetDigest: state.State.Prepared.TargetDigest,
		}
	}
	selection.StatePreferred = true
	return selection, nil
}

// readWakeStateSelectionForInspection applies the state-binding policy to one
// exact lock observation. The P2a selector deliberately remains independent so
// old, unbound locks retain their legacy fallback behavior.
func readWakeStateSelectionForInspection(
	root string,
	me string,
	inspection wakeLockInspection,
) (wakeStateReadSelection, error) {
	if err := fsq.ValidateHandle(me); err != nil {
		return wakeStateReadSelection{}, err
	}
	bound, err := wakeLockInspectionStateBound(inspection)
	if err != nil {
		return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(err)
	}
	agentDir, err := openWakeDirectory(fsq.AgentBase(root, me), "wake agent directory")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if bound {
				return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(
					fmt.Errorf("bound wake state directory is missing"),
				)
			}
			return wakeStateReadSelection{}, nil
		}
		return wakeStateReadSelection{}, err
	}
	defer func() { _ = agentDir.Close() }()
	var selection wakeStateReadSelection
	err = agentDir.withFD(func(dirfd int) error {
		var readErr error
		selection, readErr = readWakeStateSelectionForInspectionAt(dirfd, agentDir, root, me, inspection)
		return readErr
	})
	if err != nil {
		if !bound {
			return selection, err
		}
		return wakeStateReadSelection{}, err
	}
	if err := validateCanonicalWakeAgentDir(agentDir); err != nil {
		if bound {
			return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(err)
		}
		return selection, err
	}
	return selection, nil
}

func readWakeStateSelectionForInspectionAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	inspection wakeLockInspection,
) (wakeStateReadSelection, error) {
	bound, err := wakeLockInspectionStateBound(inspection)
	if err != nil {
		return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(err)
	}
	if !bound {
		if agentDir == nil {
			return wakeStateReadSelection{}, nil
		}
		return readWakeStateSelectionAt(dirfd, agentDir, root, me)
	}
	if agentDir == nil {
		return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(
			fmt.Errorf("bound wake state directory is missing"),
		)
	}
	selection, err := readWakeStateSelectionAt(dirfd, agentDir, root, me)
	if err != nil {
		return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(err)
	}
	afterWakeStateBoundSelection()
	confirmed := inspectWakeLockAt(dirfd, agentDir, root, me)
	if !sameWakeLockGeneration(inspection, confirmed) {
		return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(
			newWakeSnapshotReadChangedError(fmt.Errorf("wake lock changed during bound state selection")),
		)
	}
	if selection.stateErr != nil {
		return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(selection.stateErr)
	}
	if !selection.StatePreferred {
		return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(
			fmt.Errorf("bound wake state was not selected"),
		)
	}
	targetDigest, err := wakeTargetDigest(selection.Target)
	if err != nil {
		return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(err)
	}
	// StateDigest repeats validateWakeLockStateBinding's StateDigest == TargetDigest
	// invariant as defense in depth; it is unreachable while that check stands, and
	// becomes the only guard if it is ever relaxed.
	if targetDigest != inspection.Lock.TargetDigest || targetDigest != inspection.Lock.StateDigest {
		return wakeStateReadSelection{}, newWakeStateBoundInconclusiveError(
			fmt.Errorf("bound wake state target digest does not match wake lock"),
		)
	}
	return selection, nil
}

func wakeLockInspectionStateBound(inspection wakeLockInspection) (bool, error) {
	if !inspection.Exists {
		return false, nil
	}
	if err := validateWakeLockInspectionStateBindingJSON(inspection); err != nil {
		return false, err
	}
	if err := validateWakeLockStateBinding(inspection.Lock); err != nil {
		return false, err
	}
	return inspection.Lock.StateGeneration != "", nil
}

func wakeStateSelectionFromLegacy(legacy wakeStateLegacySnapshot) wakeStateReadSelection {
	return wakeStateReadSelection{
		Target:          legacy.Target.Target,
		TargetPresent:   legacy.TargetPresent,
		Prepared:        legacy.Prepared.Marker,
		PreparedPresent: legacy.PreparedPresent,
		legacy:          legacy,
	}
}

func readWakeStateLegacyPairAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
) (wakeStateLegacySnapshot, error, error) {
	if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
		return wakeStateLegacySnapshot{}, nil, err
	}
	target, targetPresent, err := readWakeTargetSnapshotAt(dirfd, agentDir, root, me)
	snapshot := wakeStateLegacySnapshot{
		Target:        target,
		TargetPresent: targetPresent,
	}
	if err != nil {
		return snapshot, nil, err
	}
	if targetPresent {
		if err := validateWakeTarget(target.Target, root, me); err != nil {
			return snapshot, nil, err
		}
	}
	prepared, preparedPresent, err := readWakeGenerationFileSnapshotAt(
		dirfd,
		agentDir,
		wakePreparedFileName,
		"wake prepared marker",
	)
	snapshot.Prepared = prepared
	snapshot.PreparedPresent = preparedPresent
	if err != nil {
		return snapshot, err, nil
	}
	return snapshot, nil, nil
}

func readWakeTargetFromState(root, me string) (wakeTarget, bool, error) {
	selection, err := readWakeStateSelection(root, me)
	return selection.Target, selection.TargetPresent, err
}

func readWakeTargetFromStateForInspection(
	root string,
	me string,
	inspection wakeLockInspection,
) (wakeTarget, bool, error) {
	selection, err := readWakeStateSelectionForInspection(root, me, inspection)
	return selection.Target, selection.TargetPresent, err
}

func readWakeTargetFromStateForInspectionAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	inspection wakeLockInspection,
) (wakeTarget, bool, error) {
	selection, err := readWakeStateSelectionForInspectionAt(dirfd, agentDir, root, me, inspection)
	return selection.Target, selection.TargetPresent, err
}
