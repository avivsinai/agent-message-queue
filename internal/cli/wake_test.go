package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestIsInterruptMessage(t *testing.T) {
	cfg := &wakeConfig{
		interrupt:         true,
		interruptLabel:    "interrupt",
		interruptPriority: "urgent",
	}

	if !isInterruptMessage(wakeMsgInfo{priority: "urgent", labels: []string{"interrupt"}}, cfg) {
		t.Fatalf("expected interrupt message to match")
	}
	if isInterruptMessage(wakeMsgInfo{priority: "normal", labels: []string{"interrupt"}}, cfg) {
		t.Fatalf("expected priority mismatch to not match")
	}
	if isInterruptMessage(wakeMsgInfo{priority: "urgent", labels: []string{"other"}}, cfg) {
		t.Fatalf("expected label mismatch to not match")
	}
}

func TestBuildInterruptText_DefaultSingle(t *testing.T) {
	msg := wakeMsgInfo{from: "alice", subject: "hello world"}
	text := buildInterruptText("", []wakeMsgInfo{msg}, map[string]int{"alice": 1}, 48, "")
	if !strings.Contains(text, "AMQ interrupt") {
		t.Fatalf("expected interrupt prefix, got: %s", text)
	}
	if !strings.Contains(text, "alice") {
		t.Fatalf("expected sender in text, got: %s", text)
	}
}

func TestBuildNotificationText_NoSession(t *testing.T) {
	msg := wakeMsgInfo{from: "codex", subject: "review done"}
	text := buildNotificationText("", []wakeMsgInfo{msg}, map[string]int{"codex": 1}, 48)
	if !strings.HasPrefix(text, "AMQ: ") {
		t.Fatalf("expected 'AMQ: ' prefix without session, got: %s", text)
	}
	if strings.Contains(text, "[") {
		t.Fatalf("expected no brackets without session, got: %s", text)
	}
}

func TestBuildNotificationText_WithSession(t *testing.T) {
	msg := wakeMsgInfo{from: "codex", subject: "review done"}
	text := buildNotificationText("stream3", []wakeMsgInfo{msg}, map[string]int{"codex": 1}, 48)
	if !strings.HasPrefix(text, "AMQ [stream3]: ") {
		t.Fatalf("expected 'AMQ [stream3]: ' prefix, got: %s", text)
	}
	if !strings.Contains(text, "codex") {
		t.Fatalf("expected sender in text, got: %s", text)
	}
}

func TestBuildNotificationText_MultipleWithSession(t *testing.T) {
	messages := []wakeMsgInfo{
		{from: "codex", subject: "a"},
		{from: "codex", subject: "b"},
		{from: "alice", subject: "c"},
	}
	counts := map[string]int{"codex": 2, "alice": 1}
	text := buildNotificationText("collab", messages, counts, 48)
	if !strings.HasPrefix(text, "AMQ [collab]: ") {
		t.Fatalf("expected 'AMQ [collab]: ' prefix, got: %s", text)
	}
	if !strings.Contains(text, "3 messages") {
		t.Fatalf("expected message count, got: %s", text)
	}
}

func TestBuildInterruptText_WithSession(t *testing.T) {
	msg := wakeMsgInfo{from: "alice", subject: "help"}
	text := buildInterruptText("stream2", []wakeMsgInfo{msg}, map[string]int{"alice": 1}, 48, "")
	if !strings.HasPrefix(text, "AMQ interrupt [stream2]: ") {
		t.Fatalf("expected 'AMQ interrupt [stream2]: ' prefix, got: %s", text)
	}
}

func TestBuildInterruptText_CustomOverride(t *testing.T) {
	msg := wakeMsgInfo{from: "alice", subject: "help"}
	text := buildInterruptText("stream2", []wakeMsgInfo{msg}, map[string]int{"alice": 1}, 48, "custom notice")
	if text != "custom notice" {
		t.Fatalf("expected custom override, got: %s", text)
	}
}

func TestNotificationPrefix(t *testing.T) {
	if got := notificationPrefix("AMQ", ""); got != "AMQ" {
		t.Fatalf("expected 'AMQ', got: %s", got)
	}
	if got := notificationPrefix("AMQ", "collab"); got != "AMQ [collab]" {
		t.Fatalf("expected 'AMQ [collab]', got: %s", got)
	}
	if got := notificationPrefix("[AMQ]", "stream3"); got != "[AMQ] [stream3]" {
		t.Fatalf("expected '[AMQ] [stream3]', got: %s", got)
	}
}

