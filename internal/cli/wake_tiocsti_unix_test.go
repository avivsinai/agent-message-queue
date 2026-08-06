//go:build darwin || linux

package cli

import (
	"errors"
	"testing"
)

func TestTIOCSTILegacySysctlHintOnlyTreatsReadableZeroAsUnsupported(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		err      error
		disabled bool
	}{
		{name: "zero", data: []byte("0\n"), disabled: true},
		{name: "one", data: []byte("1\n")},
		{name: "other", data: []byte("disabled\n")},
		{name: "absent", err: errors.New("not found")},
		{name: "unreadable", err: errors.New("permission denied")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldRead := readTIOCSTILegacySysctl
			readTIOCSTILegacySysctl = func() ([]byte, error) {
				return test.data, test.err
			}
			t.Cleanup(func() {
				readTIOCSTILegacySysctl = oldRead
			})

			if got := tiocstiLegacyDisabledHint(); got != test.disabled {
				t.Fatalf("tiocstiLegacyDisabledHint() = %v, want %v", got, test.disabled)
			}
		})
	}
}
