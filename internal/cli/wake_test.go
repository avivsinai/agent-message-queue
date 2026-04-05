package cli

import (
	"strings"
	"testing"
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