const injectViaHelperEnv = "AMQ_TEST_INJECT_VIA_HELPER"

func injectViaCaptureConfig(t *testing.T, fixedArgs ...string) (*wakeConfig, string) {
	t.Helper()

	t.Setenv(injectViaHelperEnv, "1")
	outputPath := filepath.Join(secureTempDirForTest(t), "inject output.txt")
	args := []string{"-test.run=^TestInjectViaHelperProcess$", "--", outputPath}
	args = append(args, fixedArgs...)

	return &wakeConfig{
		injectVia:     copyTestBinaryForInjectVia(t),
		injectArgs:    args,
		injectTimeout: 60 * time.Second,
		debug:         false,
	}, outputPath
}

func copyTestBinaryForInjectVia(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	path := filepath.Join(secureTempDirForTest(t), "inject-via-helper")
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatalf("write inject-via helper: %v", err)
	}
	return path
}

func TestInjectViaHelperProcess(t *testing.T) {
	if os.Getenv(injectViaHelperEnv) != "1" {
		return
	}

	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		_, _ = os.Stderr.WriteString("missing inject-via helper output path\n")
		os.Exit(2)
	}
	outputPath := os.Args[separator+1]
	payload := strings.Join(os.Args[separator+2:], "\n")
	if err := os.WriteFile(outputPath, []byte(payload), 0o600); err != nil {
		_, _ = os.Stderr.WriteString("write inject-via helper output: " + err.Error() + "\n")
		os.Exit(3)
	}
	os.Exit(0)
}

func TestInjectNotification_InjectVia(t *testing.T) {
	cfg, outputPath := injectViaCaptureConfig(t)

	text := "AMQ [collab]: message from codex - hello"
	if err := injectNotification(cfg, text, true); err != nil {
		t.Fatalf("injectNotification failed: %v", err)
	}

	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output: %v", err)
	}
	if string(got) != text {
		t.Fatalf("expected %q, got %q", text, string(got))
	}
}

func TestInjectNotification_InjectVia_MultiWordCommand(t *testing.T) {
	// Simulates: ghostty-bridge exec "Team Alpha" <text>.
	cfg, outputPath := injectViaCaptureConfig(t, "exec", "Team Alpha")

	text := "AMQ [collab]: test message"
	if err := injectNotification(cfg, text, true); err != nil {
		t.Fatalf("injectNotification failed: %v", err)
	}

	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output: %v", err)
	}
	expected := "exec\nTeam Alpha\n" + text
	if string(got) != expected {
		t.Fatalf("expected %q, got %q", expected, string(got))
	}
}

func TestInjectNotification_InjectVia_Bell(t *testing.T) {
	cfg, outputPath := injectViaCaptureConfig(t)
	cfg.bell = true

	text := "AMQ [collab]: message from codex - hello"
	if err := injectNotification(cfg, text, true); err != nil {
		t.Fatalf("injectNotification failed: %v", err)
	}

	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("failed to read output: %v", err)
	}
	expected := "\a" + text
	if string(got) != expected {
		t.Fatalf("expected %q, got %q", expected, string(got))
	}
}

func TestInjectNotificationNoneWritesOutputWithoutTIOCSTI(t *testing.T) {
	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		injected = append(injected, text)
		return errors.New("none mode must not inject")
	})

	stderr := captureWakeStderr(t, func() {
		cfg := &wakeConfig{injectMode: wakeInjectModeNone, bell: true}
		if err := injectNotification(cfg, "safe notice", true); err != nil {
			t.Fatalf("injectNotification: %v", err)
		}
	})

	if len(injected) != 0 {
		t.Fatalf("none mode injected terminal input: %q", injected)
	}
	if stderr != "\asafe notice\n" {
		t.Fatalf("stderr = %q, want bell + notice", stderr)
	}
}

func TestInjectNotificationNoneDoesNotInvokeInjectVia(t *testing.T) {
	cfg, outputPath := injectViaCaptureConfig(t)
	cfg.injectMode = wakeInjectModeNone
	cfg.bell = true

	stderr := captureWakeStderr(t, func() {
		if err := injectNotification(cfg, "safe notice", true); err != nil {
			t.Fatalf("injectNotification: %v", err)
		}
	})

	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("none mode invoked inject-via; output stat error = %v", err)
	}
	if stderr != "\asafe notice\n" {
		t.Fatalf("stderr = %q, want bell + notice", stderr)
	}
}

