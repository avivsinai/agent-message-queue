package launch

import (
	"slices"
	"testing"
)

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
