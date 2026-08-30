package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type fakeRendezvous struct {
	mu             sync.Mutex
	queue          []bridge.Envelope
	accepted       map[string]bridge.Envelope
	postCount      int
	ackCount       int
	dropFirstAck   bool
	wrongPostStage bool
	seenRaw        [][]byte
}

func newFakeRendezvous(t *testing.T) (*fakeRendezvous, *httptest.Server) {
	t.Helper()
	fake := &fakeRendezvous{accepted: make(map[string]bridge.Envelope)}
	server := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(server.Close)
	return fake, server
}

func (f *fakeRendezvous) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == transfersPath && r.Method == http.MethodPost {
		f.handlePost(w, r)
		return
	}
	if r.URL.Path == transfersPath && r.Method == http.MethodGet {
		f.handlePoll(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, transfersPath+"/") && strings.HasSuffix(r.URL.Path, "/ack") && r.Method == http.MethodPost {
		f.handleAck(w, r)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (f *fakeRendezvous) handlePost(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	env, err := bridge.UnmarshalEnvelope(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postCount++
	f.seenRaw = append(f.seenRaw, append([]byte(nil), raw...))
	if previous, ok := f.accepted[env.TransferID]; ok {
		if !strings.EqualFold(previous.PayloadSHA256, env.PayloadSHA256) {
			http.Error(w, "transfer digest conflict", http.StatusConflict)
			return
		}
	} else {
		f.accepted[env.TransferID] = env
		f.queue = append(f.queue, env)
	}
	stage := ReceiptTransportAccepted
	if f.wrongPostStage {
		stage = ReceiptDestinationMaildirCommit
	}
	writeJSON(w, http.StatusOK, transportResponse{Receipt: wireReceipt{
		Stage:         stage,
		TransferID:    env.TransferID,
		PayloadSHA256: env.PayloadSHA256,
	}})
}

func (f *fakeRendezvous) handlePoll(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if _, err := fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit); err != nil || limit < 1 {
		http.Error(w, "invalid limit", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	count := limit
	if len(f.queue) < count {
		count = len(f.queue)
	}
	envelopes := make([]json.RawMessage, 0, count)
	for _, env := range f.queue[:count] {
		raw, err := bridge.MarshalEnvelope(env)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		envelopes = append(envelopes, raw)
	}
	writeJSON(w, http.StatusOK, pollResponse{Envelopes: envelopes})
}

func (f *fakeRendezvous) handleAck(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var request ackRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ackCount++
	env, ok := f.accepted[request.Receipt.TransferID]
	if !ok || !strings.EqualFold(env.PayloadSHA256, request.Receipt.PayloadSHA256) {
		http.Error(w, "ack conflict", http.StatusConflict)
		return
	}
	if request.Receipt.Stage != ReceiptDestinationMaildirCommit {
		http.Error(w, "wrong ack stage", http.StatusBadRequest)
		return
	}
	if f.dropFirstAck {
		f.dropFirstAck = false
		http.Error(w, "simulated lost ack", http.StatusServiceUnavailable)
		return
	}
	for i, queued := range f.queue {
		if queued.TransferID == env.TransferID {
			f.queue = append(f.queue[:i], f.queue[i+1:]...)
			break
		}
	}
	writeJSON(w, http.StatusOK, transportResponse(request))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func TestPushPollAndTypedReceipts(t *testing.T) {
	fake, server := newFakeRendezvous(t)
	senderRoot := newBridgeRoot(t, "codex")
	receiverRoot := newBridgeRoot(t, "claude")

	message := testMessage(t, "msg-1", "thread-1", "codex", "hello")
	spool := filepath.Join(senderRoot, "bridge", "outbox", "codex", "new")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	spoolPath := filepath.Join(spool, "message.md")
	if err := os.WriteFile(spoolPath, message, 0o600); err != nil {
		t.Fatal(err)
	}

	sender := testCourier(t, Config{
		Root:               senderRoot,
		RendezvousURL:      server.URL,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		DestAlias:          "mac/claude",
		AllowedDestAliases: []string{"mac/claude"},
	})
	push, err := sender.PushOnce(context.Background())
	if err != nil {
		t.Fatalf("PushOnce: %v", err)
	}
	if len(push.Receipts) != 1 || push.Receipts[0].Stage != ReceiptTransportAccepted {
		t.Fatalf("push receipts = %#v, want one transport_accepted receipt", push.Receipts)
	}
	if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
		t.Fatalf("spool source still exists, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(senderRoot, "bridge", "outbox", "codex", "sent", "message.md")); err != nil {
		t.Fatalf("sent archive missing: %v", err)
	}

	receiver := testCourier(t, Config{
		Root:               receiverRoot,
		RendezvousURL:      server.URL,
		DestAlias:          "mac/claude",
		AllowedDestAliases: []string{"mac/claude"},
		AllowedSourceHosts: []string{"grok-host"},
	})
	poll, err := receiver.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(poll.Receipts) != 1 || poll.Receipts[0].Stage != ReceiptDestinationMaildirCommit || poll.Receipts[0].Replayed {
		t.Fatalf("poll receipts = %#v, want one non-replayed destination commit", poll.Receipts)
	}
	env := fake.accepted[push.Receipts[0].TransferID]
	committed, err := os.ReadFile(filepath.Join(receiverRoot, "agents", "claude", "inbox", "new", bridge.TransferFilename(env.SourceHost, env.TransferID)))
	if err != nil {
		t.Fatalf("read committed message: %v", err)
	}
	if string(committed) != string(message) {
		t.Fatalf("committed payload changed: got %q, want %q", committed, message)
	}
	if _, err := receiver.PollOnce(context.Background()); err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}

	fake.mu.Lock()
	postCount, ackCount, seen := fake.postCount, fake.ackCount, append([][]byte(nil), fake.seenRaw...)
	fake.mu.Unlock()
	if postCount != 1 || ackCount != 1 {
		t.Fatalf("rendezvous calls post=%d ack=%d, want 1/1", postCount, ackCount)
	}
	if len(seen) != 1 {
		t.Fatalf("seen envelopes = %d, want 1", len(seen))
	}
	var fields map[string]any
	if err := json.Unmarshal(seen[0], &fields); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"root", "path", "argv", "env", "executable", "endpoint"} {
		if _, ok := fields[forbidden]; ok {
			t.Fatalf("envelope contains forbidden routing field %q: %s", forbidden, seen[0])
		}
	}
}

func TestPushUsesDestSidecarNotCourierDestAlias(t *testing.T) {
	fake, server := newFakeRendezvous(t)
	root := newBridgeRoot(t, "codex")
	message := testMessage(t, "msg-dest", "thread-dest", "codex", "route me")
	spool := filepath.Join(root, "bridge", "outbox", "codex", "new")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	spoolPath := filepath.Join(spool, "routed.md")
	if err := os.WriteFile(spoolPath, message, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bridge.DestSidecarPath(spoolPath), []byte("mac/cursor\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sender := testCourier(t, Config{
		Root:               root,
		RendezvousURL:      server.URL,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		DestAlias:          "mac/claude",
		AllowedDestAliases: []string{"mac/claude", "mac/cursor"},
	})
	push, err := sender.PushOnce(context.Background())
	if err != nil {
		t.Fatalf("PushOnce: %v", err)
	}
	if len(push.Receipts) != 1 {
		t.Fatalf("push receipts = %#v", push.Receipts)
	}
	fake.mu.Lock()
	env := fake.accepted[push.Receipts[0].TransferID]
	fake.mu.Unlock()
	if env.DestAlias != "mac/cursor" {
		t.Fatalf("wire dest_alias = %q, want mac/cursor (sidecar), courier dest was mac/claude", env.DestAlias)
	}
}

func TestPollReplaysAfterLostAckWithoutDuplicateMaildirEntry(t *testing.T) {
	fake, server := newFakeRendezvous(t)
	senderRoot := newBridgeRoot(t, "codex")
	receiverRoot := newBridgeRoot(t, "claude")
	fake.dropFirstAck = true

	message := testMessage(t, "msg-replay", "thread-replay", "codex", "replay me")
	spool := filepath.Join(senderRoot, "bridge", "outbox", "codex", "new")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, "replay.md"), message, 0o600); err != nil {
		t.Fatal(err)
	}
	sender := testCourier(t, Config{
		Root: senderRoot, RendezvousURL: server.URL, SourceHost: "grok-host", SourceHandle: "codex",
		DestAlias: "mac/claude", AllowedDestAliases: []string{"mac/claude"},
	})
	if _, err := sender.PushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	receiver := testCourier(t, Config{
		Root: receiverRoot, RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"}, AllowedSourceHosts: []string{"grok-host"},
	})
	if _, err := receiver.PollOnce(context.Background()); err == nil {
		t.Fatal("first PollOnce succeeded despite simulated lost ACK")
	}
	second, err := receiver.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("replay PollOnce: %v", err)
	}
	if len(second.Receipts) != 1 || !second.Receipts[0].Replayed {
		t.Fatalf("replay receipts = %#v, want one replayed commit", second.Receipts)
	}
	entries, err := os.ReadDir(filepath.Join(receiverRoot, "agents", "claude", "inbox", "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want one after replay", len(entries))
	}
}

