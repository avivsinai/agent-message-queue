//go:build !darwin

package cli

func wakeControlSocketPath(string, string, string) string { return "" }
