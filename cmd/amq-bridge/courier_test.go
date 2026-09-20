package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	ackIDs         []string
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
	f.ackIDs = append(f.ackIDs, request.Receipt.TransferID)
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

func TestPollOneBadTransferDoesNotWedgeTheBatch(t *testing.T) {
	// review-827-r1 P1b: an uncertain ledger history for one transfer must
	// not abort the poll batch. The bad envelope stays un-ACKed (rendezvous
	// redelivers); the envelope behind it applies and ACKs normally.
	fake, server := newFakeRendezvous(t)
	receiverRoot := newBridgeRoot(t, "claude")

	bad := testSignedEnvelope(t, "msg-bad", "thread-bad", "poison payload A")
	good := testSignedEnvelope(t, "msg-good", "thread-good", "healthy payload B")
	fake.mu.Lock()
	fake.queue = append(fake.queue, bad, good)
	fake.accepted[bad.TransferID] = bad
	fake.accepted[good.TransferID] = good
	fake.mu.Unlock()

	// Pre-seed a torn ledger for the bad transfer key: unparseable-only file
	// -> torn with no valid record -> the fail-closed uncertain refusal.
	ledgerDir := filepath.Join(receiverRoot, "bridge", "transfer-ledger", "mac_claude")
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tornName := bad.SourceHost + "-" + bad.TransferID + ".jsonl"
	if err := os.WriteFile(filepath.Join(ledgerDir, tornName), []byte("{\"version\":1,\"sta"), 0o600); err != nil {
		t.Fatal(err)
	}

	receiver := testCourier(t, Config{
		Root: receiverRoot, RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"}, AllowedSourceHosts: []string{"grok-host"},
	})
	poll, err := receiver.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce must not abort the batch on a refused transfer: %v", err)
	}
	if len(poll.Refused) != 1 || !poll.Refused[0].Uncertain || poll.Refused[0].TransferID != bad.TransferID {
		t.Fatalf("refused = %#v, want the bad transfer as uncertain", poll.Refused)
	}
	if len(poll.Receipts) != 1 || poll.Receipts[0].TransferID != good.TransferID {
		t.Fatalf("receipts = %#v, want exactly the good transfer committed", poll.Receipts)
	}
	// The good envelope was ACKed; the bad one was not.
	fake.mu.Lock()
	ackIDs := append([]string(nil), fake.ackIDs...)
	fake.mu.Unlock()
	if len(ackIDs) != 1 || ackIDs[0] != good.TransferID {
		t.Fatalf("acks = %v, want only the good transfer ACKed", ackIDs)
	}
	// The good transfer is in the Maildir; the bad one is not.
	newDir := filepath.Join(receiverRoot, "agents", "claude", "inbox", "new")
	entries, err := os.ReadDir(newDir)
	if err != nil {
		t.Fatal(err)
	}
	wantGood := bridge.TransferFilename(good.SourceHost, good.TransferID)
	if len(entries) != 1 || entries[0].Name() != wantGood {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("inbox entries = %v, want only %s", names, wantGood)
	}
}

func TestPollConflictSkipsWithoutReceiptOrAck(t *testing.T) {
	// review-827-r1 P1b, conflict branch: a same-key/different-digest
	// arrival is skipped (refused list), no receipt, no ACK — the winner's
	// state is untouched and the batch behind it proceeds.
	fake, server := newFakeRendezvous(t)
	receiverRoot := newBridgeRoot(t, "claude")

	winner := testSignedEnvelope(t, "msg-winner", "thread-w", "winner payload")
	loser := winner
	loser.Payload = []byte("losing payload")
	sum := sha256.Sum256(loser.Payload)
	loser.PayloadSHA256 = hex.EncodeToString(sum[:])
	loser.TransferID = winner.TransferID // same key, different digest
	if err := bridge.SignEnvelope(&loser, testHostKey("grok-host", "1")); err != nil {
		t.Fatal(err)
	}
	after := testSignedEnvelope(t, "msg-after", "thread-a", "after payload")
	fake.mu.Lock()
	fake.queue = append(fake.queue, winner, loser, after)
	fake.accepted[winner.TransferID] = winner
	fake.accepted[after.TransferID] = after
	fake.mu.Unlock()

	receiver := testCourier(t, Config{
		Root: receiverRoot, RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"}, AllowedSourceHosts: []string{"grok-host"},
	})
	poll, err := receiver.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(poll.Receipts) != 2 {
		t.Fatalf("receipts = %d, want 2 (winner + after)", len(poll.Receipts))
	}
	if len(poll.Refused) != 1 || !poll.Refused[0].Conflict || poll.Refused[0].TransferID != loser.TransferID {
		t.Fatalf("refused = %#v, want the loser as conflict", poll.Refused)
	}
	// Winner's artifact holds the winner's bytes (immutable).
	winnerPath := filepath.Join(receiverRoot, "agents", "claude", "inbox", "new", bridge.TransferFilename(winner.SourceHost, winner.TransferID))
	data, err := os.ReadFile(winnerPath)
	if err != nil || string(data) != "winner payload" {
		t.Fatalf("winner artifact = %q err=%v, want winner payload untouched", data, err)
	}
}

