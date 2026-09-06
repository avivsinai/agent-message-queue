package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestPrintUsageRegistry(t *testing.T) {
	output := captureRegistryStdout(t, func() error {
		return printUsageRegistry()
	})

	if !strings.Contains(output, "Commands:") {
		t.Fatalf("printUsageRegistry output missing Commands section:\n%s", output)
	}
	if !strings.Contains(output, "swarm        Claude Code Agent Teams integration") {
		t.Fatalf("printUsageRegistry output missing swarm command:\n%s", output)
	}
	if !strings.Contains(output, "session") || !strings.Contains(output, "Create, list, and resume named AMQ sessions") {
		t.Fatalf("printUsageRegistry output missing session command:\n%s", output)
	}
	if !strings.Contains(output, "Exit codes:") {
		t.Fatalf("printUsageRegistry output missing Exit codes section:\n%s", output)
	}
	if !strings.Contains(output, "6  Action required") {
		t.Fatalf("printUsageRegistry output missing exit code 6:\n%s", output)
	}
}

func captureRegistryStdout(t *testing.T, fn func() error) string {
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

	if runErr != nil {
		t.Fatalf("captured function error: %v", runErr)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}
