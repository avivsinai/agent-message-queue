package cli

import (
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

func TestInjectNotification_InjectVia(t *testing.T) {
	// Use a shell script that writes the injected text to a temp file
	tmp, err := os.CreateTemp("", "wake-inject-via-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	script, err := os.CreateTemp("", "wake-inject-via-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	scriptPath := script.Name()
	script.WriteString("#!/bin/sh\nprintf '%s' \"$1\" > " + tmpPath + "\n")
	script.Close()
	os.Chmod(scriptPath, 0o755)
	defer os.Remove(scriptPath)

	cfg := &wakeConfig{
		injectVia: scriptPath,
		debug:     false,
	}

	text := "AMQ [collab]: message from codex - hello"
	if err := injectNotification(cfg, text, true); err != nil {
		t.Fatalf("injectNotification failed: %v", err)
	}

	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("failed to read output: %v", err)
	}
	if string(got) != text {
		t.Fatalf("expected %q, got %q", text, string(got))
	}
}

func TestInjectNotification_InjectVia_MultiWordCommand(t *testing.T) {
	tmp, err := os.CreateTemp("", "wake-inject-via-multi-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	scriptDir := filepath.Join(t.TempDir(), "inject dir")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}
	scriptPath := filepath.Join(scriptDir, "inject script.sh")
	script, err := os.OpenFile(scriptPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create script: %v", err)
	}
	// $1 = "exec", $2 = "TERMID", $3 = the notification text
	script.WriteString("#!/bin/sh\nprintf '%s\\n%s\\n%s' \"$1\" \"$2\" \"$3\" > " + tmpPath + "\n")
	if err := script.Close(); err != nil {
		t.Fatalf("close script: %v", err)
	}

	// Simulates: ghostty-bridge exec "Team Alpha" <text>.
	cfg := &wakeConfig{
		injectVia:  scriptPath,
		injectArgs: []string{"exec", "Team Alpha"},
		debug:      false,
	}

	text := "AMQ [collab]: test message"
	if err := injectNotification(cfg, text, true); err != nil {
		t.Fatalf("injectNotification failed: %v", err)
	}

	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("failed to read output: %v", err)
	}
	expected := "exec\nTeam Alpha\n" + text
	if string(got) != expected {
		t.Fatalf("expected %q, got %q", expected, string(got))
	}
}

func TestInjectViaTimeout(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "sleep.sh")
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

	// Should not return error — falls back to stderr
	if err := injectNotification(cfg, "test", true); err != nil {
		t.Fatalf("expected graceful fallback, got error: %v", err)
	}
}

func TestNotifyNewMessages_InjectViaInterruptInjectsKeyAndHonorsCooldown(t *testing.T) {
	root := t.TempDir()
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
	if _, err := fsq.DeliverToInbox(root, "alice", "msg-urgent.md", data); err != nil {
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
	expected := "\x03\n" + expectedText + "\n" + expectedText + "\n"

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(got) != expected {
		t.Fatalf("expected inject-via log %q, got %q", expected, string(got))
	}
}

func TestNotifyNewMessages_InjectViaInjectCmdPayload(t *testing.T) {
	root := t.TempDir()
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
	if _, err := fsq.DeliverToInbox(root, "alice", "msg-normal.md", data); err != nil {
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

func TestNotifyNewMessages_InjectViaInterruptFailureDoesNotUpdateCooldown(t *testing.T) {
	root := t.TempDir()
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
	if _, err := fsq.DeliverToInbox(root, "alice", "msg-urgent.md", data); err != nil {
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

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notifyNewMessages: %v", err)
	}
	if !cfg.lastInterrupt.IsZero() {
		t.Fatalf("expected failed interrupt transport to leave cooldown unchanged, got %s", cfg.lastInterrupt)
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
