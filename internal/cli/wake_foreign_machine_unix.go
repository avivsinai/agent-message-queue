//go:build darwin || linux

package cli

// wakeForeignGenericLock reports an ownerless generic wake lock that another
// machine wrote (a migrated home, or a VM or container sharing this root).
// This host cannot prove that wake is dead: its pid and binary path belong to
// the other machine. Only the operator can confirm that machine no longer uses
// the root; the existing confirmed path is the original launch rerun with -y.
// Owner-bound claims and any lock this host cannot classify keep their own
// recovery and inspection paths.
func wakeForeignGenericLock(inspection wakeLockInspection) bool {
	if inspection.Status != wakeLockUnverified ||
		wakeLockHasOwnerMarkers(inspection) ||
		classifyPersistedWakeClaim(inspection) != wakeClaimGeneric {
		return false
	}
	state, reason := classifyWakeLockMachine(inspection.Lock)
	return state == wakeMachineDifferent || (state == wakeMachineUnknown && reason == "hostname mismatch")
}