func TestInjectViaTimeout(t *testing.T) {
	scriptPath := filepath.Join(secureTempDirForTest(t), "sleep.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nsleep 2\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cfg := &wakeConfig{
		injectVia:     scriptPath,
		injectTimeout: 10 * time.Millisecond,
	}

	err := injectVia(cfg, "test")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestInjectNotification_InjectVia_Failure(t *testing.T) {
	cfg := &wakeConfig{
		injectVia: "/nonexistent/command",
		debug:     false,
	}

	// Stderr remains diagnostic output, but the failed external injector must
	// not acknowledge delivery or make the message ineligible for retry.
	if err := injectNotification(cfg, "test", true); err == nil {
		t.Fatal("expected failed external injection to remain retryable")
	}
}

func TestInjectNotification_InjectVia_FailureWarnsOnce(t *testing.T) {
	cfg := &wakeConfig{
		injectVia:    "/nonexistent/command",
		fallbackWarn: true,
	}

	text := "fallback notice"
	stderr := captureWakeStderr(t, func() {
		if err := injectNotification(cfg, text, true); err == nil {
			t.Fatal("first injectNotification unexpectedly succeeded")
		}
		if err := injectNotification(cfg, text, true); err == nil {
			t.Fatal("second injectNotification unexpectedly succeeded")
		}
	})

	if got := strings.Count(stderr, "amq wake: --inject-via failed:"); got != 1 {
		t.Fatalf("expected one inject-via failure warning, got %d in %q", got, stderr)
	}
	if got := strings.Count(stderr, "amq wake: falling back to stderr notification\n"); got != 1 {
		t.Fatalf("expected one fallback warning, got %d in %q", got, stderr)
	}
	if got := strings.Count(stderr, text+"\n"); got != 2 {
		t.Fatalf("expected fallback text twice, got %d in %q", got, stderr)
	}
}

func captureWakeStderr(t *testing.T, fn func()) string {
	t.Helper()

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = oldStderr
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close stderr reader: %v", err)
	}
	return string(out)
}

func stubTIOCSTIInject(t *testing.T, fn func(string) error) {
	t.Helper()
	old := tiocstiInject
	tiocstiInject = fn
	t.Cleanup(func() {
		tiocstiInject = old
	})
}

func stubRawInputDrained(t *testing.T, fn func(time.Duration, time.Duration) (time.Duration, bool, error)) {
	t.Helper()
	old := waitForRawInputDrained
	waitForRawInputDrained = fn
	t.Cleanup(func() {
		waitForRawInputDrained = old
	})
}

func stubRawInjectSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	old := rawInjectSleep
	rawInjectSleep = func(d time.Duration) {
		slept = append(slept, d)
	}
	t.Cleanup(func() {
		rawInjectSleep = old
	})
	return &slept
}

func TestRawInjectSettleDelayClearsCodexEnterSuppressWindow(t *testing.T) {
	// Regression guard for the v0.41.0 Enter swallow: the settle (and rescue
	// spacing, which reuses it) must exceed codex-tui's Enter-suppress window,
	// or injected CRs are inserted as pasted newlines instead of submitting.
	// A revert to the old 50ms floor must fail this test, not just review.
	if rawInjectSettleDelay <= codexTUIEnterSuppressWindow {
		t.Fatalf("rawInjectSettleDelay = %s, must exceed codex-tui Enter-suppress window %s",
			rawInjectSettleDelay, codexTUIEnterSuppressWindow)
	}
	if margin := rawInjectSettleDelay - codexTUIEnterSuppressWindow; margin < 20*time.Millisecond {
		t.Fatalf("settle margin over suppress window = %s, want >= 20ms for scheduler jitter", margin)
	}
}

func TestRawSubmitPreludePicksByTarget(t *testing.T) {
	cases := []struct {
		me   string
		want string
	}{
		{"codex", "\n"},
		{"codex-test", "\n"},
		{"my-codex-2", "\n"},
		{"claude", ""},
		{"claude-test", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := rawSubmitPrelude(c.me); got != c.want {
			t.Fatalf("rawSubmitPrelude(%q) = %q, want %q", c.me, got, c.want)
		}
	}
}

func TestInjectNotificationRawInjectsLFPreludeForCodex(t *testing.T) {
	// codex targets get a lone LF between the drained text and the settle: it
	// routes through codex-tui's Ctrl-J binding, which flushes and clears
	// paste-burst state before the \r submit. In the reproduced Ghostty +
	// kitty-enhanced path a bare \r did not submit without it.
	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		injected = append(injected, text)
		return nil
	})
	stubRawInputDrained(t, func(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
		return time.Millisecond, true, nil
	})
	slept := stubRawInjectSleep(t)

	cfg := &wakeConfig{injectMode: "raw", me: "codex"}
	if err := injectNotification(cfg, "AMQ wake", true); err != nil {
		t.Fatalf("injectNotification: %v", err)
	}

	if got := strings.Join(injected, "|"); got != "AMQ wake|\n|\r|\r" {
		t.Fatalf("raw injection sequence = %q, want text, LF prelude, CR, rescue CR", got)
	}
	if len(*slept) != 2 {
		t.Fatalf("settle sleeps = %v, want two settle delays", *slept)
	}
}

