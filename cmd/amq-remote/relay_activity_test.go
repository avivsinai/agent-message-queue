package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"

	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/activity"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// observedRuntime is a fake attachment with the Codex notification seam.
type observedRuntime struct {
	*fake.Runtime
	mu sync.Mutex
	fn func(codex.Notification)
}

func (o *observedRuntime) ObserveNotifications(fn func(codex.Notification)) func() {
	o.mu.Lock()
	o.fn = fn
	o.mu.Unlock()
	return func() { o.mu.Lock(); o.fn = nil; o.mu.Unlock() }
}

func (o *observedRuntime) emit(n codex.Notification) bool {
	o.mu.Lock()
	fn := o.fn
	o.mu.Unlock()
	if fn != nil {
		fn(n)
	}
	return fn != nil
}

// 611.17 slice 3, through serve's relay wiring: the pinned Codex thread's
// notifications reach the relay as frames the owner can decrypt, and export
// stops while another native session is attached.
func TestActivityExportsPinnedThreadAndStopsOnReplacement(t *testing.T) {
	root := t.TempDir()
	keyDir := filepath.Join(root, "extensions", "remote", "keys", "work")
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	body, tag := enrollShare(t, keyDir, owner)
	lr, srv, url := relaytest.Start(body.PublicKeyHex(), []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()})
	defer srv.Close()
	r := &manifest.Relay{URL: url, Shares: []manifest.Share{{Target: "cx", Session: "work", OwnerPubKey: tag.OwnerPubKey, NativeSessionID: "thread-1", Activity: true}}}
	stateDir := filepath.Join(root, "extensions", "remote")
	edges := buildDMEdges(root, stateDir, r, io.Discard)
	var native atomic.Value
	native.Store("thread-1")
	src := &observedRuntime{Runtime: fake.New("cx", "e1")}
	edges.bind(func(*protocol.Command, core.Source) (any, error) { return nil, nil }, func(string) string { return native.Load().(string) })
	edges.bindAttachments(func(string) (core.Attachment, bool) { return src, true })
	ctx, cancel := context.WithCancel(context.Background())
	wg := startRelays(ctx, root, stateDir, r, edges, io.Discard)
	defer func() { cancel(); wg.Wait() }()

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (activity = %q)", what, edges.activityView("work"))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("export to start", func() bool { return edges.activityView("work") == "exporting" })
	for _, n := range []codex.Notification{
		{Method: "turn/started", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1"}}`)},
		{Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"type":"agentMessage","id":"item-1","text":"hello"}}`)},
	} {
		src.emit(n)
	}
	key, err := nip44.GenerateConversationKey(nostr.GetPublicKey(owner), nostr.SecretKey(body.Secret()))
	if err != nil {
		t.Fatal(err)
	}
	waitFor("a decryptable frame", func() bool {
		for _, evt := range lr.Events() {
			if evt.Kind == activity.KindTelemetry && evt.PubKey.Hex() == body.PublicKeyHex() {
				if plain, err := nip44.Decrypt(evt.Content, key); err == nil && strings.Contains(plain, "hello") {
					return true
				}
			}
		}
		return false
	})
	native.Store("thread-2") // a replacement native session attaches
	waitFor("export to close", func() bool { return strings.HasPrefix(edges.activityView("work"), "closed:") })
}
