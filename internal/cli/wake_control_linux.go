//go:build linux

package cli

import "os"

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

func startWakeControlListenerInDirWithRestart(
	agentDir *wakeAgentDir,
	root string,
	me string,
	lock wakeLock,
	_ chan<- os.Signal,
) (func(), <-chan struct{}, func(), error) {
	return startWakeControlListenerInDir(agentDir, root, me, lock)
}
