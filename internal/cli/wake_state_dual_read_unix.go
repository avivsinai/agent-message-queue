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

type wakeStateReadSelection struct {
	Target          wakeTarget
	TargetPresent   bool
	Prepared        wakeReady
	PreparedPresent bool
	PreparedErr     error
	StatePreferred  bool
	legacy          wakeStateLegacySnapshot
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
	if stateErr != nil || !stateExists || !after.TargetPresent ||
		beforePreparedErr != nil || afterPreparedErr != nil {
		return selection, nil
	}
	if err := validateWakeStateAgainstLegacy(state.State, after.legacy()); err != nil {
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

func readWakeTargetFromStateAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
) (wakeTarget, bool, error) {
	selection, err := readWakeStateSelectionAt(dirfd, agentDir, root, me)
	return selection.Target, selection.TargetPresent, err
}
