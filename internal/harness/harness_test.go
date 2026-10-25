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
