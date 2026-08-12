//go:build linux

package cli

import "testing"

func TestIsLinuxMachineID(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"0123456789abcdef0123456789abcdef", true},
		{"", false},
		{"0123456789abcdef0123456789abcde", false},
		{"0123456789abcdef0123456789abcdef0", false},
		{"0123456789ABCDEF0123456789ABCDEF", false},
		{"0123456789abcdef0123456789abcdeg", false},
		{"0123456789abcdef0123456789abcde ", false},
	}
	for _, tt := range tests {
		if got := isLinuxMachineID(tt.value); got != tt.want {
			t.Fatalf("isLinuxMachineID(%q) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestReadWakeMachineIDPlatformValidOrEmpty(t *testing.T) {
	if id := readWakeMachineIDPlatform(); id != "" && !isLinuxMachineID(id) {
		t.Fatalf("machine id = %q, want empty or 32 lowercase hex", id)
	}
}
