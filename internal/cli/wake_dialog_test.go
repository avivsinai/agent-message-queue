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
	"regexp"
	"strings"
	"testing"
	"time"

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
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir)
	hook := func(verb, input string) {
		t.Helper()
		if err := runWakeDialog([]string{verb}, strings.NewReader(input)); err != nil {
			t.Fatalf("wake dialog %s: %v", verb, err)
		}
	}

	// The transcript is missing, so it proves nothing and the hold stays.
	hook("open", `{"hook_event_name":"PreToolUse","session_id":"s1","transcript_path":"/nonexistent/amq-test/session.jsonl","tool_name":"AskUserQuestion","tool_use_id":"toolu_1","tool_input":{"questions":[]}}`)
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
	// field: session3/claude doorbell held 13 h (2026-10-26). The wake log
	// named no marker event, kind or time, so the hold could not be traced;
	// the holding line now dates itself and names the marker.
	h.cfg.dialogHoldDetail = wakeDialogHoldDetailInDir(agentDir)
	h.send("a", format.PriorityNormal)
	_, stderr, _ := captureEnvOutput(t, func() error {
		h.scan(h.cfg)
		return nil
	})
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into an open dialog", h.injected)
	}
	if !regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ amq wake: holding the doorbell while a harness dialog is open \(marker event="PreToolUse" kind="" harness="claude" tool="AskUserQuestion" age=0s transcript=unreadable, holding: "open transcript: [^"]*"\)$`).MatchString(stderr) {
		t.Fatalf("holding diagnostic = %q, want a UTC-stamped line naming the marker event, kind, harness, tool, age and the unreadable transcript", stderr)
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

// field: agy busy turns showed held notices (2026-10-26). A working marker
// younger than wakeDialogQuietHold holds without a desktop notice or
// terminal attention; an older one shows the held notice.
func TestWakeDialogBusyTurnHoldsQuietly(t *testing.T) {
	for _, tt := range []struct {
		name    string
		age     time.Duration
		notices int
	}{
		{"younger than the quiet hold", wakeDialogQuietHold - time.Minute, 0},
		{"older than the quiet hold", wakeDialogQuietHold, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newWakeHoldHarness(t, wakeHoldPolicy{})
			agentDir, err := openWakeAgentDir(h.root, "codex")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = agentDir.Close() })
			marker := filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
			raw := `{"schema":1,"event":"OSC7501","harness":"agy","kind":"working","opened_at":"2026-10-26T00:00:00Z"}` + "\n"
			if err := os.WriteFile(marker, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			opened := h.now.Add(-tt.age)
			if err := os.Chtimes(marker, opened, opened); err != nil {
				t.Fatal(err)
			}
			var notices []string
			attention := 0
			h.cfg.dialogSource = wakeDialogSourceInDir(agentDir)
			h.cfg.dialogHoldReason = wakeDialogHoldReasonInDir(agentDir)
			h.cfg.settings.heldNotice = true
			h.cfg.desktopNotify = func(title, body string) error {
				notices = append(notices, body)
				return nil
			}
			h.cfg.attentionWrite = func(p []byte) (int, error) {
				attention++
				return len(p), nil
			}
			h.send("a", format.PriorityNormal)
			h.scan(h.cfg)
			if len(h.injected) != 0 {
				t.Fatalf("typed %q into a busy agy", h.injected)
			}
			if len(notices) != tt.notices || (attention > 0) != (tt.notices > 0) {
				t.Fatalf("notices = %q, attention writes = %d, want %d notices", notices, attention, tt.notices)
			}
			if tt.notices > 0 && !strings.Contains(notices[0], "Antigravity is busy") {
				t.Fatalf("notice = %q, want the busy reason", notices[0])
			}
		})
	}
}