func TestInjectNotificationRawNeverInjectsEscapeBytes(t *testing.T) {
	// TIOCSTI delivers one byte per ioctl, so a multi-byte escape sequence can
	// be split by reader scheduling: a reader that sees a lone ESC parses the
	// Escape key, which cancels an active codex turn. Raw-mode injection must
	// therefore never contain ESC for any target.
	for _, me := range []string{"", "claude", "codex", "codex-test"} {
		var injected []string
		stubTIOCSTIInject(t, func(text string) error {
			injected = append(injected, text)
			return nil
		})
		stubRawInputDrained(t, func(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
			return time.Millisecond, true, nil
		})
		stubRawInjectSleep(t)

		cfg := &wakeConfig{injectMode: "raw", me: me}
		if err := injectNotification(cfg, "AMQ wake", true); err != nil {
			t.Fatalf("injectNotification(me=%q): %v", me, err)
		}
		for _, chunk := range injected {
			if strings.Contains(chunk, "\x1b") {
				t.Fatalf("me=%q injected chunk %q contains ESC", me, chunk)
			}
		}
	}
}

func TestInjectNotificationRawDrainsSettlesThenInjectsCRWithRescue(t *testing.T) {
	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		injected = append(injected, text)
		return nil
	})

	var drainCalls [][2]time.Duration
	stubRawInputDrained(t, func(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
		drainCalls = append(drainCalls, [2]time.Duration{timeout, pollInterval})
		return 30 * time.Millisecond, true, nil
	})
	slept := stubRawInjectSleep(t)

	cfg := &wakeConfig{injectMode: "raw"}
	if err := injectNotification(cfg, "AMQ wake", true); err != nil {
		t.Fatalf("injectNotification: %v", err)
	}

	if got := strings.Join(injected, "|"); got != "AMQ wake|\r|\r" {
		t.Fatalf("raw injection sequence = %q, want text then CR then rescue CR", got)
	}
	wantDrains := [][2]time.Duration{
		{rawInjectDrainTimeout, rawInjectDrainPollInterval},
		{rawInjectCRDrainTimeout, rawInjectDrainPollInterval},
	}
	if len(drainCalls) != len(wantDrains) || drainCalls[0] != wantDrains[0] || drainCalls[1] != wantDrains[1] {
		t.Fatalf("drain calls = %v, want %v", drainCalls, wantDrains)
	}
	if len(*slept) != 2 || (*slept)[0] != rawInjectSettleDelay || (*slept)[1] != rawInjectSettleDelay {
		t.Fatalf("settle sleeps = %v, want two of %s", *slept, rawInjectSettleDelay)
	}
}

func TestInjectNotificationRawSkipsRescueCRWhenFirstCRStillQueued(t *testing.T) {
	// Models the stated skip branch precisely: the notification text drains
	// normally, then the first CR is still queued at the CR-drain deadline.
	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		injected = append(injected, text)
		return nil
	})
	var drainCalls [][2]time.Duration
	stubRawInputDrained(t, func(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
		drainCalls = append(drainCalls, [2]time.Duration{timeout, pollInterval})
		if len(drainCalls) == 1 {
			return 5 * time.Millisecond, true, nil // text consumed by the TUI
		}
		return timeout, false, nil // first CR still in the kernel queue
	})
	slept := stubRawInjectSleep(t)

	cfg := &wakeConfig{injectMode: "raw", debug: true}
	stderr := captureWakeStderr(t, func() {
		if err := injectNotification(cfg, "AMQ wake", true); err != nil {
			t.Fatalf("injectNotification: %v", err)
		}
	})

	if got := strings.Join(injected, "|"); got != "AMQ wake|\r" {
		t.Fatalf("raw injection sequence = %q, want text then one CR", got)
	}
	wantDrains := [][2]time.Duration{
		{rawInjectDrainTimeout, rawInjectDrainPollInterval},
		{rawInjectCRDrainTimeout, rawInjectDrainPollInterval},
	}
	if len(drainCalls) != len(wantDrains) || drainCalls[0] != wantDrains[0] || drainCalls[1] != wantDrains[1] {
		t.Fatalf("drain calls = %v, want %v", drainCalls, wantDrains)
	}
	if !strings.Contains(stderr, "input queue drained") {
		t.Fatalf("expected text drain debug log, got %q", stderr)
	}
	if !strings.Contains(stderr, "skipping rescue submit") {
		t.Fatalf("expected rescue skip debug log, got %q", stderr)
	}
	if len(*slept) != 1 || (*slept)[0] != rawInjectSettleDelay {
		t.Fatalf("settle sleeps = %v, want one of %s", *slept, rawInjectSettleDelay)
	}
}

