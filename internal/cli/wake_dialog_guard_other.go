//go:build !darwin && !linux

package cli

// ClaudeDialogGuardAt reports DialogGuardUnsupported: this platform has no
// wake, so a doorbell never answers a dialog and no guard is needed.
func ClaudeDialogGuardAt(string) (state, detail string) {
	return DialogGuardUnsupported, ""
}

// InstallClaudeDialogGuardAt installs nothing: this platform has no wake.
func InstallClaudeDialogGuardAt(string, string) (bool, error) { return false, nil }

// InstallCodexDialogGuard installs nothing: this platform has no wake.
func InstallCodexDialogGuard(string) (path string, changed bool, trust string, err error) {
	return "", false, "", nil
}
