package cli

import "testing"

// A coop-named launch of the amit product binary must auto-name the session
// via the --name argv injection (amit accepts a --name global flag like pi)
// instead of falling into coopNamedModeUnknown, which prints the
// "name this CLI session manually (unknown binary)" reminder on every
// product launch through coop exec.
func TestCoopNamedAmitHarnessUsesArgvMode(t *testing.T) {
	harness, ok := coopNamedHarnessFor("amit")
	if !ok {
		t.Fatalf("coopNamedHarnessFor(amit) not found; the product binary is still an unknown harness")
	}
	if harness.mode != coopNamedModeArgv {
		t.Fatalf("amit mode = %v, want %v", harness.mode, coopNamedModeArgv)
	}
	if harness.resumeSyntax != coopNamedResumeFlags {
		t.Fatalf("amit resumeSyntax = %v, want %v", harness.resumeSyntax, coopNamedResumeFlags)
	}
	if got := coopNamedModeFor("/usr/local/bin/amit"); got != coopNamedModeArgv {
		t.Fatalf("coopNamedModeFor(basename path) = %v, want %v", got, coopNamedModeArgv)
	}
	// The reminder must NOT fire for amit anymore.
	if got := coopNamedModeFor("amit"); got == coopNamedModeUnknown {
		t.Fatalf("amit still resolves to unknown mode")
	}
	// A genuinely unknown binary still lands in the manual-rename mode.
	if got := coopNamedModeFor("mystery-agent"); got != coopNamedModeUnknown {
		t.Fatalf("mystery-agent mode = %v, want %v", got, coopNamedModeUnknown)
	}
}
