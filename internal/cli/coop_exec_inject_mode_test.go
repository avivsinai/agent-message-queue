//go:build darwin || linux

package cli

import (
	"slices"
	"testing"
)

// Bead agent-message-queue-vd4.5 (surface audit defect 4): the wake picked
// raw typing only when the handle contained "claude" or "codex", so
// `amq coop exec --me reviewer claude` got paste, whose Enter Claude Code
// drops. coop exec now picks the mode from the harness binary.
func TestCoopWakeTypesByHarnessNotHandle(t *testing.T) {
	for _, tc := range []struct{ binary, requested, want string }{
		{"/usr/local/bin/claude", wakeInjectModeAuto, wakeInjectModeRaw},
		{"codex", wakeInjectModeAuto, wakeInjectModeRaw},
		{"grok", wakeInjectModeAuto, wakeInjectModePaste},
		{"claude", wakeInjectModePaste, wakeInjectModePaste},
	} {
		args := buildCoopWakeArgs("reviewer", "/q", coopWakeInjectMode(tc.requested, tc.binary), "", nil, "")
		at := slices.Index(args, "--inject-mode")
		if at < 0 || at+1 >= len(args) || args[at+1] != tc.want {
			t.Fatalf("%s with --wake-inject-mode %s: wake args %q, want --inject-mode %s", tc.binary, tc.requested, args, tc.want)
		}
	}
}
