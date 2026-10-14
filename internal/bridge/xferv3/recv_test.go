package xferv3

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestFileIdempotentAcrossRelock(t *testing.T) {
	root, snap, localKey, signed, payload := setupRecv(t, "m-replay")
	first, err := File(root, snap, signed, payload, localKey)
	if err != nil {
		t.Fatalf("first file: %v", err)
	}
	if err := VerifyReceipt(localKey.Public().(ed25519.PublicKey), first); err != nil {
		t.Fatal(err)
	}
	again, err := File(root, snap, signed, payload, localKey)
	if err != nil {
		t.Fatalf("replay file: %v", err)
	}
	if again.Receipt().ID() != first.Receipt().ID() {
		t.Fatalf("receipt id changed")
	}
	got := inboxFiles(t, root, "grok")
	if len(got) != 1 {
		t.Fatalf("filings = %d, want 1", len(got))
	}
}

func TestACLRefusalWritesNoFactThenFilesOnce(t *testing.T) {
	root, snap, localKey, signed, payload := setupRecv(t, "m-acl")
	denied := snap
	denied.AllowIn = []Handle{mustHandle(t, "other")}
	_, err := File(root, denied, signed, payload, localKey)
	if err == nil {
		t.Fatal("expected ACL refusal")
	}
	if _, err := os.Stat(ledgerPath(root, signed.ID())); !os.IsNotExist(err) {
		t.Fatalf("ACL refusal wrote a ledger file: %v", err)
	}
	if got := inboxFiles(t, root, "grok"); len(got) != 0 {
		t.Fatalf("ACL refusal filed mail: %v", got)
	}
	if _, err := File(root, snap, signed, payload, localKey); err != nil {
		t.Fatalf("file after ACL fix: %v", err)
	}
	if got := inboxFiles(t, root, "grok"); len(got) != 1 {
		t.Fatalf("filings after ACL fix = %d, want 1", len(got))
	}
}

func TestRecoverIntentWithStagePublishes(t *testing.T) {
	root, snap, _, signed, payload := setupRecv(t, "m-intent")
	rec := signed.Record()
	sealed, err := Seal(rec, payload)
	if err != nil {
		t.Fatal(err)
	}
	rxDir := filepath.Join(root, "bridge", "rx", snap.Peer.String())
	if err := os.MkdirAll(rxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := stampFrom(sealed.Payload(), snap.Peer, rec.Handle())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagePath(root, snap.Peer, rec.ID()), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendFact(root, Fact{State: StateIntent, Peer: snap.Peer, ID: rec.ID()}); err != nil {
		t.Fatal(err)
	}
	state, err := Recover(root, snap, rec.ID())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if state != StatePublished {
		t.Fatalf("state = %q, want published", state)
	}
	if got := inboxFiles(t, root, "grok"); len(got) != 1 {
		t.Fatalf("filings = %d, want 1", len(got))
	}
	if fileExists(stagePath(root, snap.Peer, rec.ID())) {
		t.Fatal("staged copy still present")
	}
}

func TestRecoverStageWithoutIntentDeletes(t *testing.T) {
	root, snap, _, signed, payload := setupRecv(t, "m-stage")
	rxDir := filepath.Join(root, "bridge", "rx", snap.Peer.String())
	if err := os.MkdirAll(rxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagePath(root, snap.Peer, signed.ID()), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := Recover(root, snap, signed.ID())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if state != "" {
		t.Fatalf("state = %q, want absent", state)
	}
	if fileExists(stagePath(root, snap.Peer, signed.ID())) {
		t.Fatal("staged copy should be gone")
	}
	if got := inboxFiles(t, root, "grok"); len(got) != 0 {
		t.Fatalf("unexpected filing: %v", got)
	}
}

func TestDurableSyncPrimitive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "one")
	if err := writeSyncedFile(path, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := syncDir(dir); err != nil {
		t.Fatal(err)
	}
}

func setupRecv(t *testing.T, messageID string) (string, Snapshot, ed25519.PrivateKey, Signed, []byte) {
	t.Helper()
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "grok"); err != nil {
		t.Fatal(err)
	}
	localSeed := bytes.Repeat([]byte{0x22}, 32)
	peerSeed := bytes.Repeat([]byte{0x33}, 32)
	localKey := ed25519.NewKeyFromSeed(localSeed)
	peerKey := ed25519.NewKeyFromSeed(peerSeed)
	local := mustHost(t, "box")
	peer := mustHost(t, "mac")
	allow := mustHandle(t, "grok")
	payload := testPayload(t, messageID, "codex", "grok")
	rec, err := NewRecord(peer, mustHandle(t, "codex"), mustDest(t, "box/grok"), mustMessage(t, messageID), mustThread(t, "p2p/codex__grok"), payload)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Sign(peerKey, rec)
	if err != nil {
		t.Fatal(err)
	}
	snap := Snapshot{Local: local, Peer: peer, PeerKey: peerKey.Public().(ed25519.PublicKey), AllowIn: []Handle{allow}}
	return root, snap, localKey, signed, payload
}

func testPayload(t *testing.T, id, from, to string) []byte {
	t.Helper()
	raw, err := (format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      id,
			From:    from,
			To:      []string{to},
			Thread:  "p2p/codex__grok",
			Created: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
		},
		Body: "hello over p2p",
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func inboxFiles(t *testing.T, root, agent string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "agents", agent, "inbox", "new"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

func mustHandle(t *testing.T, name string) Handle {
	t.Helper()
	handle, err := ParseHandle(name)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func mustDest(t *testing.T, alias string) Dest {
	t.Helper()
	dest, err := ParseDest(alias)
	if err != nil {
		t.Fatal(err)
	}
	return dest
}

func mustMessage(t *testing.T, id string) MessageID {
	t.Helper()
	got, err := ParseMessageID(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustThread(t *testing.T, id string) ThreadID {
	t.Helper()
	got, err := ParseThreadID(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
