package activity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"

	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
)

func agentMessageNote(text string) codex.Notification {
	raw, _ := json.Marshal(map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"item":     map[string]string{"type": "agentMessage", "text": text},
	})
	return codex.Notification{Method: "item/completed", Params: raw}
}

func TestSequenceSurvivesSinkRecreation(t *testing.T) {
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
		if err := deliver(context.Background(), sink, agentMessageNote("hello")); err != nil {
			t.Fatal(err)
		}
		rounds = append(rounds, seqs)
	}
	if len(rounds[0]) == 0 || len(rounds[1]) == 0 || rounds[1][0] <= rounds[0][len(rounds[0])-1] {
		t.Fatalf("same body/native session sequence resets across sink recreation: %v", rounds)
	}
}

func TestRateLimitAppliesAtSend(t *testing.T) {
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
	if err := deliver(context.Background(), sink, agentMessageNote(strings.Repeat("a", 48000*50))); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if err := deliver(context.Background(), sink, agentMessageNote("next")); err != nil {
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

func TestStaleQueuedFramesAreDiscarded(t *testing.T) {
	body, owner := nostr.Generate(), nostr.Generate()
	now := time.Date(2026, 9, 23, 5, 0, 0, 0, time.UTC)
	sink := testSink(body, owner, func(context.Context, nostr.Event) error {
		return errors.New("disconnected")
	})
	sink.Now = func() time.Time { return now }
	if err := deliver(context.Background(), sink, agentMessageNote(strings.Repeat("a", 50000))); err == nil {
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
	if err := deliver(context.Background(), sink, agentMessageNote("new")); err != nil {
		t.Fatal(err)
	}
	if stale > 0 {
		t.Fatalf("sent %d stale frames after reconnect", stale)
	}
}

func TestEnqueueDoesNotPublish(t *testing.T) {
	body, owner := nostr.Generate(), nostr.Generate()
	called := false
	sink := testSink(body, owner, func(context.Context, nostr.Event) error {
		called = true
		return nil
	})
	if err := sink.Enqueue(agentMessageNote("hello")); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("enqueue waited on publish")
	}
}

func TestAmbiguousSendsConsumeRate(t *testing.T) {
	sent := 0
	sink := testSink(nostr.Generate(), nostr.Generate(), func(context.Context, nostr.Event) error {
		sent++
		return errors.New("written but OK lost")
	})
	for range 120 {
		_ = deliver(context.Background(), sink, agentMessageNote("hello"))
	}
	if sent > 100 {
		t.Fatalf("%d ambiguous attempts in one second, want at most 100", sent)
	}
}

func TestConcurrentSinksReserveRate(t *testing.T) {
	body, owner := nostr.Generate(), nostr.Generate()
	var sent atomic.Int64
	first := testSink(body, owner, func(context.Context, nostr.Event) error {
		sent.Add(1)
		return nil
	})
	for range 98 {
		if err := deliver(context.Background(), first, agentMessageNote("hello")); err != nil {
			t.Fatal(err)
		}
	}
	if sent.Load() != 99 {
		t.Fatalf("setup sends=%d", sent.Load())
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	pub := func(context.Context, nostr.Event) error {
		sent.Add(1)
		entered <- struct{}{}
		<-release
		return nil
	}
	returned := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		sink := testSink(body, owner, pub)
		go func() {
			defer wg.Done()
			_ = deliver(context.Background(), sink, agentMessageNote("tail"))
			returned <- struct{}{}
		}()
	}
	defer wg.Wait()
	defer close(release)
	// One sink takes the last slot and parks in Publish. The reservation
	// counts that in-flight send, so the other sink must drop its frame and
	// return, not reach Publish too.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no sink reached Publish")
	}
	select {
	case <-returned:
	case <-entered:
		t.Fatal("both sinks reached Publish: the in-flight send did not hold its rate slot")
	case <-time.After(5 * time.Second):
		t.Fatal("the second sink neither dropped its frame nor published")
	}
	if sent.Load() > 100 {
		t.Fatalf("%d sends in one second across sinks", sent.Load())
	}
}

func TestExpiryCheckedBetweenSends(t *testing.T) {
	now := time.Date(2026, 9, 23, 5, 0, 0, 0, time.UTC)
	calls, stale := 0, 0
	sink := testSink(nostr.Generate(), nostr.Generate(), func(_ context.Context, e nostr.Event) error {
		if calls > 0 && now.Unix()-int64(e.CreatedAt) > 30 {
			stale++
		}
		calls++
		if calls == 1 {
			now = now.Add(31 * time.Second)
		}
		return nil
	})
	sink.Now = func() time.Time { return now }
	if err := deliver(context.Background(), sink, agentMessageNote(strings.Repeat("a", 50000))); err != nil {
		t.Fatal(err)
	}
	if stale > 0 {
		t.Fatalf("published %d stale frames after first send stalled", stale)
	}
}

func TestNativeEnqueueAndConsumerRunConcurrently(t *testing.T) {
	sink := testSink(nostr.Generate(), nostr.Generate(), func(context.Context, nostr.Event) error {
		return nil
	})
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range 30 {
			_ = sink.Enqueue(agentMessageNote("native"))
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range 30 {
			_ = deliver(context.Background(), sink, agentMessageNote("consumer"))
		}
	}()
	close(start)
	wg.Wait()
}

func TestFinalSequenceStillFitsFrame(t *testing.T) {
	sink := testSink(nostr.Generate(), nostr.Generate(), func(context.Context, nostr.Event) error {
		return nil
	})
	at := time.Date(2026, 9, 23, 5, 0, 0, 0, time.UTC)
	obs := observation{Seq: 9, Kind: "acp_read", SessionID: sink.ThreadID, TurnID: "turn-1", At: at}
	plain, err := obs.marshal()
	if err != nil {
		t.Fatal(err)
	}
	obs.Text = strings.Repeat("x", 40960-len(plain))
	plain, err = obs.marshal()
	if err != nil || len(plain) != 40960 {
		t.Fatalf("setup plaintext size=%d err=%v", len(plain), err)
	}
	for range 8 {
		if _, err := sink.commitSeq(); err != nil {
			t.Fatal(err)
		}
	}
	advanced := false
	sink.Now = func() time.Time {
		if !advanced {
			advanced = true
			_, _ = processSeq.commit(sink.seqKey(), sink.StateDir)
		}
		return at
	}
	frames, err := sink.fit(obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, evt := range frames {
		n, err := frameLen(evt)
		if err != nil {
			t.Fatal(err)
		}
		if n > MaxFrameBytes {
			t.Fatalf("actual final frame is %d bytes after sequence changed; cap=%d", n, MaxFrameBytes)
		}
	}
}

func TestStateDirUpgradeKeepsSequenceDurable(t *testing.T) {
	dir := t.TempDir()
	var before seqAllocator
	first, err := before.commit("body/native", "")
	if err != nil {
		t.Fatal(err)
	}
	durable, err := before.commit("body/native", dir)
	if err != nil {
		t.Fatal(err)
	}
	var restarted seqAllocator
	after, err := restarted.commit("body/native", dir)
	if err != nil {
		t.Fatal(err)
	}
	if after <= durable {
		t.Fatalf("sequence reused after StateDir upgrade: process-only=%d durable=%d restarted=%d", first, durable, after)
	}
}

// Regression: #865, equal block ceilings must not reuse the persisted
// reservation.
func TestUpgradeReadsEqualDurableHighWater(t *testing.T) {
	dir := t.TempDir()
	var old seqAllocator
	_, err := old.commit("body/native", dir)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := old.commit("body/native", dir)
	if err != nil {
		t.Fatal(err)
	}
	var current seqAllocator
	_, err = current.commit("body/native", "")
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := current.commit("body/native", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("old process issued=%d, durable reservation=1024, upgraded issued=%d", issued, upgraded)
	if upgraded <= 1024 {
		t.Fatalf("upgrade reused prior durable reservation: %d", upgraded)
	}
}

// Regression: #865, Close must not leave ciphertext or the sink pointer in the global body map.
func TestCloseReleasesRetainedFrames(t *testing.T) {
	now := time.Now()
	sink := &Sink{Now: func() time.Time { return now }}
	var q liveQueue
	q.push(sink, "body", nostr.Event{CreatedAt: nostr.Timestamp(now.Unix()), Content: strings.Repeat("x", 64000)}, 64000)
	q.release(sink, "body")
	retained := q.body["body"]
	if len(retained) != 0 || q.processBytes != 0 {
		t.Fatal("close did not clear accounting")
	}
	if cap(retained) > 0 && retained[:cap(retained)][0].evt.Content != "" {
		t.Fatalf("close reports zero queued bytes but global map retains %d bytes and sink pointer", len(retained[:cap(retained)][0].evt.Content))
	}
}
