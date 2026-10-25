//go:build !darwin && !linux

package cli

func codexLoginConfigured(_, _, _, _ string) bool { return false }
