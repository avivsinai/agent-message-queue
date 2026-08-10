package amq

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// wakeStderrDrainMode is deliberately private to the amq-keepalive binary.
// The parent passes the wake's stderr pipe as fd 3 and the bounded capture file
// as fd 4. The drain process outlives the short launcher invocation, preserves
// the wake's stderr reader, and discards everything beyond the diagnostic cap.
const wakeStderrDrainMode = "AMQ_KEEPALIVE_INTERNAL_STDERR_DRAIN_V1"

// init handles the private subprocess before main or a linked test binary can
// interpret its arguments. This keeps the single-binary protocol valid in
// every package that exercises StartWake, without test-binary name sniffing.
func init() {
	if os.Getenv(wakeStderrDrainMode) != "1" || len(os.Args) != 2 || os.Args[1] != "__wake-stderr-drain" {
		return
	}
	if err := runInheritedWakeStderrDrain(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "amq-keepalive stderr drain failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runInheritedWakeStderrDrain() error {
	reader := os.NewFile(uintptr(3), "wake-stderr-pipe")
	output := os.NewFile(uintptr(4), "wake-stderr-capture")
	defer func() { _ = reader.Close() }()
	defer func() { _ = output.Close() }()
	return drainWakeStderr(reader, output, os.Stderr)
}

type wakeStderrCapture interface {
	io.Writer
	Sync() error
}

func drainWakeStderr(reader io.Reader, output wakeStderrCapture, diagnostic io.Writer) error {
	var diagnosticErr error
	_, captureErr := io.CopyN(output, reader, maxWakeStartupStderrBytes)
	if captureErr != nil && !errors.Is(captureErr, io.EOF) {
		// Keep draining even when the diagnostic destination fails so the wake
		// never blocks or receives SIGPIPE because its observer went away.
		_, _ = io.Copy(io.Discard, reader)
		return fmt.Errorf("capture wake stderr: %w", captureErr)
	}
	if captureErr == nil {
		probe := []byte{0}
		n, probeErr := reader.Read(probe)
		if n > 0 {
			if _, err := fmt.Fprintf(diagnostic, "[stderr truncated after %d bytes]\n", maxWakeStartupStderrBytes); err != nil {
				diagnosticErr = fmt.Errorf("write wake stderr truncation diagnostic: %w", err)
			}
		}
		if probeErr != nil && !errors.Is(probeErr, io.EOF) {
			_, _ = io.Copy(io.Discard, reader)
			return fmt.Errorf("probe wake stderr truncation: %w", probeErr)
		}
		if n == 0 {
			if err := output.Sync(); err != nil {
				return fmt.Errorf("sync wake stderr capture: %w", err)
			}
			return nil
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return fmt.Errorf("drain wake stderr: %w", err)
		}
	}
	if err := output.Sync(); err != nil {
		return fmt.Errorf("sync wake stderr capture: %w", err)
	}
	return diagnosticErr
}
