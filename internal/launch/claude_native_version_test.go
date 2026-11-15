package launch

import (
	"testing"
)

// Bead upbr: a release above 2.1.311 is admitted.
func TestClaudeNativeVersionSupported(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"2.1.311", true},
		{"2.1.325", true},
		{"2.1.310", false},
	}
	for _, c := range cases {
		got := ClaudeNativeVersionSupported(c.version)
		if got != c.want {
			t.Errorf("ClaudeNativeVersionSupported(%q) = %v; want %v", c.version, got, c.want)
		}
	}
}
