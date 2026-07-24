//go:build !darwin

package cli

func wakeControlSocketPath(string, string, string) string { return "" }
func startWakeControlListener(string, string, wakeLock) (func(), <-chan struct{}, func(), error) {
	return func() {}, nil, func() {}, nil
}
func startWakeControlListenerInDir(*wakeAgentDir, string, string, wakeLock) (func(), <-chan struct{}, func(), error) {
	return func() {}, nil, func() {}, nil
}
