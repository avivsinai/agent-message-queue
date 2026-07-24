//go:build linux

package cli

func darwinControlSocketBasenameForCleanup(*wakeAgentDir, string) (string, error) {
	return "", nil
}
