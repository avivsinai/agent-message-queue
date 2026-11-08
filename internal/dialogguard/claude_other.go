//go:build !darwin && !linux

package dialogguard

// ClaudeAt reports Unsupported: this platform has no wake, so a doorbell
// never answers a dialog and no guard is needed.
func ClaudeAt(string) (state, detail string) {
	return Unsupported, ""
}

// InstallClaudeAt installs nothing: this platform has no wake.
func InstallClaudeAt(string, string) (bool, error) { return false, nil }
