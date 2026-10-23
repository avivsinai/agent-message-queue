//go:build darwin || linux

package cli

import (
	"slices"
	"testing"
)

// Bead agent-message-queue-vd4.5 (surface audit defect 4): the wake picked
// raw typing only when the handle contained "claude" or "codex", so
// `amq coop exec --me reviewer claude` got paste, whose Enter Claude Code
// drops. coop exec now picks the mode from the harness binary. #1187 review:
// the LF that submits in Codex also keyed on the handle. Bead
// agent-message-queue-t97.3 (Pro finding 8): an unknown binary still fell
// back to the handle, so `--me codex-reviewer unknown-cli` was typed into as
// Codex; and a wrapped managed seat was judged by its wrapper executable.
func TestCoopWakeTypesByHarnessNotHandle(t *testing.T) {
	for _, tc := range []struct {
		me, binary, managedProvider, requested string
		mode, harness, prelude                 string
		alternateScreen                        bool
	}{
		{"reviewer", "/usr/local/bin/claude", "", wakeInjectModeAuto, wakeInjectModeRaw, "claude", "", true},
		{"x", "/opt/homebrew/bin/codex", "", wakeInjectModeAuto, wakeInjectModeRaw, "codex", "\n", true},
		{"reviewer", "grok", "", wakeInjectModeAuto, wakeInjectModePaste, "grok", "", false},
		{"reviewer", "claude", "", wakeInjectModePaste, wakeInjectModePaste, "claude", "", true},
		{"reviewer", "/opt/bin/AGENT.exe", "", wakeInjectModeAuto, wakeInjectModePaste, "cursor-agent", "", false},
		{"codex-reviewer", "/usr/local/bin/unknown-cli", "", wakeInjectModeAuto, wakeInjectModePaste, "other", "", false},
		{"reviewer", "/usr/bin/sandbox-wrapper", "claude", wakeInjectModeAuto, wakeInjectModeRaw, "claude", "", true},
	} {
		wakeHarness := coopWakeHarness(tc.binary, tc.managedProvider)
		args := buildCoopWakeArgs(tc.me, "/q", coopWakeInjectMode(tc.requested, wakeHarness), wakeHarness, "", nil, "")
		mode, harnessFlag := slices.Index(args, "--inject-mode"), slices.Index(args, "--harness")
		if mode < 0 || harnessFlag < 0 || args[mode+1] != tc.mode || args[harnessFlag+1] != tc.harness {
			t.Fatalf("%s as %s: wake args %q, want --inject-mode %s --harness %s", tc.binary, tc.me, args, tc.mode, tc.harness)
		}
		cfg := &wakeConfig{me: tc.me, harness: args[harnessFlag+1]}
		if got := rawSubmitPrelude(cfg); got != tc.prelude {
			t.Fatalf("%s as %s: submit prelude %q, want %q", tc.binary, tc.me, got, tc.prelude)
		}
		if got := wakeUsesAlternateScreenTUI(cfg); got != tc.alternateScreen {
			t.Fatalf("%s as %s: alternate screen %v, want %v", tc.binary, tc.me, got, tc.alternateScreen)
		}
	}
}
