//go:build !darwin && !linux

package cli

import (
	"fmt"
	"os"
)

func removeWakeLockIfUnchangedGuarded(inspection wakeLockInspection) error {
	if _, err := readWakeStateSelectionForInspection(
		inspection.Root,
		inspection.Agent,
		inspection,
	); err != nil {
		return err
	}
	if err := reclaimWakeRestartStateForGuardedLockRemoval(inspection); err != nil {
		return fmt.Errorf("reconcile wake restart ownership before lock removal: %w", err)
	}
	committed, err := removeWakeLockIfUnchangedGuardedWithIOStatus(
		inspection,
		func() ([]byte, os.FileInfo, error) { return readWakeLockFileWithInfo(inspection.LockPath) },
		func() error { return os.Remove(inspection.LockPath) },
	)
	if err != nil || !committed {
		return err
	}
	if err := removeWakeSelfUpgradeArtifactsGuarded(inspection.Root, inspection.Agent); err != nil {
		_ = writeStderr(
			"warning: removed wake lock for %s but left diagnostic-only self-upgrade residue: %v\n",
			inspection.Agent,
			err,
		)
	}
	return nil
}
