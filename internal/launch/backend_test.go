package launch

import (
	"slices"
	"testing"
)

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

func TestDefaultBackendsMatchesLauncherRegistry(t *testing.T) {
	want := knownLaunchers()
	backends := DefaultBackends()
	if len(backends) != len(want) {
		t.Fatalf("DefaultBackends size = %d, want %d keys %v", len(backends), len(want), want)
	}
	for _, name := range want {
		backend, ok := backends[name]
		if !ok || backend == nil {
			t.Fatalf("DefaultBackends missing %q", name)
		}
		if got := backend.Detect().Profile.Backend; got != name {
			t.Fatalf("DefaultBackends[%q].Profile.Backend = %q", name, got)
		}
	}
	for name := range backends {
		if !slices.Contains(want, name) {
			t.Fatalf("DefaultBackends extra %q", name)
		}
	}
}
