//go:build darwin || linux

package cli

// Purpose: guards the wake dialog guard (#1054). The regression test drives
// the real hook receiver and the real marker reader against the hold
// harness; it breaks if the wake types while a Claude Code dialog is open,
// if the hold spends the reminder budget, or if the doorbell does not ring
// in full after the dialog closes. The installer test breaks if
// install-dialog-hook misses an event or matcher, or is not idempotent.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// #1054 (field, 2026-10-13): an injected doorbell answered an open
// AskUserQuestion dialog in Claude Code.
func TestWakeDialogHoldsDoorbellUntilDialogCloses(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	clearSendMailboxTestEnv(t)
	t.Setenv(envMe, "codex")
	t.Setenv(envRoot, h.root)
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir)
	hook := func(verb, input string) {
		t.Helper()
		if err := runWakeDialog([]string{verb}, strings.NewReader(input)); err != nil {
			t.Fatalf("wake dialog %s: %v", verb, err)
		}
	}

	hook("open", `{"hook_event_name":"PreToolUse","session_id":"s1","tool_name":"AskUserQuestion","tool_use_id":"toolu_1","tool_input":{"questions":[]}}`)
	// The dialog opened at the harness clock's now.
	marker := filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatal(err)
	}
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into an open dialog", h.injected)
	}
	if h.cfg.doorbell.reminderAttempts != 0 || h.cfg.doorbell.presentationConfirmed {
		t.Fatalf("hold spent the doorbell: reminderAttempts=%d presentationConfirmed=%v",
			h.cfg.doorbell.reminderAttempts, h.cfg.doorbell.presentationConfirmed)
	}
	if got, want := h.deadline(), h.now.Add(wakeDialogRecheck); !got.Equal(want) {
		t.Fatalf("recheck deadline = %v, want %v", got, want)
	}

	hook("close", `{"hook_event_name":"Stop","session_id":"s1"}`)
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells after the dialog closed = %d, want 1", got)
	}
}

func TestWakeInstallDialogHookWritesEveryEventOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	install := func() []byte {
		t.Helper()
		if _, err := captureStdout(t, func() error {
			return runWake([]string{"install-dialog-hook"})
		}); err != nil {
			t.Fatalf("install-dialog-hook: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	first := install()

	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(first, &settings); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ event, matcher, verb string }{
		{"PreToolUse", "AskUserQuestion|ExitPlanMode", "open"},
		{"PermissionRequest", "", "open"},
		{"PostToolUse", "AskUserQuestion|ExitPlanMode", "close"},
		{"UserPromptSubmit", "", "close"},
		{"Stop", "", "close"},
		{"SessionStart", "", "close"},
		{"SessionEnd", "", "close"},
	} {
		groups := settings.Hooks[want.event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s groups = %+v, want one group with one hook", want.event, groups)
		}
		command := groups[0].Hooks[0].Command
		if groups[0].Matcher != want.matcher ||
			!strings.HasPrefix(command, wakeDialogHookMarker) ||
			!strings.HasSuffix(command, " wake dialog "+want.verb+" || true") {
			t.Fatalf("%s entry = matcher %q command %q, want matcher %q and wake dialog %s",
				want.event, groups[0].Matcher, command, want.matcher, want.verb)
		}
	}

	if second := install(); string(second) != string(first) {
		t.Fatalf("second install changed settings:\n%s\nwant:\n%s", second, first)
	}
}
