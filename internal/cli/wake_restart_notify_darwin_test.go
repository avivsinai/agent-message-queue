//go:build darwin

package cli

import (
	"testing"
)

func TestConfigureDarwinWakeRestartAdvertisementUsesExactControlSocket(t *testing.T) {
	root := secureTempDirForTest(t)
	lock := wakeLock{
		Generation:   "0123456789abcdef0123456789abcdef",
		ResumeSignal: wakeResumeSignalUSR1,
	}
	configureWakeRestartAdvertisementPlatform(&lock, root, "codex")
	if lock.ResumeSignal != "" {
		t.Fatalf("Darwin resume signal = %q, want empty", lock.ResumeSignal)
	}
	want := wakeControlSocketPath(root, "codex", lock.Generation)
	if lock.ControlSocket != want {
		t.Fatalf("Darwin control socket = %q, want %q", lock.ControlSocket, want)
	}
	if err := validateWakeRestartTransportPlatform(lock, root, "codex"); err != nil {
		t.Fatal(err)
	}
}
