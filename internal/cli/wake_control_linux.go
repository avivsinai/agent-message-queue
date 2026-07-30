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
	chan<- string,
) (func(), <-chan struct{}, func(), error) {
	return func() {}, nil, func() {}, nil
}
