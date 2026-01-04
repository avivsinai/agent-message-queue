package cli

import (
	"errors"
	"testing"
)

func TestGetExitCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected int
	}{
		{"nil error", nil, ExitSuccess},
		{"plain error", errors.New("oops"), ExitError},
		{"usage error", UsageError("bad flag"), ExitUsage},
		{"not found error", NotFoundError("msg not found"), ExitNotFound},
		{"timeout error", TimeoutError("timed out"), ExitTimeout},
		{"wrapped exit code", WithExitCode(ExitNotFound, errors.New("custom")), ExitNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetExitCode(tt.err)
			if got != tt.expected {
				t.Errorf("GetExitCode() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestExitCodeErrorUnwrap(t *testing.T) {
	underlying := errors.New("underlying")
	wrapped := WithExitCode(ExitNotFound, underlying)

	if !errors.Is(wrapped, underlying) {
		t.Error("wrapped error should be unwrappable to underlying")
	}

	exitErr := wrapped.(*ExitCodeError)
	if exitErr.Unwrap() != underlying {
		t.Error("Unwrap() should return underlying error")
	}
}

func TestExitCodeErrorMessage(t *testing.T) {
	err := UsageError("invalid flag: %s", "--foo")
	if err.Error() != "invalid flag: --foo" {
		t.Errorf("Error() = %q, want %q", err.Error(), "invalid flag: --foo")
	}

	// Error with no underlying message
	empty := &ExitCodeError{Code: ExitError, Err: nil}
	if empty.Error() != "exit code 1" {
		t.Errorf("Error() = %q, want %q", empty.Error(), "exit code 1")
	}
}
