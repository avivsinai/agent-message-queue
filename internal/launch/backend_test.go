package launch

import "testing"

func TestDetectResultRejectsEffectiveOutsideMaximum(t *testing.T) {
	result := DetectResult{
		Available: true,
		Profile:   CommandsProfile(),
		Effective: []Capability{CapClose},
	}
	if err := result.Validate(); err == nil || err.Error() != `effective capability "close" is outside the static profile` {
		t.Fatalf("Validate = %v", err)
	}
}

func TestProfileIdentity(t *testing.T) {
	if got := CommandsProfile().Identity(); got != "commands/any/v1" {
		t.Fatalf("Identity = %q", got)
	}
}
