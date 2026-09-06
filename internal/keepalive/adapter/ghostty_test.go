package adapter

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

type fakeCommandRunner struct {
	output  []byte
	err     error
	calls   []commandCall
	results []fakeCommandResult
}

type fakeCommandResult struct {
	output []byte
	err    error
}

type commandCall struct {
	name string
	args []string
}

func (f *fakeCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, commandCall{name: name, args: append([]string{}, args...)})
	if index := len(f.calls) - 1; index < len(f.results) {
		return f.results[index].output, f.results[index].err
	}
	return f.output, f.err
}

func TestGhosttyDiscoverReturnsTerminalTarget(t *testing.T) {
	skipNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte("BEDE3893-CE56-4309-8AEC-3D930F11225D\n")}
	target, err := (Ghostty{Runner: runner}).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if target != "ghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D" {
		t.Fatalf("target = %q, want terminal id target", target)
	}
	if len(runner.calls) != 1 || runner.calls[0].name != "osascript" {
		t.Fatalf("calls = %#v, want one osascript call", runner.calls)
	}
}

func TestGhosttyProbePassesTerminalIDAsArgument(t *testing.T) {
	skipNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte("ok\n")}
	err := (Ghostty{Runner: runner}).Probe(context.Background(), "ghostty:terminal:terminal-1")
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	call := runner.calls[0]
	if got := call.args[len(call.args)-1]; got != "TERMINAL-1" {
		t.Fatalf("last osascript arg = %q, want terminal id", got)
	}
}

func TestGhosttyInjectPassesTerminalIDAndPayloadAsArguments(t *testing.T) {
	skipNonDarwin(t)
	runner := &fakeCommandRunner{}
	payload := "AMQ [team-upgrader_v3]: message from claude\nline two"
	err := (Ghostty{Runner: runner}).Inject(context.Background(), "ghostty:terminal:terminal-1", payload)
	if err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want text then enter", len(runner.calls))
	}
	textCall := runner.calls[0]
	if got := textCall.args[len(textCall.args)-2]; got != "TERMINAL-1" {
		t.Fatalf("target arg = %q, want terminal id", got)
	}
	if got := textCall.args[len(textCall.args)-1]; got != payload {
		t.Fatalf("payload arg = %q, want payload", got)
	}
	if !strings.Contains(textCall.args[1], "input text payload to targetTerminal") {
		t.Fatalf("text script does not use native Ghostty input: %q", textCall.args[1])
	}
	if strings.Contains(textCall.args[1], `send key "enter"`) {
		t.Fatalf("text script still sends enter: %q", textCall.args[1])
	}
	submitCall := runner.calls[1]
	if got := submitCall.args[len(submitCall.args)-1]; got != "TERMINAL-1" {
		t.Fatalf("submit target arg = %q, want terminal id", got)
	}
	if !strings.Contains(submitCall.args[1], `send key "enter" to targetTerminal`) {
		t.Fatalf("submit script does not send enter: %q", submitCall.args[1])
	}
	if strings.Contains(submitCall.args[1], "input text") {
		t.Fatalf("submit script still sends text: %q", submitCall.args[1])
	}
	for _, call := range runner.calls {
		for _, disallowed := range []string{"System Events", "the clipboard", "keystroke", "AXRaise", "activate"} {
			if strings.Contains(call.args[1], disallowed) {
				t.Fatalf("script still uses %q: %q", disallowed, call.args[1])
			}
		}
	}
}

func TestGhosttyInjectEnterFailureAfterTextIsUncertain(t *testing.T) {
	skipNonDarwin(t)
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{},
		{output: []byte("enter failed"), err: errors.New("exit status 1")},
	}}
	err := (Ghostty{Runner: runner}).Inject(context.Background(), "ghostty:terminal:terminal-1", "payload")
	if !errors.Is(err, ErrInjectUncertain) {
		t.Fatalf("Inject() error = %v, want ErrInjectUncertain", err)
	}
	if !strings.Contains(err.Error(), "enter failed") {
		t.Fatalf("Inject() error = %v, want command output", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want text then failed enter", len(runner.calls))
	}
	if !strings.Contains(runner.calls[0].args[1], "input text payload to targetTerminal") {
		t.Fatalf("first call is not text inject: %#v", runner.calls[0])
	}
	if !strings.Contains(runner.calls[1].args[1], `send key "enter" to targetTerminal`) {
		t.Fatalf("second call is not enter: %#v", runner.calls[1])
	}
}

func skipNonDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Ghostty adapter uses macOS Accessibility")
	}
}