func TestInjectNotificationRawSkipsRescueCROnTotalReaderStall(t *testing.T) {
	// Degraded branch: the reader is fully stalled — the text drain times out,
	// the CR is injected anyway, and the rescue is skipped because the CR is
	// provably still queued behind the unread text.
	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		injected = append(injected, text)
		return nil
	})
	stubRawInputDrained(t, func(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
		return timeout, false, nil
	})
	slept := stubRawInjectSleep(t)

	cfg := &wakeConfig{injectMode: "raw", debug: true}
	stderr := captureWakeStderr(t, func() {
		if err := injectNotification(cfg, "AMQ wake", true); err != nil {
			t.Fatalf("injectNotification: %v", err)
		}
	})

	if got := strings.Join(injected, "|"); got != "AMQ wake|\r" {
		t.Fatalf("raw injection sequence = %q, want text then one CR", got)
	}
	if !strings.Contains(stderr, "input drain timeout") {
		t.Fatalf("expected drain timeout debug log, got %q", stderr)
	}
	if !strings.Contains(stderr, "skipping rescue submit") {
		t.Fatalf("expected rescue skip debug log, got %q", stderr)
	}
	if len(*slept) != 1 || (*slept)[0] != rawInjectSettleDelay {
		t.Fatalf("settle sleeps = %v, want one of %s", *slept, rawInjectSettleDelay)
	}
}

func TestInjectNotificationRawInjectsBothCRsOnDrainError(t *testing.T) {
	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		injected = append(injected, text)
		return nil
	})
	stubRawInputDrained(t, func(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
		return 15 * time.Millisecond, false, errors.New("open /dev/tty: permission denied")
	})
	slept := stubRawInjectSleep(t)

	cfg := &wakeConfig{injectMode: "raw", debug: true}
	stderr := captureWakeStderr(t, func() {
		if err := injectNotification(cfg, "AMQ wake", true); err != nil {
			t.Fatalf("injectNotification: %v", err)
		}
	})

	// With the queue unobservable, fall back to timing alone: both CRs are sent
	// on the settle cadence, mirroring the pre-drain-wait behavior.
	if got := strings.Join(injected, "|"); got != "AMQ wake|\r|\r" {
		t.Fatalf("raw injection sequence = %q, want text then CR then rescue CR", got)
	}
	if !strings.Contains(stderr, "input drain wait unavailable") {
		t.Fatalf("expected drain unavailable debug log, got %q", stderr)
	}
	if len(*slept) != 2 {
		t.Fatalf("settle sleeps = %v, want two settle delays", *slept)
	}
}

func TestNotifyNewMessages_InjectViaInterruptInjectsKeyAndHonorsCooldown(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	logPath := filepath.Join(root, "inject.log")
	scriptPath := filepath.Join(root, "inject.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+logPath+"\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:   1,
			ID:       "msg-urgent",
			From:     "codex",
			To:       []string{"alice"},
			Thread:   "p2p/alice__codex",
			Subject:  "help needed",
			Created:  "2026-04-25T10:00:00Z",
			Priority: "urgent",
			Labels:   []string{"interrupt"},
		},
		Body: "urgent body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, root, "alice", "msg-urgent.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	cfg := &wakeConfig{
		me:                "alice",
		root:              root,
		session:           "collab",
		injectVia:         scriptPath,
		previewLen:        48,
		interrupt:         true,
		interruptKey:      "\x03",
		interruptLabel:    "interrupt",
		interruptPriority: "urgent",
		interruptCooldown: 7 * time.Second,
	}

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("first notifyNewMessages: %v", err)
	}
	if cfg.lastInterrupt.IsZero() {
		t.Fatal("expected interrupt cooldown timestamp to be updated")
	}

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("second notifyNewMessages: %v", err)
	}

	expectedText := buildInterruptText(
		"collab",
		[]wakeMsgInfo{{from: "codex", subject: "help needed", priority: "urgent", labels: []string{"interrupt"}}},
		map[string]int{"codex": 1},
		48,
		"",
	)
	expected := "\x03\n" + expectedText + "\n"

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(got) != expected {
		t.Fatalf("expected inject-via log %q, got %q", expected, string(got))
	}
}