func TestPollRetryableRejectedIsRefusedListedNotFatal(t *testing.T) {
	// review-827-r1 P1b, non-committed branch: a retryable-rejected outcome
	// (e.g. unwritable inbox/tmp) is refused-and-listed, the poll does not
	// error, and the envelope is not ACKed (rendezvous redelivers after the
	// condition clears).
	fake, server := newFakeRendezvous(t)
	receiverRoot := newBridgeRoot(t, "claude")

	bad := testSignedEnvelope(t, "msg-retryable", "thread-r", "blocked payload")
	fake.mu.Lock()
	fake.queue = append(fake.queue, bad)
	fake.accepted[bad.TransferID] = bad
	fake.mu.Unlock()

	tmpDir := filepath.Join(receiverRoot, "agents", "claude", "inbox", "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmpDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmpDir, 0o700) })

	receiver := testCourier(t, Config{
		Root: receiverRoot, RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"}, AllowedSourceHosts: []string{"grok-host"},
	})
	poll, err := receiver.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce must surface the refusal in the result, not the error: %v", err)
	}
	if len(poll.Refused) != 1 || poll.Refused[0].TransferID != bad.TransferID {
		t.Fatalf("refused = %#v, want the blocked transfer", poll.Refused)
	}
	if len(poll.Receipts) != 0 {
		t.Fatalf("receipts = %d, want none for a refused-only poll", len(poll.Receipts))
	}
	fake.mu.Lock()
	ackCount := fake.ackCount
	fake.mu.Unlock()
	if ackCount != 0 {
		t.Fatalf("acks = %d, want 0 (refused transfer is not ACKed)", ackCount)
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

// testSignedEnvelope builds a valid, signed envelope with a custom payload
// and distinct message/thread ids (helper for multi-envelope poll tests).
func testSignedEnvelope(t *testing.T, id, thread, body string) bridge.Envelope {
	t.Helper()
	digest := sha256.Sum256([]byte(body))
	env := bridge.Envelope{
		Version:         bridge.EnvelopeVersion,
		SourceHost:      "grok-host",
		SourceHandle:    "codex",
		DestAlias:       "mac/claude",
		SourceMessageID: id,
		ThreadID:        thread,
		PayloadSHA256:   hex.EncodeToString(digest[:]),
		KeyGeneration:   "1",
		Payload:         []byte(body),
	}
	env.TransferID = bridge.DeriveTransferID(env.SourceHost, env.SourceHandle, env.SourceMessageID, env.DestAlias)
	if err := bridge.SignEnvelope(&env, testHostKey("grok-host", "1")); err != nil {
		t.Fatal(err)
	}
	return env
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

// TestRunOnceSurfacesDiagnosticsAndRefused (review-827-r2 P2-3 + r1 CLI gap):
// writeRunResult must emit refused transfers and unresolved ledger state —
// previously PollResult.Refused was serialized by nothing and
// UnresolvedTransfers had no production caller, so a stuck retryable
// transfer was invisible on the CLI.
func TestRunOnceSurfacesDiagnosticsAndRefused(t *testing.T) {
	// Unit-level: writeRunResult serialization.
	var buf bytes.Buffer
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- writeRunResult(RunResult{
			Poll: PollResult{Refused: []RefusedTransfer{{TransferID: "tid", Reason: "conflict", Conflict: true}}},
			Diagnostics: []LedgerDiagnostic{
				{TransferID: "tid2", State: string(bridge.LedgerRejected), Retryable: true, Reason: "apply failed (retryable): x"},
			},
		})
		_ = w.Close()
	}()
	if err := <-writeErr; err != nil {
		os.Stdout = old
		t.Fatalf("writeRunResult: %v", err)
	}
	os.Stdout = old
	out, _ := io.ReadAll(r)
	_ = buf
	if !strings.Contains(string(out), `"refused"`) || !strings.Contains(string(out), `"tid"`) {
		t.Fatalf("writeRunResult output missing refused transfers: %s", out)
	}
	if !strings.Contains(string(out), `"retryable":true`) || !strings.Contains(string(out), "tid2") {
		t.Fatalf("writeRunResult output missing ledger diagnostics: %s", out)
	}
}

// TestPollPublishedDurabilityUnknownWithholdsReceiptAndAck (codex r2-r2
// finding 1): a delivery whose destination directory sync fails is PUBLISHED
// but not durably committed. The poll must NOT emit a success receipt and
// must NOT ACK — the transfer is refused-listed, and a repaired re-poll
// promotes to committed with a receipt and ACK without duplicating.
func TestPollPublishedDurabilityUnknownWithholdsReceiptAndAck(t *testing.T) {
	fake, server := newFakeRendezvous(t)
	receiverRoot := newBridgeRoot(t, "claude")

	env := testSignedEnvelope(t, "msg-cde", "thread-cde", "cde payload")
	fake.mu.Lock()
	fake.queue = append(fake.queue, env)
	fake.accepted[env.TransferID] = env
	fake.mu.Unlock()

	receiver := testCourier(t, Config{
		Root: receiverRoot, RendezvousURL: server.URL, DestAlias: "mac/claude",
		AllowedDestAliases: []string{"mac/claude"}, AllowedSourceHosts: []string{"grok-host"},
	})
	fsq.SetPackageSyncDirFaultForTest(func(dir string) error {
		if strings.HasSuffix(dir, filepath.Join("inbox", "new")) {
			return fmt.Errorf("injected EIO")
		}
		return nil
	})
	t.Cleanup(func() { fsq.SetPackageSyncDirFaultForTest(nil) })

	poll, err := receiver.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(poll.Receipts) != 0 {
		t.Fatalf("receipts = %d, want 0 (publication is not durable completion)", len(poll.Receipts))
	}
	if len(poll.Refused) != 1 || !poll.Refused[0].Uncertain || poll.Refused[0].TransferID != env.TransferID {
		t.Fatalf("refused = %#v, want the published-but-unverified transfer", poll.Refused)
	}
	fake.mu.Lock()
	ackCount := fake.ackCount
	fake.mu.Unlock()
	if ackCount != 0 {
		t.Fatalf("acks = %d, want 0 (durability unverified)", ackCount)
	}
	// The artifact IS visible in new (publication fact).
	newDir := filepath.Join(receiverRoot, "agents", "claude", "inbox", "new")
	entries, err := os.ReadDir(newDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("new entries = %d err=%v, want 1 (published)", len(entries), err)
	}

	// Repair the sync and re-poll (rendezvous redelivers; the ledger
	// re-verifies durability WITHOUT re-applying): committed + receipt + ACK.
	fsq.SetPackageSyncDirFaultForTest(nil)
	poll2, err := receiver.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	if len(poll2.Receipts) != 1 || poll2.Receipts[0].TransferID != env.TransferID {
		t.Fatalf("receipts = %#v, want the promoted committed receipt", poll2.Receipts)
	}
	if len(poll2.Refused) != 0 {
		t.Fatalf("refused = %#v, want none after durability verified", poll2.Refused)
	}
	fake.mu.Lock()
	ackCount2 := fake.ackCount
	fake.mu.Unlock()
	if ackCount2 != 1 {
		t.Fatalf("acks = %d, want 1 after promotion", ackCount2)
	}
	// No duplicate delivery.
	newEntries2, _ := os.ReadDir(newDir)
	curDir := filepath.Join(receiverRoot, "agents", "claude", "inbox", "cur")
	curEntries2, _ := os.ReadDir(curDir)
	if len(newEntries2)+len(curEntries2) != 1 {
		t.Fatalf("DUPLICATE: new=%d cur=%d, want 1 total", len(newEntries2), len(curEntries2))
	}
}
