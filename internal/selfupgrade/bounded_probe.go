package selfupgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	boundedProbeMaxOutput = 4 * 1024
	boundedProbeWaitDelay = 5 * time.Second
)

// BoundedProbeOptions carries the inherited process state needed by a probe.
// The file descriptors remain owned by the caller.
type BoundedProbeOptions struct {
	Env        []string
	ExtraFiles []*os.File
}

// RunBoundedProbe runs a short-lived probe with bounded stdout and stderr and process
// cleanup. Callers must use it only after binding and verifying the image. A
// wake uses this for the post-bind preflight of the exact image it already
// verified; it is not a candidate-version discovery mechanism.
func RunBoundedProbe(
	ctx context.Context,
	path string,
	args []string,
	options BoundedProbeOptions,
) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("bounded probe context is nil")
	}
	command := exec.CommandContext(ctx, path, args...)
	command.WaitDelay = boundedProbeWaitDelay
	configureBoundedProbe(command)
	if options.Env != nil {
		command.Env = append([]string(nil), options.Env...)
	}
	if options.ExtraFiles != nil {
		command.ExtraFiles = options.ExtraFiles
	}
	output := &boundedProbeOutput{remaining: boundedProbeMaxOutput}
	stderr := &boundedProbeOutput{remaining: boundedProbeMaxOutput}
	command.Stdout = output
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	pid := command.Process.Pid
	waitErr := command.Wait()
	cleanupErr := cleanupBoundedProbeProcessGroup(pid)
	wrapProbeError := func(probeErr error) error {
		if probeErr == nil {
			return nil
		}
		if stderr.overflow {
			probeErr = errors.Join(
				probeErr,
				fmt.Errorf("bounded probe stderr exceeds %d bytes", boundedProbeMaxOutput),
			)
		}
		if diagnostic := strings.TrimSpace(stderr.buffer.String()); diagnostic != "" {
			probeErr = fmt.Errorf("%w; stderr: %s", probeErr, diagnostic)
		}
		return probeErr
	}
	if waitErr != nil {
		if cleanupErr != nil {
			return nil, wrapProbeError(errors.Join(
				waitErr,
				fmt.Errorf("bounded probe process-group cleanup: %w", cleanupErr),
			))
		}
		return nil, wrapProbeError(waitErr)
	}
	if cleanupErr != nil {
		return nil, wrapProbeError(fmt.Errorf("bounded probe process-group cleanup: %w", cleanupErr))
	}
	if output.overflow {
		return nil, wrapProbeError(fmt.Errorf("bounded probe output exceeds %d bytes", boundedProbeMaxOutput))
	}
	if stderr.overflow {
		return nil, wrapProbeError(fmt.Errorf("bounded probe stderr exceeds %d bytes", boundedProbeMaxOutput))
	}
	return append([]byte(nil), output.buffer.Bytes()...), nil
}

type boundedProbeOutput struct {
	buffer    bytes.Buffer
	remaining int
	overflow  bool
}

func (output *boundedProbeOutput) Write(data []byte) (int, error) {
	length := len(data)
	if length > output.remaining {
		output.overflow = true
		data = data[:output.remaining]
	}
	if len(data) > 0 {
		written, err := output.buffer.Write(data)
		output.remaining -= written
		if err != nil {
			return length, err
		}
	}
	return length, nil
}
