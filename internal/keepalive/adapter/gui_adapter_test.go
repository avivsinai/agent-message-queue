package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDefaultRegistryKeepsGUIAdaptersGated(t *testing.T) {
	registry := DefaultRegistry()
	for _, name := range []string{"codex-app", "claude-desktop"} {
		if _, err := registry.Get(name); err == nil {
			t.Fatalf("DefaultRegistry().Get(%q) succeeded before the GUI capability gate", name)
		}
	}
}

func TestCodexAppSkeletonRefusesEveryLiveOperationBeforeTask0(t *testing.T) {
	app := CodexApp{}
	if got, err := app.NormalizeTarget(" codex-app:tab:window-1/tab-1 "); err != nil || got != "codex-app:tab:window-1/tab-1" {
		t.Fatalf("NormalizeTarget() = %q, %v; want stable target", got, err)
	}
	if _, err := app.Discover(context.Background()); !errors.Is(err, ErrGUIAdapterNotReady) {
		t.Fatalf("Discover() error = %v, want task-0 refusal", err)
	}
	if err := app.Probe(context.Background(), "codex-app:tab:window-1/tab-1"); !errors.Is(err, ErrGUIAdapterNotReady) {
		t.Fatalf("Probe() error = %v, want task-0 refusal", err)
	}
	if err := app.Inject(context.Background(), "codex-app:tab:window-1/tab-1", "payload"); !errors.Is(err, ErrGUIAdapterNotReady) {
		t.Fatalf("Inject() error = %v, want task-0 refusal", err)
	}
}

func TestCodexAppTargetRejectsUnpinnedOrUnsafeIdentity(t *testing.T) {
	for _, target := range []string{
		"",
		"ChatGPT",
		"codex-app:tab:",
		"codex-app:tab:window-1/tab\n1",
	} {
		if _, err := (CodexApp{}).NormalizeTarget(target); err == nil {
			t.Fatalf("NormalizeTarget(%q) succeeded; want refusal", target)
		}
	}
}

func TestClaudeDesktopSkeletonRefusesEveryLiveOperation(t *testing.T) {
	app := ClaudeDesktop{}
	if got, err := app.NormalizeTarget(" claude-desktop:new "); err != nil || got != claudeDesktopTarget {
		t.Fatalf("NormalizeTarget() = %q, %v; want new-session target", got, err)
	}
	if _, err := app.Discover(context.Background()); !errors.Is(err, ErrGUIAdapterNotReady) {
		t.Fatalf("Discover() error = %v, want capability refusal", err)
	}
	if err := app.Probe(context.Background(), claudeDesktopTarget); !errors.Is(err, ErrGUIAdapterNotReady) {
		t.Fatalf("Probe() error = %v, want capability refusal", err)
	}
	if err := app.Inject(context.Background(), claudeDesktopTarget, "payload"); !errors.Is(err, ErrGUIAdapterNotReady) {
		t.Fatalf("Inject() error = %v, want capability refusal", err)
	}
}

func TestGUIAppleScriptUsesNoForbiddenGenericAutomation(t *testing.T) {
	for _, forbidden := range []string{"System Events", "keystroke", "the clipboard", "AXRaise", "activate"} {
		if strings.Contains(codexAppTask0AppleScript, forbidden) {
			t.Fatalf("Codex app task-0 script contains forbidden %q", forbidden)
		}
	}
	if !strings.Contains(codexAppTask0AppleScript, "execute javascript") {
		t.Fatal("Codex app task-0 script does not use execute javascript")
	}
	if !strings.Contains(codexAppTask0AppleScript, "AMQ_CODEX_APP_EXECUTE_JS_TASK_0") {
		t.Fatal("Codex app task-0 script does not use the fixed probe result")
	}
}
