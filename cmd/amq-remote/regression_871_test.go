package main

import (
	"context"
	"encoding/json"
	"fiatjaf.com/nostr"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/coder/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func reviewActivityConn(t *testing.T, event func()) (*relay.Conn, *activityShare) {
	t.Helper()
	root := t.TempDir()
	owner := nostr.Generate()
	body, tag := enrollShare(t, filepath.Join(root, "extensions", "remote", "keys", "work"), owner)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		ctx := r.Context()
		challenge := "review"
		raw, _ := nostr.AuthEnvelope{Challenge: &challenge}.MarshalJSON()
		if ws.Write(ctx, websocket.MessageText, raw) != nil {
			return
		}
		for {
			_, raw, err := ws.Read(ctx)
			if err != nil {
				return
			}
			msg, err := nostr.ParseMessage(string(raw))
			if err != nil {
				continue
			}
			var id nostr.ID
			switch m := msg.(type) {
			case *nostr.AuthEnvelope:
				id = m.Event.ID
			case *nostr.EventEnvelope:
				id = m.ID
				if event != nil {
					event()
				}
			default:
				continue
			}
			ok, _ := nostr.OKEnvelope{EventID: id, OK: true}.MarshalJSON()
			if ws.Write(ctx, websocket.MessageText, ok) != nil {
				return
			}
		}
	}))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	sh := manifest.Share{Target: "cx", Session: "work", OwnerPubKey: tag.OwnerPubKey, NativeSessionID: "thread-1", Activity: true}
	cfg, err := relayConfigFor(root, url, sh)()
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	conn, err := relay.Connect(context.Background(), cfg)
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); srv.Close() })
	as := &activityShare{share: sh, body: nostr.SecretKey(body.Secret()), owner: nostr.GetPublicKey(owner), stateDir: filepath.Join(root, "activity")}
	return conn, as
}

func reviewWait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timeout: " + what)
		}
		time.Sleep(time.Millisecond)
	}
}
func reviewNote() codex.Notification {
	return codex.Notification{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","id":"item-1","text":"hello"}}`)}
}

// codex #871 r1 #1: the fence was checked once per drain, so frames queued
// behind a held OK went out after a replacement session attached.
func TestActivityFenceHoldsBetweenQueuedPublications(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var count atomic.Int32
	conn, as := reviewActivityConn(t, func() {
		if count.Add(1) == 1 {
			close(entered)
			<-release
		}
	})
	src := &observedRuntime{}
	var native atomic.Value
	native.Store("thread-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		as.export(ctx, conn, src, func(string) string { return native.Load().(string) }, io.Discard)
	}()
	reviewWait(t, "observer", func() bool { return as.view() == "exporting" })
	src.emit(reviewNote()) // session_resolved plus the message are queued
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no first publication")
	}
	native.Store("thread-2")
	unblock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("export did not stop")
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("published %d frames; queued frame was sent after native identity changed", n)
	}
}

// codex #871 r1 #2: the read-pump callback encoded and wrote sequence state,
// so a slow directory sync blocked the Codex read pump.
func TestActivityCallbackNeverBlocksTheReadPump(t *testing.T) {
	conn, as := reviewActivityConn(t, nil)
	src := &observedRuntime{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exportDone := make(chan struct{})
	go func() {
		defer close(exportDone)
		as.export(ctx, conn, src, func(string) string { return "thread-1" }, io.Discard)
	}()
	reviewWait(t, "observer", func() bool { return as.view() == "exporting" })
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	restore := fsq.SyncDirAmbientSwapForTest(func(dir string) error {
		if strings.HasPrefix(dir, as.stateDir) {
			enteredOnce.Do(func() { close(entered); <-release })
		}
		return nil
	})
	defer restore()
	callbackDone := make(chan struct{})
	go func() { defer close(callbackDone); src.emit(reviewNote()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no sequence fsync")
	}
	blocked := false
	select {
	case <-callbackDone:
	default:
		blocked = true
	}
	unblock()
	<-callbackDone
	cancel()
	<-exportDone
	if blocked {
		t.Fatal("read-pump observer callback is blocked inside sequence directory sync")
	}
}

// keptObserver keeps its callback after stop, like a callback the pump has
// already copied out of the observer map.
type keptObserver struct {
	mu sync.Mutex
	fn func(codex.Notification)
}

func (k *keptObserver) ObserveNotifications(fn func(codex.Notification)) func() {
	k.mu.Lock()
	k.fn = fn
	k.mu.Unlock()
	return func() {}
}

// codex #871 r1 #3: a callback already copied before stop still enqueued
// and wrote sequence state after the sink was closed.
func TestActivityCallbackAfterCleanupDoesNothing(t *testing.T) {
	conn, as := reviewActivityConn(t, nil)
	src := &keptObserver{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		as.export(ctx, conn, src, func(string) string { return "thread-1" }, io.Discard)
	}()
	reviewWait(t, "observer", func() bool { return as.view() == "exporting" })
	cancel()
	<-done
	src.mu.Lock()
	fn := src.fn
	src.mu.Unlock()
	fn(reviewNote())
	time.Sleep(100 * time.Millisecond)
	if entries, err := os.ReadDir(as.stateDir); err == nil && len(entries) > 0 {
		t.Fatal("a callback after cleanup wrote sequence state")
	}
}

// codex #871 r2: the queue bounded only the count, so 256 native frames of
// up to 16 MiB could be copied and held. Admission now checks the method,
// the per-item cap and the byte budget before any copy.
func TestActivityInboxBoundsBytesBeforeCopying(t *testing.T) {
	b := newActivityInbox()
	big := json.RawMessage(strings.Repeat("x", activityItemBytes))
	if b.offer(codex.Notification{Method: "item/completed", Params: append(big, 'x')}) {
		t.Fatal("admitted a payload over the per-item cap")
	}
	if b.offer(codex.Notification{Method: "thread/tokenUsage", Params: json.RawMessage(`{}`)}) {
		t.Fatal("admitted a method the projection ignores")
	}
	admitted := 0
	for i := 0; i < activityQueueBytes/activityItemBytes+1; i++ {
		if b.offer(codex.Notification{Method: "item/completed", Params: big}) {
			admitted++
		}
	}
	if admitted != activityQueueBytes/activityItemBytes {
		t.Fatalf("admitted %d MiB, want the %d MiB budget", admitted, activityQueueBytes>>20)
	}
	b.taken(<-b.q)
	if !b.offer(codex.Notification{Method: "item/completed", Params: big}) {
		t.Fatal("a taken item's bytes were not released")
	}
}
