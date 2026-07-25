//go:build linux

package cli

import (
	"os"
	"testing"
)

func TestLinuxUnsupportedPrivateBootstrapDescriptorsAreScrubbed(t *testing.T) {
	tests := []string{
		envWakePrivateStopFD,
		envWakeRepairChildControlFD,
	}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "9")

			loopCalled := false
			err := runWakeWithLoop(nil, func(wakeConfig) error {
				loopCalled = true
				return nil
			})
			want := name + " is unsupported on linux"
			if err == nil || err.Error() != want {
				t.Fatalf("unsupported descriptor error = %v, want %q", err, want)
			}
			if loopCalled {
				t.Fatal("wake loop ran with an unsupported private bootstrap descriptor")
			}
			if value, present := os.LookupEnv(name); present {
				t.Fatalf("%s remained in the process environment as %q", name, value)
			}
		})
	}
}
