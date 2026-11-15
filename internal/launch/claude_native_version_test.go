package launch

import (
	"testing"
)

// ClaudeNativeVersionSupported admits strict releases at or above the floor.
// Bead agent-message-queue-upbr: a registration that reports 2.1.325 must be
// admitted; one below the floor must be refused with a line that names the
// installed version.
func TestClaudeNativeVersionSupported(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"2.1.311", true},
		{"2.1.325", true},
		{"2.1.310", false},
		{"2.1.0", false},
		{"bogus", false},
		{"", false},
	}
	for _, c := range cases {
		got := ClaudeNativeVersionSupported(c.version)
		if got != c.want {
			t.Errorf("ClaudeNativeVersionSupported(%q) = %v; want %v", c.version, got, c.want)
		}
	}
}
