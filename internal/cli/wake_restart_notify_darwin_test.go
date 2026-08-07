//go:build darwin

package cli

import (
	"strings"
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

func TestValidateDarwinWakeRestartTransportRefusesLegacySignalAndWrongSocket(t *testing.T) {
	root := secureTempDirForTest(t)
	valid := wakeLock{Generation: "0123456789abcdef0123456789abcdef"}
	configureWakeRestartAdvertisementPlatform(&valid, root, "codex")
	tests := []struct {
		name   string
		mutate func(*wakeLock)
		want   string
	}{
		{
			name: "legacy direct signal",
			mutate: func(lock *wakeLock) {
				lock.ResumeSignal = wakeResumeSignalUSR1
			},
			want: "direct signal",
		},
		{
			name: "missing socket",
			mutate: func(lock *wakeLock) {
				lock.ControlSocket = ""
			},
			want: "exact generation control socket",
		},
		{
			name: "wrong socket",
			mutate: func(lock *wakeLock) {
				lock.ControlSocket = wakeControlSocketPath(root, "claude", lock.Generation)
			},
			want: "exact generation control socket",
		},
		{
			name: "malformed generation",
			mutate: func(lock *wakeLock) {
				lock.Generation = "not-a-generation"
			},
			want: "malformed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lock := valid
			test.mutate(&lock)
			err := validateWakeRestartTransportPlatform(lock, root, "codex")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("transport validation error = %v, want %q", err, test.want)
			}
		})
	}
}
