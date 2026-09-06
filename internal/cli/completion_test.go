package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func captureCompletionStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	runErr := fn()

	_ = w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String(), runErr
}

func TestCompletionBash(t *testing.T) {
	output, err := captureCompletionStdout(t, func() error {
		return runCompletion([]string{"bash"})
	})
	if err != nil {
		t.Fatalf("runCompletion(bash) error: %v", err)
	}

	// Must define the _amq function.
	if !strings.Contains(output, "_amq()") {
		t.Fatal("bash completion missing _amq() function")
	}
	// Must register the completion.
	if !strings.Contains(output, "complete -F _amq amq") {
		t.Fatal("bash completion missing 'complete -F _amq amq'")
	}
	// Must include top-level commands from the registry.
	for _, name := range commandNames() {
		if !strings.Contains(output, name) {
			t.Fatalf("bash completion missing command %q", name)
		}
	}
	// Must include subcommands for groups.
	for _, group := range []string{"dlq", "coop", "swarm", "presence"} {
		if !strings.Contains(output, group+")") {
			t.Fatalf("bash completion missing case for group %q", group)
		}
	}
}
