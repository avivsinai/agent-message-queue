//go:build linux

package cli

import (
	"errors"
	"os"
	"testing"
)

var signalWakeProcess = func(int, os.Signal) error {
	return errors.New("bare-PID wake signaling is unavailable on Linux")
}

func requireBarePIDWakeTermination(t *testing.T) {
	t.Helper()
	t.Skip("bare-PID termination is Darwin-only; Linux pidfd behavior has platform tests")
}