func TestNotifyNewMessagesNoneUrgentUsesOutputBellWithoutInput(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:   1,
			ID:       "msg-urgent-none",
			From:     "codex",
			To:       []string{"alice"},
			Thread:   "p2p/alice__codex",
			Subject:  "help needed",
			Created:  "2026-07-11T07:00:00Z",
			Priority: "urgent",
			Labels:   []string{"interrupt"},
		},
		Body: "urgent body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, root, "alice", "msg-urgent-none.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		injected = append(injected, text)
		return errors.New("none mode must not inject")
	})

	cfg := &wakeConfig{
		me:                "alice",
		root:              root,
		session:           "collab",
		injectMode:        wakeInjectModeNone,
		previewLen:        48,
		interrupt:         true,
		interruptKey:      "\x03",
		interruptLabel:    "interrupt",
		interruptPriority: "urgent",
		interruptCooldown: 7 * time.Second,
	}
	stderr := captureWakeStderr(t, func() {
		if err := notifyNewMessages(cfg); err != nil {
			t.Fatalf("notifyNewMessages: %v", err)
		}
	})

	if len(injected) != 0 {
		t.Fatalf("none mode injected urgent terminal input: %q", injected)
	}
	if !cfg.lastInterrupt.IsZero() {
		t.Fatalf("none mode recorded a synthetic interrupt: %s", cfg.lastInterrupt)
	}
	expectedText := buildInterruptText(
		"collab",
		[]wakeMsgInfo{{from: "codex", subject: "help needed", priority: "urgent", labels: []string{"interrupt"}}},
		map[string]int{"codex": 1},
		48,
		"",
	)
	if stderr != "\a"+expectedText+"\n" {
		t.Fatalf("stderr = %q, want one bell + urgent notice %q", stderr, expectedText)
	}

	externalCfg, outputPath := injectViaCaptureConfig(t)
	externalCfg.me = "alice"
	externalCfg.root = root
	externalCfg.session = "collab"
	externalCfg.injectMode = wakeInjectModeNone
	externalCfg.previewLen = 48
	externalCfg.interrupt = true
	externalCfg.interruptKey = "\x03"
	externalCfg.interruptLabel = "interrupt"
	externalCfg.interruptPriority = "urgent"
	externalStderr := captureWakeStderr(t, func() {
		if err := notifyNewMessages(externalCfg); err != nil {
			t.Fatalf("notifyNewMessages with inject-via config: %v", err)
		}
	})
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("none mode invoked inject-via for urgent notice; output stat error = %v", err)
	}
	if externalStderr != "\a"+expectedText+"\n" {
		t.Fatalf("external stderr = %q, want one bell + urgent notice %q", externalStderr, expectedText)
	}
}

func TestNotifyNewMessages_InjectViaInjectCmdPayload(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	logPath := filepath.Join(root, "inject.log")
	scriptPath := filepath.Join(root, "inject.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf '%s' \"$1\" > "+logPath+"\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-normal",
			From:    "codex",
			To:      []string{"alice"},
			Thread:  "p2p/alice__codex",
			Subject: "normal",
			Created: "2026-04-25T10:00:00Z",
		},
		Body: "normal body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, root, "alice", "msg-normal.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	cfg := &wakeConfig{
		me:         "alice",
		root:       root,
		injectVia:  scriptPath,
		injectCmd:  "amq drain --include-body",
		previewLen: 48,
	}

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notifyNewMessages: %v", err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	expected := "\namq drain --include-body\n"
	if string(got) != expected {
		t.Fatalf("expected inject-cmd payload %q, got %q", expected, string(got))
	}
}

