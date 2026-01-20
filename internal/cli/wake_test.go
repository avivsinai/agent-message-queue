//go:build darwin || linux

package cli

import "testing"

func TestSanitizeTTYForFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// macOS TTYs
		{"/dev/ttys001", "ttys001"},
		{"/dev/ttys123", "ttys123"},
		// Linux PTYs
		{"/dev/pts/1", "pts-1"},
		{"/dev/pts/42", "pts-42"},
		// Edge cases
		{"", "unknown"},
		{"unknown", "unknown"},
		{"/dev/", "unknown"},
		// Unusual but valid
		{"/dev/tty", "tty"},
		{"/dev/console", "console"},
		// Paths with unsafe chars get sanitized
		{"/dev/foo:bar", "foo-bar"},
		{"/dev/foo bar", "foo-bar"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeTTYForFilename(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeTTYForFilename(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