func TestPushDigestMismatchFailsClosed(t *testing.T) {
	_, server := newFakeRendezvous(t)
	root := newBridgeRoot(t, "codex")
	spool := filepath.Join(root, "bridge", "outbox", "codex", "new")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spool, "same-id.md")
	if err := os.WriteFile(path, testMessage(t, "same-id", "thread", "codex", "first"), 0o600); err != nil {
		t.Fatal(err)
	}
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: server.URL, SourceHost: "grok-host", SourceHandle: "codex",
		DestAlias: "mac/claude", AllowedDestAliases: []string{"mac/claude"},
	})
	first, err := courier.PushOnce(context.Background())
	if err != nil || len(first.Receipts) != 1 {
		t.Fatalf("first PushOnce receipts=%#v err=%v", first.Receipts, err)
	}
	receiptPath := filepath.Join(root, "bridge", "receipts", receiptFilename(first.Receipts[0].TransferID, ReceiptTransportAccepted))
	if err := os.Remove(receiptPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, testMessage(t, "same-id", "thread", "codex", "changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := courier.PushOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("digest mismatch error = %v, want rendezvous conflict", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("conflicting spool file was removed: %v", err)
	}
}

func TestHTTP200WithWrongStageDoesNotDrainSpool(t *testing.T) {
	fake, server := newFakeRendezvous(t)
	fake.wrongPostStage = true
	root := newBridgeRoot(t, "codex")
	spool := filepath.Join(root, "bridge", "outbox", "codex", "new")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spool, "wrong-stage.md")
	if err := os.WriteFile(path, testMessage(t, "wrong-stage", "thread", "codex", "keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: server.URL, SourceHost: "grok-host", SourceHandle: "codex",
		DestAlias: "mac/claude", AllowedDestAliases: []string{"mac/claude"},
	})
	if _, err := courier.PushOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected rendezvous receipt stage") {
		t.Fatalf("wrong-stage error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spool file disappeared after wrong receipt: %v", err)
	}
}