func TestNotifyNewMessagesSkipsBaselineWithoutDraining(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	writeMessage := func(filename, id, subject string) {
		t.Helper()
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      id,
				From:    "codex",
				To:      []string{"alice"},
				Thread:  "p2p/alice__codex",
				Subject: subject,
				Created: "2026-07-22T00:00:00Z",
			},
			Body: "body",
		}
		data, err := msg.Marshal()
		if err != nil {
			t.Fatalf("marshal %s: %v", id, err)
		}
		if _, err := deliverToInboxForTest(t, root, "alice", filename, data); err != nil {
			t.Fatalf("deliver %s: %v", id, err)
		}
	}

	writeMessage("stale.md", "stale", "stale subject")
	cfg, outputPath := injectViaCaptureConfig(t)
	cfg.me = "alice"
	cfg.root = root
	cfg.previewLen = 48
	staleInfo, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "stale.md"))
	if err != nil {
		t.Fatalf("stat stale baseline message: %v", err)
	}
	staleIdentity, ok := captureWakeFileIdentity(staleInfo)
	if !ok {
		t.Fatal("capture stale baseline identity")
	}
	cfg.baselineExisting = map[string]wakeFileIdentity{"stale.md": staleIdentity}

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify stale baseline: %v", err)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("baseline message triggered injection; output stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "stale.md")); err != nil {
		t.Fatalf("baseline message was moved or removed: %v", err)
	}
	receipts, err := os.ReadDir(fsq.AgentReceipts(root, "alice"))
	if err != nil {
		t.Fatalf("read receipts: %v", err)
	}
	if len(receipts) != 0 {
		t.Fatalf("wake created receipts for an unread baseline: %v", receipts)
	}

	writeMessage("fresh.md", "fresh", "fresh subject")
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify fresh message: %v", err)
	}
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read injection output: %v", err)
	}
	if !strings.Contains(string(got), "fresh subject") || strings.Contains(string(got), "stale subject") {
		t.Fatalf("injected payload = %q, want fresh message only", string(got))
	}
}

func TestNotifyNewMessages_InjectViaInterruptFailureDoesNotUpdateCooldown(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:   1,
			ID:       "msg-urgent",
			From:     "codex",
			To:       []string{"alice"},
			Thread:   "p2p/alice__codex",
			Subject:  "help needed",
			Created:  "2026-04-25T10:00:00Z",
			Priority: "urgent",
			Labels:   []string{"interrupt"},
		},
		Body: "urgent body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, root, "alice", "msg-urgent.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	cfg := &wakeConfig{
		me:                "alice",
		root:              root,
		injectVia:         filepath.Join(root, "missing-injector"),
		previewLen:        48,
		interrupt:         true,
		interruptKey:      "\x03",
		interruptLabel:    "interrupt",
		interruptPriority: "urgent",
		interruptCooldown: 7 * time.Second,
	}

	if err := notifyNewMessages(cfg); err == nil {
		t.Fatal("notifyNewMessages unexpectedly acknowledged failed external injection")
	}
	if !cfg.lastInterrupt.IsZero() {
		t.Fatalf("expected failed interrupt transport to leave cooldown unchanged, got %s", cfg.lastInterrupt)
	}
}

func TestWaitForInputQueueDrainReturnsWhenQueueClears(t *testing.T) {
	samples := []int{3, 1, 0}
	sampleIndex := 0
	now := time.Unix(0, 0)
	var sleeps []time.Duration

	waited, drained, err := waitForInputQueueDrain(
		func() (int, error) {
			if sampleIndex >= len(samples) {
				t.Fatalf("sample called too many times")
				return 0, nil
			}
			pending := samples[sampleIndex]
			sampleIndex++
			return pending, nil
		},
		func() time.Time {
			return now
		},
		func(delay time.Duration) {
			sleeps = append(sleeps, delay)
			now = now.Add(delay)
		},
		100*time.Millisecond,
		10*time.Millisecond,
	)

	if err != nil {
		t.Fatalf("waitForInputQueueDrain: %v", err)
	}
	if !drained {
		t.Fatal("expected queue to drain")
	}
	if waited != 20*time.Millisecond {
		t.Fatalf("waited = %s, want 20ms", waited)
	}
	if sampleIndex != 3 {
		t.Fatalf("sample calls = %d, want 3", sampleIndex)
	}
	if len(sleeps) != 2 || sleeps[0] != 10*time.Millisecond || sleeps[1] != 10*time.Millisecond {
		t.Fatalf("sleeps = %v, want [10ms 10ms]", sleeps)
	}
}

