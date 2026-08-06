//go:build darwin

package cli

func startWakeReloadTransportInDir(
	_ *wakeAgentDir,
	_ string,
	_ string,
	_ wakeLockInspection,
	_ wakeOwner,
) (func(), error) {
	return func() {}, nil
}