// Pre-merge review of fix/wake-holds-0113 (Pro finding 8): a permission
// prompt after a quiet busy turn inherited the earlier hold's notice
// ladder, so its first notice came up to 15 minutes late. A busy to dialog
// change starts a new hold with an immediate notice.
func TestWakeDialogPromptAfterABusyTurnNoticesAtOnce(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	marker := filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
	write := func(kind string) {
		t.Helper()
		raw := `{"schema":1,"event":"OSC7501","harness":"agy","kind":"` + kind + `","opened_at":"2026-10-26T00:00:00Z"}` + "\n"
		if err := os.WriteFile(marker, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(marker, h.now, h.now); err != nil {
			t.Fatal(err)
		}
	}
	var notices []string
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir)
	h.cfg.dialogHoldReason = wakeDialogHoldReasonInDir(agentDir)
	h.cfg.settings.heldNotice = true
	h.cfg.desktopNotify = func(_, body string) error {
		notices = append(notices, body)
		return nil
	}
	h.cfg.attentionWrite = func(p []byte) (int, error) { return len(p), nil }
	write("permission")
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	write("working")
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	write("permission")
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(notices) != 2 || strings.Contains(notices[1], "busy") {
		t.Fatalf("notices = %q, want one per permission prompt", notices)
	}
}

// W1 review (Pro finding 7): a permission prompt that followed a busy turn
// the wake never sampled kept the previous prompt's notice ladder. A new
// dialog marker starts a fresh ladder; an unchanged one does not.
func TestWakeDialogNewPromptMarkerNoticesAtOnce(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	marker := filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
	write := func(opened string) {
		t.Helper()
		raw := `{"schema":1,"event":"OSC7501","harness":"agy","kind":"permission","opened_at":"` + opened + `"}` + "\n"
		if err := os.WriteFile(marker, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(marker, h.now, h.now); err != nil {
			t.Fatal(err)
		}
	}
	var notices []string
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir)
	h.cfg.dialogHoldReason = wakeDialogHoldReasonInDir(agentDir)
	h.cfg.settings.heldNotice = true
	h.cfg.desktopNotify = func(_, body string) error {
		notices = append(notices, body)
		return nil
	}
	h.cfg.attentionWrite = func(p []byte) (int, error) { return len(p), nil }
	write("2026-10-26T00:00:00.1Z")
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(notices) != 1 {
		t.Fatalf("notices for an unchanged prompt = %q, want one", notices)
	}
	write("2026-10-26T00:00:09.2Z")
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(notices) != 2 {
		t.Fatalf("notices after a new prompt = %q, want two", notices)
	}
}

// W1 review (Pro finding 6): a new busy turn kept the notice ladder of the
// prompt before it, so once the turn passed its quiet hold the notice still
// waited for the earlier, longer deadline.
func TestWakeDialogNewBusyTurnStartsAFreshNoticeLadder(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	marker := filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
	write := func(kind string) {
		t.Helper()
		raw := `{"schema":1,"event":"OSC7501","harness":"agy","kind":"` + kind + `","opened_at":"2026-10-26T00:00:00Z"}` + "\n"
		if err := os.WriteFile(marker, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(marker, h.now, h.now); err != nil {
			t.Fatal(err)
		}
	}
	var notices []string
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir)
	h.cfg.dialogHoldReason = wakeDialogHoldReasonInDir(agentDir)
	h.cfg.settings.heldNotice = true
	h.cfg.desktopNotify = func(_, body string) error {
		notices = append(notices, body)
		return nil
	}
	h.cfg.attentionWrite = func(p []byte) (int, error) { return len(p), nil }
	write("permission")
	h.send("a", format.PriorityNormal)
	h.scan(h.cfg)
	// The prompt has reached the longest notice interval.
	h.cfg.doorbell.nextHeldNotice = h.now.Add(wakeDoorbellAttentionRetryMax)
	write("working")
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	h.advance(wakeDialogQuietHold)
	h.scan(h.cfg)
	if len(notices) != 2 || !strings.Contains(notices[1], "busy") {
		t.Fatalf("notices = %q, want the busy notice once the turn passes its quiet hold", notices)
	}
}

