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

func TestParseGhosttyTerminalTarget(t *testing.T) {
	id, err := parseGhosttyTerminalTarget(" ghostty:terminal:terminal-1 ")
	if err != nil {
		t.Fatalf("parseGhosttyTerminalTarget() error = %v", err)
	}
	if id != "TERMINAL-1" {
		t.Fatalf("id = %q, want TERMINAL-1", id)
	}
}

func TestGhosttyNormalizeTargetTrimsTerminalID(t *testing.T) {
	target, err := (Ghostty{}).NormalizeTarget(" ghostty:terminal:terminal-1 ")
	if err != nil {
		t.Fatalf("NormalizeTarget() error = %v", err)
	}
	if target != "ghostty:terminal:TERMINAL-1" {
		t.Fatalf("target = %q, want canonical terminal target", target)
	}
}

func TestGhosttyNormalizeTargetCanonicalizesTerminalIDCase(t *testing.T) {
	target, err := (Ghostty{}).NormalizeTarget("ghostty:terminal:bede3893-ce56-4309-8aec-3d930f11225d")
	if err != nil {
		t.Fatalf("NormalizeTarget() error = %v", err)
	}
	if want := "ghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D"; target != want {
		t.Fatalf("target = %q, want %q", target, want)
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

func TestGhosttyCaseAliasesUseTheSameAppleScriptTargetIdentity(t *testing.T) {
	skipNonDarwin(t)
	const lower = "ghostty:terminal:bede3893-ce56-4309-8aec-3d930f11225d"
	const canonical = "BEDE3893-CE56-4309-8AEC-3D930F11225D"
	for _, call := range []struct {
		name string
		run  func(Ghostty) error
	}{
		{"probe", func(g Ghostty) error { return g.Probe(context.Background(), lower) }},
		{"inject", func(g Ghostty) error { return g.Inject(context.Background(), lower, "payload") }},
	} {
		runner := &fakeCommandRunner{output: []byte("ok\n")}
		if err := call.run(Ghostty{Runner: runner}); err != nil {
			t.Fatalf("%s() error = %v", call.name, err)
		}
		got := runner.calls[0].args[len(runner.calls[0].args)-1]
		if call.name == "inject" {
			got = runner.calls[0].args[len(runner.calls[0].args)-2]
		}
		if got != canonical {
			t.Fatalf("%s target id = %q, want canonical %q", call.name, got, canonical)
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
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want text then enter", len(runner.calls))
	}
	textCall := runner.calls[0]
	if got, want := textCall.args[len(textCall.args)-1], "AMQ [team-upgrader_v3]: message from claude\nline two"; got != want {
		t.Fatalf("payload arg = %q, want %q", got, want)
	}
	submitCall := runner.calls[1]
	if !strings.Contains(submitCall.args[1], `send key "enter" to targetTerminal`) {
		t.Fatalf("second call is not enter: %q", submitCall.args[1])
	}
}

func TestGhosttyScriptsFailClosedOnTerminalIDs(t *testing.T) {
	for name, script := range map[string]string{
		"probe":  ghosttyProbeScript,
		"text":   ghosttyInjectTextScript,
		"submit": ghosttyInjectSubmitScript,
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

func TestGhosttyInjectDoesNotSendEnterWhenTextFails(t *testing.T) {
	skipNonDarwin(t)
	runner := &fakeCommandRunner{results: []fakeCommandResult{
		{output: []byte("text failed"), err: errors.New("exit status 1")},
	}}
	err := (Ghostty{Runner: runner}).Inject(context.Background(), "ghostty:terminal:terminal-1", "payload")
	if err == nil || !strings.Contains(err.Error(), "text failed") {
		t.Fatalf("Inject() error = %v, want command output", err)
	}
	if errors.Is(err, ErrInjectUncertain) {
		t.Fatalf("text failure marked uncertain: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want failed text call only", len(runner.calls))
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

func TestGhosttyProbeClassifiesOnlyExplicitMissingTerminal(t *testing.T) {
	skipNonDarwin(t)
	missing := &fakeCommandRunner{output: []byte("execution error: no Ghostty terminal with id: terminal-1"), err: errors.New("exit status 1")}
	err := (Ghostty{Runner: missing}).Probe(context.Background(), "ghostty:terminal:terminal-1")
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("missing Probe() error = %v, want ErrTargetNotFound", err)
	}

	ambiguous := &fakeCommandRunner{output: []byte("accessibility denied"), err: errors.New("exit status 1")}
	err = (Ghostty{Runner: ambiguous}).Probe(context.Background(), "ghostty:terminal:terminal-1")
	if err == nil || errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("ambiguous Probe() error = %v, want non-missing failure", err)
	}
}

func skipNonDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Ghostty adapter uses macOS Accessibility")
	}
}
