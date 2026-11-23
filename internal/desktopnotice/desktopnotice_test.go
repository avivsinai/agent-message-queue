package desktopnotice

import "testing"

// The held-doorbell notice builds an AppleScript literal: a quote or
// backslash in the body must not end the string, and controls are removed.
func TestAppleScriptStringEscapesQuotesAndBackslashes(t *testing.T) {
	got := appleScriptString("say \"hi\" C:\\tmp\x07\nnext")
	want := `"say \"hi\" C:\\tmp  next"`
	if got != want {
		t.Fatalf("appleScriptString = %s, want %s", got, want)
	}
}
