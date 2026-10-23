//go:build !darwin && !linux

package cli

// codexStartupHoldHint has nothing to report where coop exec cannot run.
func codexStartupHoldHint(string, string) (opsHint, bool) { return opsHint{}, false }
