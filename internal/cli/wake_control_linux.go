//go:build linux

package cli

func darwinControlSocketBasenameForCleanup(*wakeAgentDir, string) (string, error) {
	return "", nil
}

func startWakeControlListenerInDir(
	*wakeAgentDir,
	string,
	string,
	wakeLock,
) (func(), <-chan struct{}, func(), error) {
	return func() {}, nil, func() {}, nil
}
