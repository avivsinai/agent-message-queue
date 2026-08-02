//go:build !darwin && !linux

package cli

func readWakeTargetFromState(root, me string) (wakeTarget, bool, error) {
	return readWakeTarget(root, me)
}
