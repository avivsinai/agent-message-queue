package amq

import (
	"bytes"
	"errors"
	"io"
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

type eofTrackingReader struct {
	reader *strings.Reader
	sawEOF bool
}

func (reader *eofTrackingReader) Read(data []byte) (int, error) {
	n, err := reader.reader.Read(data)
	if errors.Is(err, io.EOF) {
		reader.sawEOF = true
	}
	return n, err
}

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }

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

func TestDrainWakeStderrDoesNotReportTruncationAtExactCap(t *testing.T) {
	output := &memoryWakeStderrCapture{}
	var diagnostic bytes.Buffer
	input := strings.Repeat("x", maxWakeStartupStderrBytes)
	if err := drainWakeStderr(strings.NewReader(input), output, &diagnostic); err != nil {
		t.Fatalf("drainWakeStderr() error = %v", err)
	}
	if output.Len() != maxWakeStartupStderrBytes {
		t.Fatalf("capture length = %d, want %d", output.Len(), maxWakeStartupStderrBytes)
	}
	if diagnostic.Len() != 0 {
		t.Fatalf("diagnostic = %q, want no truncation marker", diagnostic.String())
	}
}

func TestDrainWakeStderrDrainsReaderAfterCaptureWriteFailure(t *testing.T) {
	want := errors.New("write failed")
	input := &eofTrackingReader{reader: strings.NewReader(strings.Repeat("x", maxWakeStartupStderrBytes+8))}
	output := &memoryWakeStderrCapture{writeErr: want, failAt: 4}
	err := drainWakeStderr(input, output, &bytes.Buffer{})
	if !errors.Is(err, want) {
		t.Fatalf("drainWakeStderr() error = %v, want %v", err, want)
	}
	if !input.sawEOF {
		t.Fatal("capture write failure left wake stderr unread")
	}
}

func TestDrainWakeStderrReportsDiagnosticWriteFailureAfterDraining(t *testing.T) {
	want := errors.New("diagnostic write failed")
	input := &eofTrackingReader{reader: strings.NewReader(strings.Repeat("x", maxWakeStartupStderrBytes+8))}
	output := &memoryWakeStderrCapture{}
	err := drainWakeStderr(input, output, errorWriter{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("drainWakeStderr() error = %v, want %v", err, want)
	}
	if !input.sawEOF {
		t.Fatal("diagnostic write failure left wake stderr unread")
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
