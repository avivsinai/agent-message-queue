package amq

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type memoryWakeStderrCapture struct {
	bytes.Buffer
	syncErr error
}

func (capture *memoryWakeStderrCapture) Sync() error { return capture.syncErr }

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

func TestDrainWakeStderrCapsCaptureAndReportsTruncation(t *testing.T) {
	output := &memoryWakeStderrCapture{}
	var diagnostic bytes.Buffer
	input := strings.Repeat("x", maxWakeStartupStderrBytes+8)
	if err := drainWakeStderr(strings.NewReader(input), output, &diagnostic); err != nil {
		t.Fatalf("drainWakeStderr() error = %v", err)
	}
	if output.Len() != maxWakeStartupStderrBytes {
		t.Fatalf("capture length = %d, want %d", output.Len(), maxWakeStartupStderrBytes)
	}
	if !strings.Contains(diagnostic.String(), "stderr truncated after 16384 bytes") {
		t.Fatalf("diagnostic = %q, want truncation marker", diagnostic.String())
	}
}

func TestDrainWakeStderrReportsSyncFailure(t *testing.T) {
	want := errors.New("sync failed")
	output := &memoryWakeStderrCapture{syncErr: want}
	err := drainWakeStderr(strings.NewReader("specific refusal"), output, &bytes.Buffer{})
	if !errors.Is(err, want) {
		t.Fatalf("drainWakeStderr() error = %v, want %v", err, want)
	}
}
