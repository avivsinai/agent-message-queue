package amq

import (
	"bytes"
	"strings"
	"testing"
)

type memoryWakeStderrCapture struct {
	buffer   bytes.Buffer
	syncErr  error
	writeErr error
	failAt   int
}

func (capture *memoryWakeStderrCapture) Sync() error { return capture.syncErr }
func (capture *memoryWakeStderrCapture) Len() int    { return capture.buffer.Len() }
func (capture *memoryWakeStderrCapture) String() string {
	return capture.buffer.String()
}

func (capture *memoryWakeStderrCapture) Write(data []byte) (int, error) {
	if capture.writeErr == nil {
		return capture.buffer.Write(data)
	}
	remaining := capture.failAt - capture.buffer.Len()
	if remaining <= 0 {
		return 0, capture.writeErr
	}
	if remaining < len(data) {
		n, _ := capture.buffer.Write(data[:remaining])
		return n, capture.writeErr
	}
	return capture.buffer.Write(data)
}

// Restored from origin/main during the round-2 cull: the only direct proof that
// drainWakeStderr delivers a short refusal verbatim to the capture sink and
// leaves the diagnostic writer untouched. Happy path of the stderr drain.
func TestDrainWakeStderrPreservesShortDiagnostic(t *testing.T) {
	output := &memoryWakeStderrCapture{}
	var diagnostic bytes.Buffer
	if err := drainWakeStderr(strings.NewReader("specific refusal\n"), output, &diagnostic); err != nil {
		t.Fatalf("drainWakeStderr() error = %v", err)
	}
	if got := output.String(); got != "specific refusal\n" {
		t.Fatalf("capture = %q", got)
	}
	if diagnostic.Len() != 0 {
		t.Fatalf("diagnostic = %q, want empty", diagnostic.String())
	}
}
