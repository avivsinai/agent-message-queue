package cli

import (
	"errors"
	"fmt"
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

func TestActionRequiredError(t *testing.T) {
	err := ActionRequiredError("stale token for %s", "claude")
	if GetExitCode(err) != ExitActionRequired {
		t.Errorf("GetExitCode() = %d, want %d", GetExitCode(err), ExitActionRequired)
	}
	if err.Error() != "stale token for claude" {
		t.Errorf("Error() = %q, want %q", err.Error(), "stale token for claude")
	}
}

func TestAggregateExitCode(t *testing.T) {
	ranked := []int{ExitSuccess, ExitError, ExitTimeout, ExitActionRequired}
	rank := map[int]int{
		ExitActionRequired: 3,
		ExitTimeout:        2,
		ExitError:          1,
		ExitSuccess:        0,
	}

	for _, a := range ranked {
		for _, b := range ranked {
			want := a
			if rank[b] > rank[a] {
				want = b
			}
			t.Run(fmt.Sprintf("pair %d+%d", a, b), func(t *testing.T) {
				got := AggregateExitCode(0, []AgentOutcome{{Code: a}, {Code: b}})
				if got != want {
					t.Errorf("got %d, want %d", got, want)
				}
			})
		}
	}

	tests := []struct {
		name         string
		wholeCommand int
		agents       []AgentOutcome
		want         int
	}{
		{
			name: "6 beats 4: action_required plus timeout",
			agents: []AgentOutcome{
				{Code: ExitActionRequired},
				{Code: ExitTimeout},
			},
			want: ExitActionRequired,
		},
		{
			name: "all expected dispositions is 0",
			agents: []AgentOutcome{
				{Code: ExitError, Disposition: AgentDispositionDisabled},
				{Code: ExitTimeout, Disposition: AgentDispositionUnsupported},
				{Code: ExitActionRequired, Disposition: AgentDispositionFresh},
			},
			want: ExitSuccess,
		},
		{
			name: "disabled must not escalate a nonzero code",
			agents: []AgentOutcome{
				{Code: ExitError, Disposition: AgentDispositionDisabled},
			},
			want: ExitSuccess,
		},
		{
			name: "disabled 6 plus timeout is 4, not 6",
			agents: []AgentOutcome{
				{Code: ExitActionRequired, Disposition: AgentDispositionDisabled},
				{Code: ExitTimeout},
			},
			want: ExitTimeout,
		},
		{
			name:         "usage preempts per-agent action_required",
			wholeCommand: ExitUsage,
			agents:       []AgentOutcome{{Code: ExitActionRequired}},
			want:         ExitUsage,
		},
		{
			name:         "context mismatch preempts per-agent timeout",
			wholeCommand: ExitContextMismatch,
			agents:       []AgentOutcome{{Code: ExitTimeout}},
			want:         ExitContextMismatch,
		},
		{
			name:         "not-found preempts per-agent error",
			wholeCommand: ExitNotFound,
			agents:       []AgentOutcome{{Code: ExitError}},
			want:         ExitNotFound,
		},
		{
			name:   "empty agents is success",
			agents: nil,
			want:   ExitSuccess,
		},
		{
			name:         "nonzero whole-command is not dropped to success",
			wholeCommand: ExitError,
			agents:       nil,
			want:         ExitError,
		},
		{
			name:         "off-contract per-agent code fails closed as general error",
			wholeCommand: 0,
			agents:       []AgentOutcome{{Code: ExitNotFound}},
			want:         ExitError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AggregateExitCode(tt.wholeCommand, tt.agents)
			if got != tt.want {
				t.Errorf("AggregateExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}
