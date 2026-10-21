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
		{"context mismatch", ContextMismatchError("unsafe context"), ExitContextMismatch},
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
