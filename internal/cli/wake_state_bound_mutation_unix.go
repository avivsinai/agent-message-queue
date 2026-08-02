//go:build darwin || linux

package cli

// validateBoundWakeMutationAt is the fail-closed gate for lifecycle mutations
// derived from an exact lock observation. Unbound P2a claims retain their
// legacy behavior; a bound claim must complete the same state selection and
// closing lock reinspection as read-side decisions before it can authorize a
// stop, release, recovery, or rollback mutation.
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
	_, err = readWakeStateSelectionForInspectionAt(
		dirfd,
		agentDir,
		inspection.Root,
		inspection.Agent,
		inspection,
	)
	return err
}
