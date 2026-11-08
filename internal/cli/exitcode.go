package cli

import "github.com/avivsinai/agent-message-queue/internal/exitcode"

// The exit codes and their error types live in internal/exitcode. These
// aliases keep one identity, so errors.As and type assertions match either
// name.
const (
	ExitSuccess         = exitcode.ExitSuccess
	ExitError           = exitcode.ExitError
	ExitUsage           = exitcode.ExitUsage
	ExitNotFound        = exitcode.ExitNotFound
	ExitTimeout         = exitcode.ExitTimeout
	ExitContextMismatch = exitcode.ExitContextMismatch
	ExitActionRequired  = exitcode.ExitActionRequired
)

type (
	SessionContextError = exitcode.SessionContextError
	ExitCodeError       = exitcode.ExitCodeError
)

// GetExitCode returns the process exit code for err.
func GetExitCode(err error) int { return exitcode.GetExitCode(err) }

// WithExitCode wraps err with code.
func WithExitCode(code int, err error) error { return exitcode.WithExitCode(code, err) }

// UsageError creates an error with ExitUsage code.
func UsageError(format string, args ...any) error { return exitcode.UsageError(format, args...) }

// NotFoundError creates an error with ExitNotFound code.
func NotFoundError(format string, args ...any) error {
	return exitcode.NotFoundError(format, args...)
}

// TimeoutError creates an error with ExitTimeout code.
func TimeoutError(format string, args ...any) error {
	return exitcode.TimeoutError(format, args...)
}

// ContextMismatchError creates an error with ExitContextMismatch code.
func ContextMismatchError(format string, args ...any) error {
	return exitcode.ContextMismatchError(format, args...)
}

// ActionRequiredError creates an error with ExitActionRequired code.
func ActionRequiredError(format string, args ...any) error {
	return exitcode.ActionRequiredError(format, args...)
}