func TestWaitForInputQueueDrainBoundsSleepByTimeout(t *testing.T) {
	now := time.Unix(0, 0)
	sampleCalls := 0
	var sleeps []time.Duration

	waited, drained, err := waitForInputQueueDrain(
		func() (int, error) {
			sampleCalls++
			return 1, nil
		},
		func() time.Time {
			return now
		},
		func(delay time.Duration) {
			sleeps = append(sleeps, delay)
			now = now.Add(delay)
		},
		25*time.Millisecond,
		10*time.Millisecond,
	)

	if err != nil {
		t.Fatalf("waitForInputQueueDrain: %v", err)
	}
	if drained {
		t.Fatal("expected drain timeout")
	}
	if waited != 25*time.Millisecond {
		t.Fatalf("waited = %s, want 25ms", waited)
	}
	if sampleCalls != 4 {
		t.Fatalf("sample calls = %d, want 4", sampleCalls)
	}
	if len(sleeps) != 3 || sleeps[0] != 10*time.Millisecond || sleeps[1] != 10*time.Millisecond || sleeps[2] != 5*time.Millisecond {
		t.Fatalf("sleeps = %v, want [10ms 10ms 5ms]", sleeps)
	}
}

func TestTTYInputStateActive(t *testing.T) {
	now := time.Date(2026, 4, 24, 7, 0, 0, 0, time.UTC)
	quietFor := 1200 * time.Millisecond

	if active, reason := (ttyInputState{pendingBytes: 1}).active(now, quietFor); !active || reason != "pending terminal input" {
		t.Fatalf("expected pending bytes to defer, active=%v reason=%q", active, reason)
	}
	if active, reason := (ttyInputState{
		lastRead:    now.Add(-500 * time.Millisecond),
		hasLastRead: true,
	}).active(now, quietFor); !active || reason != "recent terminal input" {
		t.Fatalf("expected recent read to defer, active=%v reason=%q", active, reason)
	}
	if active, reason := (ttyInputState{
		lastRead:    now.Add(-2 * time.Second),
		hasLastRead: true,
	}).active(now, quietFor); active || reason != "" {
		t.Fatalf("expected stale read to be inactive, active=%v reason=%q", active, reason)
	}
	if active, reason := (ttyInputState{}).active(now, quietFor); active || reason != "" {
		t.Fatalf("expected missing read time to be inactive, active=%v reason=%q", active, reason)
	}
}

func TestTTYInputStateActiveTreatsFutureReadAsActive(t *testing.T) {
	now := time.Date(2026, 4, 24, 7, 0, 0, 0, time.UTC)
	active, reason := (ttyInputState{
		lastRead:    now.Add(100 * time.Millisecond),
		hasLastRead: true,
	}).active(now, time.Second)
	if !active || reason != "recent terminal input" {
		t.Fatalf("expected future read time to defer, active=%v reason=%q", active, reason)
	}
}

func TestInputDeferralDelay(t *testing.T) {
	now := time.Date(2026, 4, 24, 7, 0, 0, 0, time.UTC)
	deadline := now.Add(30 * time.Second)
	state := ttyInputState{
		lastRead:    now.Add(-1100 * time.Millisecond),
		hasLastRead: true,
	}

	got := inputDeferralDelay(state, now, deadline, 1200*time.Millisecond, 200*time.Millisecond)
	if got != 100*time.Millisecond {
		t.Fatalf("expected delay to stop at quiet boundary, got %s", got)
	}
}

func TestInputDeferralDelayBoundsByDeadline(t *testing.T) {
	now := time.Date(2026, 4, 24, 7, 0, 0, 0, time.UTC)
	deadline := now.Add(50 * time.Millisecond)
	got := inputDeferralDelay(ttyInputState{pendingBytes: 1}, now, deadline, time.Second, 200*time.Millisecond)
	if got != 50*time.Millisecond {
		t.Fatalf("expected delay bounded by deadline, got %s", got)
	}
}

func TestShouldDeferBeforeInject(t *testing.T) {
	cfg := &wakeConfig{deferWhileInput: true}
	if !shouldDeferBeforeInject(cfg, true) {
		t.Fatalf("expected normal wake injection to use input deferral")
	}
	if shouldDeferBeforeInject(cfg, false) {
		t.Fatalf("expected interrupt wake injection to bypass input deferral")
	}

	cfg.deferWhileInput = false
	if shouldDeferBeforeInject(cfg, true) {
		t.Fatalf("expected disabled input deferral to inject immediately")
	}

	cfg.deferWhileInput = true
	cfg.injectVia = "external-injector"
	if shouldDeferBeforeInject(cfg, true) {
		t.Fatalf("expected external injection to bypass local TTY input deferral")
	}
}
