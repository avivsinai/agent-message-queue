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

// Regressions for codex #865 r1, spec/relay-stack
// 2026-09-23T06-33-19.663Z_pid4792_721e2f4a.

func reviewNote(text string) codex.Notification {
	raw, _ := json.Marshal(map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"item":     map[string]string{"type": "agentMessage", "text": text},
	})
	return codex.Notification{Method: "item/completed", Params: raw}
}

func TestReviewSequenceSurvivesSinkRecreation(t *testing.T) {
	body, owner := nostr.Generate(), nostr.Generate()
	key, err := nip44.GenerateConversationKey(body.Public(), owner)
	if err != nil {
		t.Fatal(err)
	}
	var rounds [][]uint64
	for range 2 {
		var seqs []uint64
		sink := testSink(body, owner, func(_ context.Context, e nostr.Event) error {
			plain, err := nip44.Decrypt(e.Content, key)
			if err != nil {
				return err
			}
			var o observerJSON
			if err := json.Unmarshal([]byte(plain), &o); err != nil {
				return err
			}
			seqs = append(seqs, o.Seq)
			return nil
		})
		if err := sink.Accept(context.Background(), reviewNote("hello")); err != nil {
			t.Fatal(err)
		}
		rounds = append(rounds, seqs)
	}
	if len(rounds[0]) == 0 || len(rounds[1]) == 0 || rounds[1][0] <= rounds[0][len(rounds[0])-1] {
		t.Fatalf("same body/native session sequence resets across sink recreation: %v", rounds)
	}
}

func TestReviewDesktopAssistantKind(t *testing.T) {
	o, ok := projectCodex("thread-1", reviewNote("hello"))
	if !ok {
		t.Fatal("not projected")
	}
	if o.Kind != "acp_read" {
		t.Fatalf("assistant frame kind %q is not consumed by Desktop ACP branch", o.Kind)
	}
}

func TestReviewLimitAppliesAtSend(t *testing.T) {
	body, owner := nostr.Generate(), nostr.Generate()
	now := time.Date(2026, 9, 23, 5, 0, 0, 0, time.UTC)
	var sent []time.Time
	sink := testSink(body, owner, func(_ context.Context, _ nostr.Event) error {
		if len(sent) == 0 {
			now = now.Add(2 * time.Second)
		}
		sent = append(sent, now)
		return nil
	})
	sink.Now = func() time.Time { return now }
	if err := sink.Accept(context.Background(), reviewNote(strings.Repeat("a", 48000*50))); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if err := sink.Accept(context.Background(), reviewNote("next")); err != nil {
			t.Fatal(err)
		}
	}
	sameWindow := 0
	for _, at := range sent {
		if at.After(now.Add(-time.Second)) {
			sameWindow++
		}
	}
	if sameWindow > 100 {
		t.Fatalf("published %d frames in one sliding second after send stalled", sameWindow)
	}
}

func TestReviewStaleQueuedFramesAreDiscarded(t *testing.T) {
	body, owner := nostr.Generate(), nostr.Generate()
	now := time.Date(2026, 9, 23, 5, 0, 0, 0, time.UTC)
	sink := testSink(body, owner, func(context.Context, nostr.Event) error {
		return errors.New("disconnected")
	})
	sink.Now = func() time.Time { return now }
	if err := sink.Accept(context.Background(), reviewNote(strings.Repeat("a", 50000))); err == nil {
		t.Fatal("expected send error")
	}
	now = now.Add(31 * time.Second)
	var stale int
	sink.Publish = func(_ context.Context, e nostr.Event) error {
		if now.Unix()-int64(e.CreatedAt) > 30 {
			stale++
		}
		return nil
	}
	if err := sink.Accept(context.Background(), reviewNote("new")); err != nil {
		t.Fatal(err)
	}
	if stale > 0 {
		t.Fatalf("sent %d stale frames after reconnect", stale)
	}
}

func TestReviewEnqueueDoesNotPublish(t *testing.T) {
	body, owner := nostr.Generate(), nostr.Generate()
	called := false
	sink := testSink(body, owner, func(context.Context, nostr.Event) error {
		called = true
		return nil
	})
	if err := sink.Enqueue(reviewNote("hello")); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("enqueue waited on publish")
	}
}
