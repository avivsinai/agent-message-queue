//go:build darwin || linux

package cli

// Purpose: guards the wake dialog guard (#1054). The regression test drives
// the real hook receiver and the real marker reader against the hold
// harness; it breaks if the wake types while a Claude Code dialog is open,
// if the hold spends the reminder budget, or if the doorbell does not ring
// in full after the dialog closes. The installer test breaks if
// install-dialog-hook misses an event, matcher or timeout, or is not
// idempotent. The gate test runs the installed command strings under /bin/sh,
// the boundary where the defect lived.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// Regression: #1054, an injected doorbell answered an open
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
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir, &wakeStartupRecheck{})
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
	// field: held Codex doorbell invisible for 2.4 h (2026-10-25). The
	// first held attempt shows the desktop notice at once, not after
	// wakeDialogQuietHold; an unchanged count stays quiet within the
	// attention period, and a new message shows it again.
	var notices []string
	h.cfg.settings.heldNotice = true
	h.cfg.dialogHoldReason = wakeDialogHoldReasonInDir(agentDir)
	h.cfg.desktopNotify = func(title, body string) error {
		notices = append(notices, title+": "+body)
		return nil
	}
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into an open dialog", h.injected)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "AMQ: codex: 1 message waiting") ||
		!strings.Contains(notices[0], "Claude Code dialog open") {
		t.Fatalf("notices on the first held attempt = %q, want one for codex's 1 message", notices)
	}
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(notices) != 1 {
		t.Fatalf("notices within the attention period = %q, want still one", notices)
	}
	// post-release review v0.113.2 finding W1: each new message showed
	// another notice at once, with no cap. Only the attention ladder paces
	// the notice.
	h.send("b", format.PriorityNormal)
	h.scan(h.cfg)
	if len(notices) != 1 {
		t.Fatalf("notices after a new message within the attention period = %q, want still one", notices)
	}
	h.advance(wakeDoorbellAttentionRetryBase)
	h.scan(h.cfg)
	if len(notices) != 2 || !strings.Contains(notices[1], "2 messages waiting") {
		t.Fatalf("notices once the attention period ends = %q, want a second for 2 messages", notices)
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
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(first, &settings); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		event, matcher, verb string
		timeout              int
	}{
		{"PreToolUse", "AskUserQuestion|ExitPlanMode", "open", 5},
		{"PermissionRequest", "", "open", 5},
		{"PostToolUse", "AskUserQuestion|ExitPlanMode", "close", 5},
		{"UserPromptSubmit", "", "close", 5},
		{"Stop", "", "close", 5},
		{"SessionStart", "", "close", 5},
		{"SessionEnd", "", "close", 1},
	} {
		groups := settings.Hooks[want.event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s groups = %+v, want one group with one hook", want.event, groups)
		}
		command := groups[0].Hooks[0].Command
		if groups[0].Matcher != want.matcher ||
			groups[0].Hooks[0].Timeout != want.timeout ||
			!strings.HasPrefix(command, wakeDialogHookMarker) ||
			!strings.HasSuffix(command, " wake dialog "+want.verb+" || true") {
			t.Fatalf("%s entry = matcher %q timeout %d command %q, want matcher %q, timeout %d and wake dialog %s",
				want.event, groups[0].Matcher, groups[0].Hooks[0].Timeout, command, want.matcher, want.timeout, want.verb)
		}
	}

	if second := install(); string(second) != string(first) {
		t.Fatalf("second install changed settings:\n%s\nwant:\n%s", second, first)
	}
}

// Regression: hook audit inventory #7, every Claude Code session started
// amq through the dialog guard on each prompt, stop, and session start and
// end, with or without AM_ME.
func TestWakeDialogHookStartsAmqOnlyForAParticipantWithWork(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	fake := filepath.Join(dir, "amq")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch \"$RAN\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	agent := fsq.AgentBase(root, "codex")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(agent, wakeDialogFileName)
	// Regression (#1165 review): the receiver trims AM_ROOT and cleans it
	// lexically, so a padded root or one with ".." through a symlink names
	// the marker's directory for amq but an empty directory for sh.
	if err := os.MkdirAll(fsq.AgentBase(root+" ", "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fsq.AgentBase(filepath.Join(dir, "a", "root"), "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "a", "b"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	dotted := filepath.Join(dir, "link") + "/../root"

	for _, tc := range []struct {
		name       string
		env        []string
		marker     bool
		open, shut bool // whether the open and close commands start amq
	}{
		{"no AM_ME", nil, true, false, false},
		{"participant without marker", []string{"AM_ME=codex", "AM_ROOT=" + root}, false, true, false},
		{"participant with marker", []string{"AM_ME=codex", "AM_ROOT=" + root}, true, true, true},
		{"participant without AM_ROOT", []string{"AM_ME=codex"}, false, true, true},
		{"padded AM_ROOT, marker in the trimmed root", []string{"AM_ME=codex", "AM_ROOT=" + root + " "}, true, true, true},
		{"AM_ROOT with .. through a symlink", []string{"AM_ME=codex", "AM_ROOT=" + dotted}, true, true, true},
	} {
		for _, hook := range wakeDialogHooks(fake) {
			_ = os.Remove(ran)
			_ = os.Remove(marker)
			if tc.marker {
				if err := os.WriteFile(marker, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("/bin/sh", "-c", hook.Command)
			cmd.Dir = dir
			cmd.Env = append([]string{"PATH=/usr/bin:/bin", "RAN=" + ran}, tc.env...)
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"` + hook.Event + `"}`)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s %s: %v: %s", tc.name, hook.Event, err, out)
			}
			_, err := os.Stat(ran)
			started := err == nil
			want := tc.shut
			if wakeDialogHookVerb(hook.Event) == "open" {
				want = tc.open
			}
			if started != want {
				t.Errorf("%s %s: started amq = %v, want %v", tc.name, hook.Event, started, want)
			}
		}
	}
}

// Regression: hook audit, the doctor regex read the binary only right after
// the marker, so a new-form hook with a missing binary passed as installed,
// and an old form passed without a remedy.
func TestDoctorReportsStaleOrBrokenDialogHook(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	oldForm := func(bin string) []claude.CommandHook {
		hooks := wakeDialogHooks(bin)
		for i := range hooks {
			hooks[i].Command = wakeDialogHookMarker + "'" + bin + "' --no-update-check wake dialog " +
				wakeDialogHookVerb(hooks[i].Event) + " || true"
		}
		return hooks
	}
	for _, tc := range []struct {
		name  string
		hooks []claude.CommandHook
		want  string
	}{
		{"old form", oldForm(bin), "stale form"},
		{"missing binary", wakeDialogHooks(filepath.Join(t.TempDir(), "gone", "amq")), "missing amq binary"},
		{"current", wakeDialogHooks(bin), "installed"},
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := claude.InstallCommandHooks(home, tc.hooks); err != nil {
			t.Fatal(err)
		}
		check, ok := checkClaudeDialogHook()
		if !ok || !strings.Contains(check.Message, tc.want) {
			t.Errorf("%s: doctor = %+v, want message with %q", tc.name, check, tc.want)
		}
	}
}