// Pre-merge review of fix/wake-holds-0113 (lead's W1 brief): the Cursor
// /rename namer typed its command and Enter after a fixed delay, while a
// startup or dialog marker could hold the doorbell. It now waits for the
// marker as the doorbell does.
func TestCoopNamedInjectWaitsForTheDialogMarker(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "cur"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fsq.AgentBase(root, "cur"), wakeDialogFileName)
	if err := os.WriteFile(marker, []byte(`{"schema":1,"event":"Startup","harness":"cursor"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if waitCoopNamedInputFree(root, "cur", 50*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("the namer may type while the Startup marker holds")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if !waitCoopNamedInputFree(root, "cur", 50*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("the namer still waits after the marker is gone")
	}
}

// field: session3/claude doorbell held 13 h (2026-10-26). An
// AskUserQuestion marker its own PreToolUse wrote closes at that call's
// PostToolUse, or when Claude Code writes a tool_result for its
// tool_use_id to the transcript (Esc runs no hook). Pre-merge review of
// fix/wake-holds-0113 (Pro findings 1 and 2): an equal call's PostToolUse,
// or the newest equal tool_use in the transcript, released a permission
// dialog that still showed. A permission marker has no tool_use_id, so
// only a turn or session event closes it.
func TestWakeDialogClaudeToolCallClosesItsMarker(t *testing.T) {
	const (
		permission = `{"hook_event_name":"PermissionRequest","session_id":"s1","transcript_path":"TRANSCRIPT","tool_name":"Bash","tool_input":{"command":"date > a.txt","description":"d"}}`
		askPre     = `{"hook_event_name":"PreToolUse","session_id":"s1","transcript_path":"TRANSCRIPT","tool_name":"AskUserQuestion","tool_use_id":"toolu_q","tool_input":{"questions":[{"question":"Color?"}]}}`
		askPerm    = `{"hook_event_name":"PermissionRequest","session_id":"s1","transcript_path":"TRANSCRIPT","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Color?"}]}}`
		askPost    = `{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"AskUserQuestion","tool_use_id":"toolu_q","tool_input":{"questions":[{"question":"Color?"}],"answers":{"Color?":"Red"}}}`
		equalPost  = `{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"Bash","tool_use_id":"toolu_2","tool_input":{"command":"date > a.txt","description":"d"},"tool_response":{}}`
		askEsc     = `{"type":"user","toolUseResult":"User rejected tool use","message":{"content":[{"type":"tool_result","is_error":true,"tool_use_id":"toolu_q"}]}}`
		equalUse   = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"date > a.txt","description":"d"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"tool_use_id":"toolu_2"}]}}
{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}`
	)
	type step struct{ verb, input string }
	for _, tc := range []struct {
		name  string
		steps []step
		// transcript is the session transcript after the steps.
		transcript string
		closed     bool
	}{
		{"answered AskUserQuestion", []step{{"open", askPre}, {"close", askPost}}, "", true},
		{"Esc on AskUserQuestion", []step{{"open", askPre}}, askEsc, true},
		// W1 review (Pro finding 3, Codex reproducer): a PermissionRequest
		// took the tool_use_id of the marker it replaced by tool name
		// alone, so another call's PostToolUse released its dialog.
		{"another call's PostToolUse after a PermissionRequest", []step{{"open", askPre}, {"open", askPerm}, {"close", askPost}}, "", false},
		// W1 review (Pro finding 4): a tool call's open replaced a present
		// permission marker after an unlocked check, so that call's
		// PostToolUse released the permission dialog.
		{"a tool call's open after a permission dialog", []step{{"open", permission}, {"open", askPre}, {"close", askPost}}, "", false},
		{"an equal call's PostToolUse after a permission dialog", []step{{"open", permission}, {"close", equalPost}}, "", false},
		{"an equal call's result in the transcript after a permission dialog", []step{{"open", permission}}, equalUse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
				t.Fatal(err)
			}
			clearSendMailboxTestEnv(t)
			t.Setenv(envMe, "claude")
			t.Setenv(envRoot, root)
			transcript := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(transcript, []byte(tc.transcript+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, step := range tc.steps {
				input := strings.ReplaceAll(step.input, "TRANSCRIPT", transcript)
				if err := runWakeDialog([]string{step.verb}, strings.NewReader(input)); err != nil {
					t.Fatalf("wake dialog %s: %v", step.verb, err)
				}
			}
			agentDir, err := openWakeAgentDir(root, "claude")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = agentDir.Close() })
			state, err := wakeDialogSourceInDir(agentDir)(time.Now())
			if got := err == nil && state == wakeDialogClosed; got != tc.closed {
				t.Fatalf("marker closed = %v (%v), want %v", got, err, tc.closed)
			}
		})
	}
}

