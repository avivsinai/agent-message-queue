//go:build !darwin && !linux

package cli

type wakeSelfUpgradeState struct{}

type wakeLockRemovalResidue string

const wakeLockResidueSelfUpgradeDiagnostic wakeLockRemovalResidue = "wake self-upgrade diagnostic"

func newWakeLockResidueError(_ wakeLockRemovalResidue, err error) error {
	return err
}

func removeWakeSelfUpgradeArtifactsGuarded(string, string) error {
	return nil
}