func TestPushRefusesRedirectWithoutDrainingSpool(t *testing.T) {
	var redirectHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect-target" {
			redirectHits++
			writeJSON(w, http.StatusOK, transportResponse{Receipt: wireReceipt{Stage: ReceiptTransportAccepted}})
			return
		}
		if r.URL.Path != transfersPath || r.Method != http.MethodPost {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Location", "/redirect-target")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)

	root := newBridgeRoot(t, "codex")
	spool := filepath.Join(root, "bridge", "outbox", "codex", "new")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spool, "redirect.md")
	if err := os.WriteFile(path, testMessage(t, "redirect", "thread", "codex", "keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: server.URL, SourceHost: "grok-host", SourceHandle: "codex",
		DestAlias: "mac/claude", AllowedDestAliases: []string{"mac/claude"},
	})
	if _, err := courier.PushOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect PushOnce error = %v, want 302 refusal", err)
	}
	if redirectHits != 0 {
		t.Fatalf("redirect target was followed %d times", redirectHits)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("spool file disappeared after redirect: %v", err)
	}
}

func TestPollRefusesRedirectOnAckWithoutFollowing(t *testing.T) {
	env := testEnvelope(t, "ack-redirect")
	raw, err := bridge.MarshalEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	var redirectHits, ackHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/redirect-target":
			redirectHits++
			writeJSON(w, http.StatusOK, transportResponse{Receipt: wireReceipt{
				Stage: ReceiptDestinationMaildirCommit, TransferID: env.TransferID, PayloadSHA256: env.PayloadSHA256,
			}})
		case r.URL.Path == transfersPath && r.Method == http.MethodGet:
			writeJSON(w, http.StatusOK, pollResponse{Envelopes: []json.RawMessage{raw}})
		case strings.HasPrefix(r.URL.Path, transfersPath+"/") && strings.HasSuffix(r.URL.Path, "/ack") && r.Method == http.MethodPost:
			ackHits++
			w.Header().Set("Location", "/redirect-target")
			w.WriteHeader(http.StatusFound)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	root := newBridgeRoot(t, "claude")
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"}, AllowedSourceHosts: []string{"grok-host"},
	})
	if _, err := courier.PollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect ACK PollOnce error = %v, want 302 refusal", err)
	}
	if ackHits != 1 {
		t.Fatalf("ACK requests = %d, want 1", ackHits)
	}
	if redirectHits != 0 {
		t.Fatalf("redirect target was followed %d times", redirectHits)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "claude", "inbox", "new", bridge.TransferFilename(env.SourceHost, env.TransferID))); err != nil {
		t.Fatalf("local delivery missing before failed ACK: %v", err)
	}
}