// fakeClaudeOnPath puts a claude on PATH that prints version for
// --version, the boundary install-dialog-hook and doctor read.
func fakeClaudeOnPath(t *testing.T, version string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + version + " (Claude Code)'\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestWakeInstallDialogHookWritesEveryEventOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeClaudeOnPath(t, "2.1.310")
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
		{"PostToolUseFailure", "AskUserQuestion|ExitPlanMode", "close", 5},
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

	// field: session3/claude doorbell held 13 h (2026-10-26) follow-up.
	// Before Claude Code 2.1.101 an unknown hook event made it ignore the
	// whole settings file, so a Claude Code older than an event's first
	// version gets no entry for it.
	home = t.TempDir()
	t.Setenv("HOME", home)
	fakeClaudeOnPath(t, "2.1.118")
	settings.Hooks = nil
	if err := json.Unmarshal(install(), &settings); err != nil {
		t.Fatal(err)
	}
	if groups, ok := settings.Hooks["PostToolUseFailure"]; ok {
		t.Errorf("Claude Code 2.1.118 got a PostToolUseFailure entry: %+v", groups)
	}
	if len(settings.Hooks["PostToolUse"]) != 1 {
		t.Errorf("Claude Code 2.1.118 PostToolUse = %+v, want one entry", settings.Hooks["PostToolUse"])
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
		// Native startup needs its positive hook even if another handler
		// removed the marker before this handler runs.
		{"native launch without marker", []string{"AM_ME=codex", "AM_ROOT=" + root, envClaudeNativeLaunch + "=bound"}, false, true, true},
		{"participant with marker", []string{"AM_ME=codex", "AM_ROOT=" + root}, true, true, true},
		{"participant without AM_ROOT", []string{"AM_ME=codex"}, false, true, true},
		{"padded AM_ROOT, marker in the trimmed root", []string{"AM_ME=codex", "AM_ROOT=" + root + " "}, true, true, true},
		{"AM_ROOT with .. through a symlink", []string{"AM_ME=codex", "AM_ROOT=" + dotted}, true, true, true},
	} {
		for _, hook := range wakeDialogHooks(fake, wakeDialogHookEvents) {
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
			if wakeDialogHookVerb(hook.Event) != "close" {
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
		hooks := wakeDialogHooks(bin, wakeDialogHookEvents)
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
		{"missing binary", wakeDialogHooks(filepath.Join(t.TempDir(), "gone", "amq"), wakeDialogHookEvents), "missing amq binary"},
		{"current", wakeDialogHooks(bin, wakeDialogHookEvents), "installed"},
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		fakeClaudeOnPath(t, "2.1.310")
		if err := claude.InstallCommandHooks(home, tc.hooks); err != nil {
			t.Fatal(err)
		}
		check, ok := checkClaudeDialogHook()
		if !ok || !strings.Contains(check.Message, tc.want) {
			t.Errorf("%s: doctor = %+v, want message with %q", tc.name, check, tc.want)
		}
	}
}
