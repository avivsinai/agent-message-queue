package activity

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
)

// Regressions for codex #865 r2, spec/relay-stack
// 2026-09-23T06-59-35.329Z_pid1862_8c18f22d.

func TestR2AmbiguousSendsConsumeRate(t *testing.T) {
	sent := 0
	sink := testSink(nostr.Generate(), nostr.Generate(), func(context.Context, nostr.Event) error {
		sent++
		return errors.New("written but OK lost")
	})
	for range 120 {
		_ = sink.Accept(context.Background(), reviewNote("hello"))
	}
	if sent > 100 {
		t.Fatalf("%d ambiguous attempts in one second, want at most 100", sent)
	}
}

func TestR2ConcurrentSinksReserveRate(t *testing.T) {
	body, owner := nostr.Generate(), nostr.Generate()
	var sent atomic.Int64
	first := testSink(body, owner, func(context.Context, nostr.Event) error {
		sent.Add(1)
		return nil
	})
	for range 98 {
		if err := first.Accept(context.Background(), reviewNote("hello")); err != nil {
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
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		sink := testSink(body, owner, pub)
		go func() {
			defer wg.Done()
			_ = sink.Accept(context.Background(), reviewNote("tail"))
		}()
	}
	// The review barrier waits for both Publish calls before releasing the
	// first. Holding the reservation across Publish stops the second call,
	// so that wait cannot complete. The cap is the assertion.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(release)
		<-done
	}
	if sent.Load() > 100 {
		t.Fatalf("%d sends in one second across sinks", sent.Load())
	}
}

func TestR2ExpiryCheckedBetweenSends(t *testing.T) {
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
	if err := sink.Accept(context.Background(), reviewNote(strings.Repeat("a", 50000))); err != nil {
		t.Fatal(err)
	}
	if stale > 0 {
		t.Fatalf("published %d stale frames after first send stalled", stale)
	}
}

func TestR2NativeEnqueueAndConsumer(t *testing.T) {
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
			_ = sink.Enqueue(reviewNote("native"))
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range 30 {
			_ = sink.Accept(context.Background(), reviewNote("consumer"))
		}
	}()
	close(start)
	wg.Wait()
}

func TestR2FinalSequenceStillFitsFrame(t *testing.T) {
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

func TestReviewStateDirUpgradeDurability(t *testing.T) {
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
