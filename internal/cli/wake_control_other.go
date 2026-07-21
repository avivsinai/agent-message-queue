//go:build !darwin

package cli

import "errors"

var errNotSupported = errors.New("cooperative wake control is not supported on this platform")

func wakeControlSocketPath(string, string, string) string { return "" }
func startWakeControlListener(string, string, wakeLock) (func(), <-chan struct{}, func(), error) {
	return func() {}, nil, func() {}, nil
}
func cooperativeStopInjectVia(wakeLockInspection) (bool, error) { return false, errNotSupported }