func TestNewCourierRendezvousURLPolicy(t *testing.T) {
	for _, test := range []struct {
		name string
		url  string
		want string
	}{
		{name: "non-loopback http", url: "http://example.test", want: "non-loopback"},
		{name: "userinfo", url: "https://user:pass@example.test", want: "userinfo"},
		{name: "fragment", url: "https://example.test/path#fragment", want: "userinfo or fragment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewCourier(Config{
				Root: t.TempDir(), RendezvousURL: test.url, DestAlias: "mac/claude",
				AllowedDestAliases: []string{"mac/claude"},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewCourier(%q) error = %v, want %q refusal", test.url, err, test.want)
			}
		})
	}

	if _, err := NewCourier(Config{
		Root: hostIDRoot(t, "mac"), RendezvousURL: "https://example.test", DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"},
	}); err != nil {
		t.Fatalf("HTTPS rendezvous URL rejected: %v", err)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	if _, err := NewCourier(Config{
		Root: hostIDRoot(t, "mac"), RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"},
	}); err != nil {
		t.Fatalf("loopback HTTP rendezvous URL rejected: %v", err)
	}
}

func TestNewCourierRequiresDestinationAllowlist(t *testing.T) {
	_, err := NewCourier(Config{Root: t.TempDir(), RendezvousURL: "https://example.test", DestAlias: "mac/claude"})
	if err == nil || !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("NewCourier error = %v, want allowlist refusal", err)
	}
}

func TestPollRequiresSourceAllowlistAndLocalReceiveAlias(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: "https://example.test", DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"},
	})
	if _, err := courier.PollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "allow-source-host") {
		t.Fatalf("empty source allowlist error = %v", err)
	}

	foreign := testCourier(t, Config{
		Root: root, RendezvousURL: "https://example.test",
		SourceHost: "mac", SourceHandle: "codex",
		DestAlias: "grok/claude", AllowedDestAliases: []string{"grok/claude", "mac/claude"},
		AllowedSourceHosts: []string{"grok"},
	})
	if _, err := foreign.PollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "receive-alias") {
		t.Fatalf("foreign dest poll error = %v", err)
	}

	split := testCourier(t, Config{
		Root: root, RendezvousURL: "https://example.test",
		SourceHost: "mac", SourceHandle: "codex",
		DestAlias: "grok/claude", ReceiveAlias: "mac/claude",
		AllowedDestAliases: []string{"grok/claude", "mac/claude"},
		AllowedSourceHosts: []string{"grok"},
	})
	if err := split.guardPollIdentity(); err != nil {
		t.Fatalf("split send/receive identity: %v", err)
	}
}

func newBridgeRoot(t *testing.T, agent string) string {
	t.Helper()
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatal(err)
	}
	return root
}

func hostIDRoot(t *testing.T, host string) string {
	t.Helper()
	root := t.TempDir()
	if err := bridge.WriteHostID(root, host); err != nil {
		t.Fatal(err)
	}
	return root
}

func testHostKey(host, generation string) bridge.HostKey {
	if generation == "" {
		generation = defaultKeyGeneration
	}
	sum := sha256.Sum256([]byte("amq-bridge-test-seed:" + host))
	return bridge.HostKey{Generation: generation, Private: ed25519.NewKeyFromSeed(sum[:ed25519.SeedSize])}
}

