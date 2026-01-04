package cli

import "fmt"

// Exit codes for CLI commands.
// These provide semantic meaning for scripting and automation.
const (
	// ExitSuccess indicates the command completed successfully.
	ExitSuccess = 0

	// ExitError indicates a general error occurred.
	ExitError = 1

	// ExitUsage indicates invalid arguments or flags were provided.
	ExitUsage = 2

	// ExitNotFound indicates a requested resource was not found
	// (message, config, agent, etc.).
	ExitNotFound = 3

	// ExitTimeout indicates a timeout occurred (watch, monitor commands).
	ExitTimeout = 4
)

// ExitCodeError wraps an error with a specific exit code.
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit code %d", e.Code)
}

func (e *ExitCodeError) Unwrap() error {
	return e.Err
}

// GetExitCode extracts the exit code from an error.
// Returns ExitSuccess (0) if err is nil.
// Returns the wrapped code if err is an *ExitCodeError.
// Returns ExitError (1) for all other errors.
func GetExitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	if exitErr, ok := err.(*ExitCodeError); ok {
		return exitErr.Code
	}
	return ExitError
}

// WithExitCode wraps an error with a specific exit code.
func WithExitCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &ExitCodeError{Code: code, Err: err}
}

// UsageError creates an error with ExitUsage code.
func UsageError(format string, args ...any) error {
	return &ExitCodeError{
		Code: ExitUsage,
		Err:  fmt.Errorf(format, args...),
	}
}

// NotFoundError creates an error with ExitNotFound code.
func NotFoundError(format string, args ...any) error {
	return &ExitCodeError{
		Code: ExitNotFound,
		Err:  fmt.Errorf(format, args...),
	}
}

// TimeoutError creates an error with ExitTimeout code.
func TimeoutError(format string, args ...any) error {
	return &ExitCodeError{
		Code: ExitTimeout,
		Err:  fmt.Errorf(format, args...),
	}
}
