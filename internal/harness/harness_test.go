package harness

import "testing"

// Bead agent-message-queue-t97.4 (Pro finding 9): an identity read from the
// outer executable named amq-acp, not the agent it hosts, and a search of
// argument text could name a harness from any string.
func TestResolveInvocation(t *testing.T) {
	for _, tc := range []struct {
		executable string
		args       []string
		want       Invocation
	}{
		{"/usr/local/bin/claude", []string{"--name", "codex"}, Invocation{Harness: Claude}},
		{"some-cli", []string{"codex"}, Invocation{Harness: Other}},
		{"amq-acp", []string{"host", "--", "grok", "agent", "stdio"}, Invocation{Harness: Grok, Wrapper: "amq-acp"}},
		{"amq-acp", []string{"host", "--protocol", "agy-stream", "--cwd", "/w", "--", "agy"}, Invocation{Harness: Agy, Wrapper: "amq-acp"}},
		{"amq-acp", []string{"host", "--check", "pi"}, Invocation{Harness: Pi, Wrapper: "amq-acp"}},
		{"amq-acp", []string{"--mailbox", "codex"}, Invocation{Harness: Other, Wrapper: "amq-acp"}},
	} {
		if got := ResolveInvocation(tc.executable, tc.args); got != tc.want {
			t.Fatalf("ResolveInvocation(%s %v) = %+v, want %+v", tc.executable, tc.args, got, tc.want)
		}
	}
}

// The doorbell changed its text; a transcript or queued prompt from an
// older wake, hook or pi bridge still holds the old line, so every reader
// that attributes a turn or suppresses a prompt knows both.
func TestDoorbellRecognizesTheCurrentAndTheLegacyLine(t *testing.T) {
	for _, tc := range []struct {
		text         string
		is, startsAs bool
	}{
		{Doorbell, true, true},
		{LegacyDoorbell, true, true},
		{"  " + LegacyDoorbell + "\n", true, true},
		{Doorbell + "\nmore", false, true},
		{LegacyDoorbell + "\nmore", false, true},
		{": AMQ doorbell run amq drain", false, false},
		{"please run amq drain --for-turn", false, false},
	} {
		if got := IsDoorbell(tc.text); got != tc.is {
			t.Errorf("IsDoorbell(%q) = %v, want %v", tc.text, got, tc.is)
		}
		if got := StartsWithDoorbell(tc.text); got != tc.startsAs {
			t.Errorf("StartsWithDoorbell(%q) = %v, want %v", tc.text, got, tc.startsAs)
		}
	}
}