func ensureHostID(t *testing.T, root, host string) {
	t.Helper()
	if _, err := os.Lstat(bridge.HostIDPath(root)); err == nil {
		got, loadErr := bridge.LoadHostID(root)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if got != host {
			t.Fatalf("host-id = %q, want %q", got, host)
		}
		return
	}
	if err := bridge.WriteHostID(root, host); err != nil {
		t.Fatal(err)
	}
}

func ensureIdentity(t *testing.T, root, host string) {
	t.Helper()
	if _, err := os.Lstat(bridge.IdentityPath(root)); err == nil {
		return
	}
	if err := bridge.WriteIdentity(root, testHostKey(host, defaultKeyGeneration)); err != nil {
		t.Fatal(err)
	}
}

func ensureTrusted(t *testing.T, root, sourceHost string) {
	t.Helper()
	if _, err := os.Lstat(bridge.TrustedPath(root, sourceHost)); err == nil {
		return
	}
	key := testHostKey(sourceHost, defaultKeyGeneration)
	if err := bridge.WriteTrusted(root, sourceHost, key.Public(), defaultKeyGeneration); err != nil {
		t.Fatal(err)
	}
}

func testCourier(t *testing.T, cfg Config) *Courier {
	t.Helper()
	hostID := cfg.SourceHost
	if hostID == "" {
		alias := cfg.ReceiveAlias
		if alias == "" {
			alias = cfg.DestAlias
		}
		host, _, err := bridge.ParseAlias(alias)
		if err != nil {
			t.Fatal(err)
		}
		hostID = host
	}
	ensureHostID(t, cfg.Root, hostID)
	if cfg.SourceHost != "" {
		ensureIdentity(t, cfg.Root, cfg.SourceHost)
	}
	for _, host := range cfg.AllowedSourceHosts {
		ensureTrusted(t, cfg.Root, strings.TrimSpace(host))
	}
	courier, err := NewCourier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return courier
}

func testMessage(t *testing.T, id, thread, from, body string) []byte {
	t.Helper()
	data, err := (format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      id,
			From:    from,
			To:      []string{"claude"},
			Thread:  thread,
			Created: time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
		},
		Body: body,
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testEnvelope(t *testing.T, transferID string) bridge.Envelope {
	t.Helper()
	payload := []byte("poll payload")
	digest := sha256.Sum256(payload)
	env := bridge.Envelope{
		Version:         bridge.EnvelopeVersion,
		SourceHost:      "grok-host",
		SourceHandle:    "codex",
		DestAlias:       "mac/claude",
		SourceMessageID: transferID + "-message",
		ThreadID:        transferID + "-thread",
		PayloadSHA256:   hex.EncodeToString(digest[:]),
		KeyGeneration:   "1",
		Payload:         payload,
	}
	env.TransferID = bridge.DeriveTransferID(env.SourceHost, env.SourceHandle, env.SourceMessageID, env.DestAlias)
	if err := bridge.SignEnvelope(&env, testHostKey("grok-host", "1")); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestWireEnvelopeRejectsMalformedJSONBeforeApply(t *testing.T) {
	_, server := newFakeRendezvous(t)
	root := newBridgeRoot(t, "claude")
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"}, AllowedSourceHosts: []string{"grok-host"},
	})
	// This exercises the wire decoder independently of the local apply path.
	bad := []byte(`{"version":1,"transfer_id":"t1","source_host":"grok-host","source_handle":"codex","dest_alias":"mac/claude","source_message_id":"m","thread_id":"t","payload_sha256":"2bb80d537b1da3e38bd30361aa855686bde0ba3f5f3d7f8b1e8b4b9c3f2e7f6a","key_generation":"1","signature":"` + strings.Repeat("0", 128) + `","payload":"aGk=","root":"/tmp"}`)
	serverWithBadEnvelope := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, pollResponse{Envelopes: []json.RawMessage{bad}})
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(serverWithBadEnvelope.Close)
	courier.cfg.RendezvousURL = serverWithBadEnvelope.URL
	if _, err := courier.PollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "decode polled envelope") {
		t.Fatalf("unknown-field poll error = %v", err)
	}
}

