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
	_ chan<- wakeHealthRequest,
) (func(), <-chan struct{}, func(), error) {
	return startWakeControlListenerInDir(agentDir, root, me, lock)
}

func probeWakeHealthPlatform(string, string, wakeLockInspection) wakeHealthDecision {
	return wakeHealthDecision{Status: wakeHealthUnknown, Detail: "wake health protocol is unsupported on Linux"}
}
