//go:build darwin

package cli

import "testing"

func requireBarePIDWakeTermination(t *testing.T) {
	t.Helper()
	t.Skip("bare-PID termination is unavailable; Darwin fails closed to cooperative stop")
}
