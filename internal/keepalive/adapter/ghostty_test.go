package adapter

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

type fakeCommandRunner struct {
	output []byte
	err    error
	calls  []commandCall
}

type commandCall struct {
	name string
	args []string
}

func (f *fakeCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, commandCall{name: name, args: append([]string{}, args...)})
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

func TestParseGhosttyTerminalTarget(t *testing.T) {
	id, err := parseGhosttyTerminalTarget(" ghostty:terminal:terminal-1 ")
	if err != nil {
		t.Fatalf("parseGhosttyTerminalTarget() error = %v", err)
	}
	if id != "terminal-1" {
		t.Fatalf("id = %q, want terminal-1", id)
	}
}

func TestParseGhosttyTerminalTargetRejectsOldTitleTargets(t *testing.T) {
	_, err := parseGhosttyTerminalTarget("Team Alpha")
	if err == nil {
		t.Fatal("parseGhosttyTerminalTarget(old title) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "reattach") {
		t.Fatalf("error = %v, want reattach guidance", err)
	}
}

func TestParseGhosttyTerminalTargetRejectsEmptyID(t *testing.T) {
	_, err := parseGhosttyTerminalTarget("ghostty:terminal:")
	if err == nil {
		t.Fatal("parseGhosttyTerminalTarget(empty id) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "missing an id") {
		t.Fatalf("error = %v, want missing id", err)
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
	if got := call.args[len(call.args)-1]; got != "terminal-1" {
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
	call := runner.calls[0]
	if got := call.args[len(call.args)-2]; got != "terminal-1" {
		t.Fatalf("target arg = %q, want terminal id", got)
	}
	if got := call.args[len(call.args)-1]; got != payload {
		t.Fatalf("payload arg = %q, want payload", got)
	}
	if !strings.Contains(call.args[1], "input text payload to targetTerminal") {
		t.Fatalf("script does not use native Ghostty input: %q", call.args[1])
	}
	if !strings.Contains(call.args[1], `send key "enter" to targetTerminal`) {
		t.Fatalf("script does not send enter to target terminal: %q", call.args[1])
	}
	for _, disallowed := range []string{"System Events", "the clipboard", "keystroke", "AXRaise", "activate"} {
		if strings.Contains(call.args[1], disallowed) {
			t.Fatalf("script still uses %q: %q", disallowed, call.args[1])
		}
	}
}

func TestGhosttyInjectTrimsTrailingLineBreaksBeforeEnter(t *testing.T) {
	skipNonDarwin(t)
	runner := &fakeCommandRunner{}
	payload := "AMQ [team-upgrader_v3]: message from claude\nline two\r\n\n"
	err := (Ghostty{Runner: runner}).Inject(context.Background(), "ghostty:terminal:terminal-1", payload)
	if err != nil {
		t.Fatalf("Inject() error = %v", err)
	}
	call := runner.calls[0]
	if got, want := call.args[len(call.args)-1], "AMQ [team-upgrader_v3]: message from claude\nline two"; got != want {
		t.Fatalf("payload arg = %q, want %q", got, want)
	}
}

func TestGhosttyScriptsFailClosedOnTerminalIDs(t *testing.T) {
	for name, script := range map[string]string{
		"probe":  ghosttyProbeScript,
		"inject": ghosttyInjectScript,
	} {
		if !strings.Contains(script, "matchCount") {
			t.Fatalf("%s script does not count matching terminals", name)
		}
		if !strings.Contains(script, "no Ghostty terminal with id") {
			t.Fatalf("%s script does not fail on missing target", name)
		}
		if !strings.Contains(script, "ambiguous Ghostty terminal id") {
			t.Fatalf("%s script does not fail on duplicate target", name)
		}
	}
}

func TestGhosttyErrorsIncludeCommandOutput(t *testing.T) {
	skipNonDarwin(t)
	runner := &fakeCommandRunner{output: []byte("accessibility denied"), err: errors.New("exit status 1")}
	err := (Ghostty{Runner: runner}).Probe(context.Background(), "ghostty:terminal:terminal-1")
	if err == nil {
		t.Fatal("Probe() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "accessibility denied") {
		t.Fatalf("error = %v, want command output", err)
	}
}

func skipNonDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Ghostty adapter uses macOS Accessibility")
	}
}
