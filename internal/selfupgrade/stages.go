package selfupgrade

import "path/filepath"

func selfUpgradeStagePrefix(locator string) string {
	return "." + filepath.Base(locator) + ".amq-self-upgrade-"
}

// CleanupStages removes only the private Darwin stage directories created by
// ExecImage. A successful Darwin exec cannot run cleanup in the old image, so
// the replacement performs this bounded cleanup after capturing its image.
func CleanupStages(locator string) error {
	return cleanupImageStagesPlatform(locator)
}
