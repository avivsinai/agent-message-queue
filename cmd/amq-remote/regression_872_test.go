package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// codex #872 r1: a Claude note was charged only its block text, so a note
// with a megabyte UUID was admitted as one byte.
func TestClaudeNoteMetadataCountsTowardBudget(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"type": "assistant", "sessionId": "thread-1", "uuid": strings.Repeat("x", activityItemBytes+1), "message": map[string]any{"content": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	line, ok := claude.ParseTranscriptLine(string(raw))
	if !ok {
		t.Fatal("real transcript parser rejected the line")
	}
	note := claude.ActivityNote{Line: line, SessionID: "thread-1", TurnID: "turn-1"}
	it := activityItem{claude: &note}
	if newActivityInbox().offer(it) {
		t.Fatalf("admitted %d-byte UUID plus content, charged only %d bytes against %d-byte cap", len(line.UUID), it.size(), activityItemBytes)
	}
}

// codex #872 r1: the worker's AcceptParsed and the tick's Drain both
// drained the sink and could publish frames out of order.
func TestClaudeDrainersPreservePublicationOrder(t *testing.T) {
	var published atomic.Int32
	conn, as := reviewActivityConn(t, func() { published.Add(1) })
	src := &observedClaude{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldTick := activityDrainTick
	activityDrainTick = time.Second
	defer func() { activityDrainTick = oldTick }()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	identity := func(string) string {
		// Worker admission is call 1. Call 2 is the first dequeued
		// event's Publish fence. The tick's independent Drain can overtake it.
		if calls.Add(1) == 2 {
			close(entered)
			<-release
		}
		return "thread-1"
	}
	done := make(chan struct{})
	go func() { defer close(done); as.export(ctx, conn, observerOf(src), identity, io.Discard) }()
	reviewWait(t, "observer", func() bool { return as.view() == "exporting" })
	src.mu.Lock()
	fn := src.fn
	src.mu.Unlock()
	fn(claude.ActivityNote{SessionID: "thread-1", TurnID: "turn-1", Line: claude.TranscriptLine{Type: "assistant", UUID: "a1", Blocks: []claude.TranscriptBlock{{Type: "text", Text: "hello"}}}})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first publish did not reach fence")
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for published.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	overtook := published.Load() > 0
	unblock()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("export did not stop")
	}
	if overtook {
		t.Fatal("tick Drain published the later Claude frame while the worker held the first dequeued frame at its publication fence")
	}
}
