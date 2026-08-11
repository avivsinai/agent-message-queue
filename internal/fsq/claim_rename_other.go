//go:build !windows

package fsq

// claimRename is the exclusive-claim primitive behind MoveNewToCur. On POSIX,
// rename(2) resolves the source by path atomically: exactly one of several
// concurrent claimers moves new/<name> to cur/<name> and every loser observes
// ENOENT. A pre-existing cur/<name> is replaced — that state is unreachable
// through AMQ's own flows (see ClaimCollisionError), and rename's atomicity is
// the exclusivity guarantee, so no pre-check is added here.
//
// The Windows implementation must provide the same winner/loser contract
// without path-atomic rename: see claim_rename_windows.go and issue #485.
func claimRename(root *DeliveryRoot, newPath, curPath string) error {
	return root.root.Rename(newPath, curPath)
}
