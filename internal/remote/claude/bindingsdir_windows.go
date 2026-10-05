//go:build windows

package claude

// bindingsDirState cannot read a directory without following links on
// Windows, so it reports binding evidence and never allows.
func bindingsDirState(dir, sessionID string) (allow, present bool) {
	return false, true
}
