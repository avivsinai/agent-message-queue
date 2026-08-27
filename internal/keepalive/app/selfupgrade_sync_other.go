//go:build !darwin && !linux

package app

// Directory fsync is unavailable on the platforms where self-upgrade exec is
// unsupported. The sidecar file is still written and fsynced before rename.
func syncSelfUpgradeDir(string) error { return nil }
