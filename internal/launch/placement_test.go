package launch

import (
	"testing"
)

func TestResolvePlacementOmittedUsesLegacy(t *testing.T) {
	for _, backend := range []string{LauncherTMux, LauncherCMux, LauncherGhostty, CommandsBackendName} {
		preview, err := ResolvePlacement(backend, nil)
		if err != nil || !preview.Supported || preview.Requested != nil {
			t.Fatalf("%s omitted preview = %#v, %v", backend, preview, err)
		}
		if preview.Effective != LegacyPlacement(backend) {
			t.Fatalf("%s effective = %+v, want %+v", backend, preview.Effective, LegacyPlacement(backend))
		}
	}
}
