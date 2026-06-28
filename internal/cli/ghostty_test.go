package cli

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

type ghosttyCommandCall struct {
	name string
	args []string
}

func stubGhosttyCommand(t *testing.T, fn func(context.Context, string, ...string) ([]byte, error)) *[]ghosttyCommandCall {
	t.Helper()
	var calls []ghosttyCommandCall
	old := runGhosttyCommand
	runGhosttyCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, ghosttyCommandCall{name: name, args: append([]string{}, args...)})
		return fn(ctx, name, args...)
	}
	t.Cleanup(func() {
		runGhosttyCommand = old
	})
	return &calls
}

func TestNormalizeGhosttyTerminalIDAcceptsTargetPrefix(t *testing.T) {
	got, err := normalizeGhosttyTerminalID(" ghostty:terminal:terminal-1 ")
	if err != nil {
		t.Fatalf("normalizeGhosttyTerminalID: %v", err)
	}
	if got != "terminal-1" {
		t.Fatalf("id = %q, want terminal-1", got)
	}
}

func TestNormalizeGhosttyTerminalIDRejectsEmptyAndNUL(t *testing.T) {
	if _, err := normalizeGhosttyTerminalID("ghostty:terminal:"); err == nil {
		t.Fatal("expected empty id error")
	}
	if _, err := normalizeGhosttyTerminalID("terminal\x00id"); err == nil {
		t.Fatal("expected NUL id error")
	}
}

func TestInjectGhosttyTrimsTrailingCRLFAndTargetsTerminal(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Ghostty AppleScript injection is macOS-only")
	}
	calls := stubGhosttyCommand(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("ok"), nil
	})

	if err := injectGhostty(context.Background(), "terminal-1", "payload\r\n\n"); err != nil {
		t.Fatalf("injectGhostty: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	call := (*calls)[0]
	if call.name != "osascript" {
		t.Fatalf("command = %q, want osascript", call.name)
	}
	if got := call.args[len(call.args)-2]; got != "terminal-1" {
		t.Fatalf("terminal arg = %q, want terminal-1", got)
	}
	if got := call.args[len(call.args)-1]; got != "payload" {
		t.Fatalf("payload arg = %q, want payload", got)
	}
	if !strings.Contains(call.args[1], "input text payload to targetTerminal") {
		t.Fatalf("script does not input text to target terminal: %q", call.args[1])
	}
	if !strings.Contains(call.args[1], `send key "enter" to targetTerminal`) {
		t.Fatalf("script does not submit target terminal: %q", call.args[1])
	}
}

func TestGhosttyScriptsCheckUniqueTerminalID(t *testing.T) {
	for name, script := range map[string]string{
		"probe":  ghosttyProbeScript,
		"inject": ghosttyInjectScript,
	} {
		if !strings.Contains(script, "set matches to terminals whose id is targetID") {
			t.Fatalf("%s script does not query by terminal id", name)
		}
		if !strings.Contains(script, "count of matches") {
			t.Fatalf("%s script does not count matches", name)
		}
		if !strings.Contains(script, "no Ghostty terminal with id") {
			t.Fatalf("%s script does not report missing terminal", name)
		}
		if !strings.Contains(script, "ambiguous Ghostty terminal id") {
			t.Fatalf("%s script does not report ambiguous terminal", name)
		}
	}
}

func TestProbeGhosttyTerminalIDPropagatesScriptFailure(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Ghostty AppleScript probing is macOS-only")
	}
	stubGhosttyCommand(t, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("no Ghostty terminal with id: terminal-1"), errors.New("exit status 1")
	})

	err := probeGhosttyTerminalID(context.Background(), "terminal-1")
	if err == nil {
		t.Fatal("expected probe failure")
	}
	if !strings.Contains(err.Error(), "no Ghostty terminal with id") {
		t.Fatalf("unexpected error: %v", err)
	}
}
