//go:build !darwin && !linux

package cli

type wakeStateReadSelection struct{}

func readWakeStateSelectionForInspection(
	_ string,
	_ string,
	_ wakeLockInspection,
) (wakeStateReadSelection, error) {
	return wakeStateReadSelection{}, nil
}

func readWakeTargetFromState(root, me string) (wakeTarget, bool, error) {
	return readWakeTarget(root, me)
}

func readWakeTargetFromStateForInspection(
	root string,
	me string,
	_ wakeLockInspection,
) (wakeTarget, bool, error) {
	return readWakeTargetFromState(root, me)
}