func TestWireEnvelopeUnknownFieldIsNotApplied(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	env := testEnvelope(t, "unknown-field-real")
	valid, err := bridge.MarshalEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(valid, &fields); err != nil {
		t.Fatal(err)
	}
	fields["unexpected"] = json.RawMessage(`"reject"`)
	bad, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var ackHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, pollResponse{Envelopes: []json.RawMessage{bad}})
			return
		}
		ackHits++
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"}, AllowedSourceHosts: []string{"grok-host"},
	})
	if _, err := courier.PollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "decode polled envelope") {
		t.Fatalf("unknown-field poll error = %v", err)
	}
	if ackHits != 0 {
		t.Fatalf("unknown envelope reached ACK path %d times", ackHits)
	}
	entries, err := os.ReadDir(filepath.Join(root, "agents", "claude", "inbox", "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unknown envelope was applied: %d inbox entries", len(entries))
	}
}

func TestPollRejectsAllowlistedForgedSourceHost(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	honest := testEnvelope(t, "forged-claim")
	forged := honest
	if err := bridge.SignEnvelope(&forged, testHostKey("attacker", "1")); err != nil {
		t.Fatal(err)
	}
	raw, err := bridge.MarshalEnvelope(forged)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, pollResponse{Envelopes: []json.RawMessage{raw}})
			return
		}
		http.Error(w, "ack should not run", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"}, AllowedSourceHosts: []string{"grok-host"},
	})
	if _, err := courier.PollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "authenticate") {
		t.Fatalf("forged poll error = %v, want authenticate refusal", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "agents", "claude", "inbox", "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("forged envelope was applied: %d inbox entries", len(entries))
	}
}

func TestPollRejectsBumpedTrustedGenerationWithoutAck(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	env := testEnvelope(t, "bumped-gen")
	raw, err := bridge.MarshalEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	var ackHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, pollResponse{Envelopes: []json.RawMessage{raw}})
			return
		}
		if strings.HasPrefix(r.URL.Path, transfersPath+"/") && strings.HasSuffix(r.URL.Path, "/ack") {
			ackHits++
		}
		http.Error(w, "ack should not run", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"}, AllowedSourceHosts: []string{"grok-host"},
	})
	if err := os.Remove(bridge.TrustedPath(root, "grok-host")); err != nil {
		t.Fatal(err)
	}
	rotated := testHostKey("grok-host", "2")
	if err := bridge.WriteTrusted(root, "grok-host", rotated.Public(), "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := courier.PollOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "authenticate") {
		t.Fatalf("bumped-generation poll error = %v, want authenticate refusal", err)
	}
	if ackHits != 0 {
		t.Fatalf("revoked generation reached ACK %d times", ackHits)
	}
	entries, err := os.ReadDir(filepath.Join(root, "agents", "claude", "inbox", "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("revoked generation was applied: %d inbox entries", len(entries))
	}
}

func TestMoveToSentRefusesDifferentExistingArchive(t *testing.T) {
	root := newBridgeRoot(t, "codex")
	spool := filepath.Join(root, "bridge", "outbox", "codex", "new")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	sent := filepath.Join(root, "bridge", "outbox", "codex", "sent")
	if err := os.MkdirAll(sent, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "collision.md"
	if err := os.WriteFile(filepath.Join(sent, name), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, name), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: "https://example.test", SourceHost: "grok-host", SourceHandle: "codex",
		DestAlias: "mac/claude", AllowedDestAliases: []string{"mac/claude"},
	})
	err := courier.moveToSent(name, []byte("new"))
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("moveToSent error = %v, want conflict", err)
	}
	if _, err := os.Stat(filepath.Join(spool, name)); err != nil {
		t.Fatalf("source removed after archive conflict: %v", err)
	}
}

func TestReceiptConflictIsNotSilentlyReplaced(t *testing.T) {
	root := newBridgeRoot(t, "claude")
	courier := testCourier(t, Config{
		Root: root, RendezvousURL: "https://example.test", DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"},
	})
	digest := strings.Repeat("a", 64)
	first := Receipt{Stage: ReceiptDestinationMaildirCommit, TransferID: "transfer-1", PayloadSHA256: digest, EmittedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := courier.writeReceipt(mustOpenRoot(t, root), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.PayloadSHA256 = strings.Repeat("b", 64)
	err := courier.writeReceipt(mustOpenRoot(t, root), second)
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("receipt overwrite error = %v, want conflict", err)
	}
}

func mustOpenRoot(t *testing.T, path string) *fsq.DeliveryRoot {
	t.Helper()
	identity, err := fsq.SnapshotDeliveryRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}
