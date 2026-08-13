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

	// ExitContextMismatch indicates a valid command was blocked because its
	// resolved mailbox root conflicts with the pinned AMQ session context.
	ExitContextMismatch = 5

	// ExitActionRequired indicates the command cannot proceed without an
	// operator action (stale conversation token, Inspect unknown, untrusted
	// config digest, refused committed-command shape, blocked auto-rebind).
	ExitActionRequired = 6
)

// SessionContextError identifies an unsafe or incoherent mailbox context.
// It is distinct from a usage error: the command syntax was valid.
type SessionContextError struct {
	Message string
}

func (e *SessionContextError) Error() string { return e.Message }

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

// ContextMismatchError creates an error with ExitContextMismatch code.
func ContextMismatchError(format string, args ...any) error {
	return &ExitCodeError{
		Code: ExitContextMismatch,
		Err:  &SessionContextError{Message: fmt.Sprintf(format, args...)},
	}
}

// ActionRequiredError creates an error with ExitActionRequired code.
func ActionRequiredError(format string, args ...any) error {
	return &ExitCodeError{
		Code: ExitActionRequired,
		Err:  fmt.Errorf(format, args...),
	}
}

// AgentDisposition classifies a per-agent launch/resume outcome.
type AgentDisposition string

const (
	// AgentDispositionDisabled is an expected skip; it contributes 0.
	AgentDispositionDisabled AgentDisposition = "disabled"
	// AgentDispositionUnsupported is an expected capability skip; it contributes 0.
	AgentDispositionUnsupported AgentDisposition = "unsupported"
	// AgentDispositionFresh is a policy-consistent fresh start; it contributes 0.
	AgentDispositionFresh AgentDisposition = "fresh"
)

// AgentOutcome is one agent's contribution to an aggregate process exit code.
type AgentOutcome struct {
	Code        int
	Disposition AgentDisposition
}

// AggregateExitCode implements the §11 launch/resume exit contract.
// A nonzero wholeCommand is a pre-launch failure and is returned as-is.
// Once per-agent work begins (wholeCommand == 0), the aggregate is the
// highest-precedence per-agent outcome: 6 > 4 > 1 > 0.
// Expected dispositions (disabled, unsupported, policy-consistent fresh)
// contribute 0 even when Code is set to a failure. A nonzero per-agent
// code outside {6, 4, 1, 0} fails closed as ExitError.
func AggregateExitCode(wholeCommand int, agents []AgentOutcome) int {
	if wholeCommand != ExitSuccess {
		return wholeCommand
	}
	best, bestRank := ExitSuccess, 0
	for _, agent := range agents {
		code := agent.contribution()
		if rank := agentExitRank(code); rank > bestRank {
			best, bestRank = code, rank
		}
	}
	return best
}

func (o AgentOutcome) contribution() int {
	switch o.Disposition {
	case AgentDispositionDisabled, AgentDispositionUnsupported, AgentDispositionFresh:
		return ExitSuccess
	}
	switch o.Code {
	case ExitSuccess, ExitError, ExitTimeout, ExitActionRequired:
		return o.Code
	default:
		return ExitError
	}
}

func agentExitRank(code int) int {
	switch code {
	case ExitActionRequired:
		return 3
	case ExitTimeout:
		return 2
	case ExitError:
		return 1
	default:
		return 0
	}
}
