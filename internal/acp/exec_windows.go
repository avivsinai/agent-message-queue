//go:build windows

package acp

// canExecute is true on Windows, where there is no execute bit.
func canExecute(string) bool { return true }
