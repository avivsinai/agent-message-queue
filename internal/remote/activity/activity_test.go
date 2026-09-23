package activity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"

	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
)

func TestCodexNotificationsPublishDecryptableFrames(t *testing.T) {
	body := nostr.Generate()
	owner := nostr.Generate()
	var got []nostr.Event
	sink := testSink(body, owner, func(_ context.Context, evt nostr.Event) error {
		got = append(got, evt)
		return nil
	})
	notes := []codex.Notification{
		{Method: "turn/started", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1"}}`)},
		{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","id":"item-1","text":"hello"}}`)},
		{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}`)},
	}
	for _, n := range notes {
		if err := sink.Accept(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 4 {
		t.Fatalf("frames = %d, want 4", len(got))
	}
	key, err := nip44.GenerateConversationKey(body.Public(), owner)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i, evt := range got {
		if evt.Kind != KindTelemetry {
			t.Fatalf("kind = %d", evt.Kind)
		}
		if evt.PubKey != body.Public() || !evt.VerifySignature() {
			t.Fatalf("event %d is not signed by the body", i)
		}
		if !hasTag(evt, "p", owner.Public().Hex()) || !hasTag(evt, "agent", body.Public().Hex()) || !hasTag(evt, "frame", "telemetry") {
			t.Fatalf("tags = %#v", evt.Tags)
		}
		plain, err := nip44.Decrypt(evt.Content, key)
		if err != nil {
			t.Fatal(err)
		}
		var obs observerJSON
		if err := json.Unmarshal([]byte(plain), &obs); err != nil {
			t.Fatal(err)
		}
		if obs.Seq != uint64(i+1) || obs.SessionID != "thread-1" || !strings.Contains(plain, `"channelId":null`) {
			t.Fatalf("observation = %#v", obs)
		}
		kinds = append(kinds, obs.Kind)
		switch obs.Kind {
		case "acp_read":
			if obs.TurnID != "turn-1" || !strings.Contains(string(obs.Payload), "hello") || !strings.Contains(string(obs.Payload), `"provenance":"native_projection"`) {
				t.Fatalf("payload = %s", obs.Payload)
			}
		case "session_resolved":
			if !strings.Contains(string(obs.Payload), `"isNewSession":false`) || !strings.Contains(string(obs.Payload), `"sessionId":"thread-1"`) {
				t.Fatalf("payload = %s", obs.Payload)
			}
		default:
			if obs.TurnID != "turn-1" {
				t.Fatalf("observation = %#v", obs)
			}
		}
	}
	if strings.Join(kinds, ",") != "session_resolved,turn_started,acp_read,turn_completed" {
		t.Fatalf("kinds = %s", kinds)
	}
}

func TestLimiterCapsOneHundredFramesPerSecond(t *testing.T) {
	body := nostr.Generate()
	owner := nostr.Generate()
	now := time.Date(2026, 9, 23, 5, 0, 0, 0, time.UTC)
	var n int
	sink := testSink(body, owner, func(context.Context, nostr.Event) error {
		n++
		return nil
	})
	sink.Now = func() time.Time { return now }
	note := codex.Notification{Method: "turn/started", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1"}}`)}
	for range 101 {
		if err := sink.Accept(context.Background(), note); err != nil {
			t.Fatal(err)
		}
	}
	if n != 100 {
		t.Fatalf("published = %d, want 100", n)
	}
}

func TestPublishErrorIsNotRetried(t *testing.T) {
	body := nostr.Generate()
	owner := nostr.Generate()
	calls := 0
	sink := testSink(body, owner, func(context.Context, nostr.Event) error {
		calls++
		return errors.New("ambiguous delivery")
	})
	err := sink.Accept(context.Background(), codex.Notification{
		Method: "turn/started", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1"}}`),
	})
	if err == nil || calls != 1 {
		t.Fatalf("err = %v, calls = %d", err, calls)
	}
}

func TestLongTextIsResplitUnder64KiB(t *testing.T) {
	body := nostr.Generate()
	owner := nostr.Generate()
	text := strings.Repeat("a", 50000)
	var got []nostr.Event
	sink := testSink(body, owner, func(_ context.Context, evt nostr.Event) error {
		got = append(got, evt)
		return nil
	})
	err := sink.Accept(context.Background(), codex.Notification{
		Method: "item/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","id":"item-1","text":"` + text + `"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 {
		t.Fatalf("frames = %d, want a split", len(got))
	}
	key, err := nip44.GenerateConversationKey(body.Public(), owner)
	if err != nil {
		t.Fatal(err)
	}
	var prev uint64
	var joined strings.Builder
	for _, evt := range got {
		n, err := frameLen(evt)
		if err != nil {
			t.Fatal(err)
		}
		if n > MaxFrameBytes {
			t.Fatalf("frame = %d bytes", n)
		}
		plain, err := nip44.Decrypt(evt.Content, key)
		if err != nil {
			t.Fatal(err)
		}
		var obs observerJSON
		if err := json.Unmarshal([]byte(plain), &obs); err != nil {
			t.Fatal(err)
		}
		if obs.Seq <= prev {
			t.Fatalf("seq = %d after %d", obs.Seq, prev)
		}
		prev = obs.Seq
		var note struct {
			Params struct {
				Update struct {
					Content struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal(obs.Payload, &note); err != nil {
			t.Fatal(err)
		}
		joined.WriteString(note.Params.Update.Content.Text)
	}
	if joined.String() != text {
		t.Fatalf("joined text length = %d, want %d", joined.Len(), len(text))
	}
}

func TestClaudeTranscriptPublishesDecryptableFrames(t *testing.T) {
	body := nostr.Generate()
	owner := nostr.Generate()
	var got []nostr.Event
	sink := testSink(body, owner, func(_ context.Context, evt nostr.Event) error {
		got = append(got, evt)
		return nil
	})
	const hiddenPath = "/Users/aviv.s/secret/transcript.go"
	lines := []string{
		`{"type":"user","uuid":"u1","sessionId":"thread-1","timestamp":"2026-09-23T06:00:00.000000000Z","message":{"role":"user","content":"where is the parser"}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"thread-1","timestamp":"2026-09-23T06:00:01.000000000Z","message":{"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"` + hiddenPath + `"}}]}}`,
		`{"type":"user","uuid":"r1","sessionId":"thread-1","timestamp":"2026-09-23T06:00:02.000000000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"package claude"}]}}`,
	}
	for _, line := range lines {
		if err := sink.AcceptClaude(context.Background(), line); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 5 {
		t.Fatalf("frames = %d, want 5", len(got))
	}
	key, err := nip44.GenerateConversationKey(body.Public(), owner)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ update, text, toolID string }{
		{"user_message_chunk", "where is the parser", ""},
		{"agent_message_chunk", "Looking.", ""},
		{"tool_call", "Read", "toolu_1"},
		{"tool_call_update", "package claude", "toolu_1"},
	}
	for i, evt := range got {
		if evt.Kind != KindTelemetry || evt.PubKey != body.Public() || !evt.VerifySignature() {
			t.Fatalf("event %d is not a signed 24200 frame", i)
		}
		if !hasTag(evt, "p", owner.Public().Hex()) || !hasTag(evt, "agent", body.Public().Hex()) || !hasTag(evt, "frame", "telemetry") {
			t.Fatalf("tags = %#v", evt.Tags)
		}
		for _, tag := range evt.Tags {
			joined := strings.Join(tag, " ")
			if strings.Contains(joined, hiddenPath) || (i > 0 && strings.Contains(joined, want[i-1].text)) {
				t.Fatalf("clear tag %q carries transcript data", joined)
			}
		}
		plain, err := nip44.Decrypt(evt.Content, key)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(plain, hiddenPath) {
			t.Fatalf("frame %d copied a local path", i)
		}
		var obs observerJSON
		if err := json.Unmarshal([]byte(plain), &obs); err != nil {
			t.Fatal(err)
		}
		if obs.Seq != uint64(i+1) || obs.SessionID != "thread-1" || !strings.Contains(plain, `"channelId":null`) {
			t.Fatalf("observation = %#v", obs)
		}
		if i == 0 {
			if obs.Kind != "session_resolved" || !strings.Contains(string(obs.Payload), `"isNewSession":false`) {
				t.Fatalf("observation = %#v", obs)
			}
			continue
		}
		if obs.Kind != "acp_read" || !strings.Contains(string(obs.Payload), `"provenance":"native_projection"`) {
			t.Fatalf("payload = %s", obs.Payload)
		}
		if obs.TurnID != "" {
			t.Fatalf("line uuid must not be the turn id: %q", obs.TurnID)
		}
		var note struct {
			Params struct {
				Meta   map[string]string `json:"_meta"`
				Update struct {
					SessionUpdate string `json:"sessionUpdate"`
					ToolCallID    string `json:"toolCallId"`
					Title         string `json:"title"`
					Status        string `json:"status"`
					Content       any    `json:"content"`
				} `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal(obs.Payload, &note); err != nil {
			t.Fatal(err)
		}
		gotUpdate := note.Params.Update
		w := want[i-1]
		if gotUpdate.SessionUpdate != w.update || gotUpdate.ToolCallID != w.toolID {
			t.Fatalf("update = %#v", gotUpdate)
		}
		if note.Params.Meta["itemId"] == "" {
			t.Fatalf("line uuid missing from _meta: %s", obs.Payload)
		}
		switch w.update {
		case "tool_call":
			if gotUpdate.Title != w.text || gotUpdate.Status != "pending" {
				t.Fatalf("tool_call = %#v", gotUpdate)
			}
			if _, ok := gotUpdate.Content.([]any); gotUpdate.Content != nil && !ok {
				t.Fatalf("tool content = %T", gotUpdate.Content)
			}
		case "tool_call_update":
			if gotUpdate.Status != "completed" {
				t.Fatalf("tool_call_update = %#v", gotUpdate)
			}
			items, ok := gotUpdate.Content.([]any)
			if !ok || len(items) != 1 {
				t.Fatalf("tool content = %#v", gotUpdate.Content)
			}
			item, _ := items[0].(map[string]any)
			inner, _ := item["content"].(map[string]any)
			if inner["text"] != w.text {
				t.Fatalf("tool content = %#v", gotUpdate.Content)
			}
		default:
			body, _ := gotUpdate.Content.(map[string]any)
			if body["text"] != w.text {
				t.Fatalf("text = %#v", gotUpdate.Content)
			}
		}
	}
}

func testSink(body, owner nostr.SecretKey, publish Publish) *Sink {
	return &Sink{
		ThreadID: "thread-1",
		Body:     body,
		Owner:    owner.Public(),
		Publish:  publish,
		Now:      func() time.Time { return time.Date(2026, 9, 23, 5, 0, 0, 0, time.UTC) },
	}
}

func hasTag(evt nostr.Event, name, value string) bool {
	for _, tag := range evt.Tags {
		if len(tag) >= 2 && tag[0] == name && tag[1] == value {
			return true
		}
	}
	return false
}
