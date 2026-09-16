package amqio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Regression for agent-message-queue-611.22.42: a non-ENOENT Lstat error on
// the claim source (after a rename collision — EACCES, EIO, ESTALE) used to
// be wrapped as a generic error, so importOne returned (false, err) and the
// message stayed in new, re-claimed every tick forever. Such a claim is not
// retryable: the next tick re-claims the same bytes and fails the same way.
// The fix classifies it as PermanentClaimError and DLQs the message (with a
// receipt, same as a poison route). A clean loss (ENOENT) and a recoverable
// collision (ClaimCollisionError) are NOT permanent and stay on their paths.

// Direct claim-level test: claimRename with a non-ENOENT Lstat returns
// *PermanentClaimError; a clean ENOENT does not.
func TestB42ClaimRenameClassifiesNonEnoentLstatAsPermanent(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	droot, _ := fsq.OpenDeliveryRoot(root, identity)
	t.Cleanup(func() { _ = droot.Close() })

	now := time.Now()
	id, _ := format.NewMessageID(now)
	name := id + ".md"
	// Seed a collision target in cur so renameNoReplace returns ErrExist.
	if _, err := fsq.DeliverToInboxes(droot, []string{"alice"}, name, []byte("seed")); err != nil {
		t.Fatal(err)
	}
	if err := fsq.MoveNewToCur(droot, "alice", name); err != nil {
		t.Fatal(err)
	}
	// Re-deliver the real source into new (same filename) to collide.
	if _, err := fsq.DeliverToInboxes(droot, []string{"alice"}, name, []byte("real")); err != nil {
		t.Fatal(err)
	}

	// Fault the Lstat to return a non-ENOENT error → PermanentClaimError.
	droot.SetLstatFaultForTest(func(path string) error {
		return errors.New("permission denied (simulated)")
	})
	err := fsq.MoveNewToCur(droot, "alice", name)
	var permanent *fsq.PermanentClaimError
	if !errors.As(err, &permanent) {
		t.Fatalf("claim with non-ENOENT Lstat: err = %v, want *PermanentClaimError (611.22.42 — must DLQ, not loop in new)", err)
	}
}

// Clean ENOENT (a real loss) is NOT permanent-claim: it returns os.ErrNotExist.
func TestB42ClaimRenameEnoentIsNotPermanent(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	droot, _ := fsq.OpenDeliveryRoot(root, identity)
	t.Cleanup(func() { _ = droot.Close() })

	now := time.Now()
	id, _ := format.NewMessageID(now)
	name := id + ".md"
	if _, err := fsq.DeliverToInboxes(droot, []string{"alice"}, name, []byte("seed")); err != nil {
		t.Fatal(err)
	}
	if err := fsq.MoveNewToCur(droot, "alice", name); err != nil {
		t.Fatal(err)
	}
	if _, err := fsq.DeliverToInboxes(droot, []string{"alice"}, name, []byte("real")); err != nil {
		t.Fatal(err)
	}
	droot.SetLstatFaultForTest(func(path string) error {
		return os.ErrNotExist
	})
	err := fsq.MoveNewToCur(droot, "alice", name)
	var permanent *fsq.PermanentClaimError
	if errors.As(err, &permanent) {
		t.Fatalf("clean ENOENT loss classified as PermanentClaimError: %v (611.22.42 — ENOENT is a clean loss, not poison)", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ENOENT claim: err = %v, want os.ErrNotExist", err)
	}
}

// Full carrier path: a permanent claim failure DLQs the message + emits a
// receipt, the message is gone from new (no infinite loop), and it lands in
// the DLQ. Driven through the real ImportOnce path with the carrier's own
// root faulted via SetLstatFaultForTest.
func TestB42PermanentClaimFailureDLQsAndEmitsReceipt(t *testing.T) {
	endpointRoot := t.TempDir()
	if err := fsq.EnsureRootDirs(endpointRoot); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(endpointRoot, DefaultHandle); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(endpointRoot, "codex"); err != nil {
		t.Fatal(err)
	}
	store, err := requests.Open(filepath.Join(endpointRoot, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var carrier *Carrier
	ep := core.New(core.Config{
		Store: store,
		Publish: func(s protocol.Snapshot, origin map[string]string) error {
			return carrier.Publish(s, origin)
		},
	})
	carrier, err = New(endpointRoot, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	rt := fake.New("fake", "e_1")
	ep.Register(rt)
	t.Cleanup(func() { _ = ep.Close() })

	now := time.Now()
	id, _ := format.NewMessageID(now)
	name := id + ".md"
	// Seed a collision target in cur so the claim's renameNoReplace fails
	// with ErrExist and the Lstat-of-source path is reached.
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, name, []byte("seed")); err != nil {
		t.Fatal(err)
	}
	if err := fsq.MoveNewToCur(droot, DefaultHandle, name); err != nil {
		t.Fatal(err)
	}
	_ = droot.Close()

	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"11111111-1111-4111-8111-11111111142f","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"hi"}}`
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: body}
	data, _ := msg.Marshal()
	droot2, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot2, []string{DefaultHandle}, name, data); err != nil {
		t.Fatal(err)
	}
	_ = droot2.Close()

	// Install the Lstat fault on every root the carrier opens (ImportOnce
	// opens its own). Fire only on the claim-source Lstat (the new/<name>
	// path), returning a non-ENOENT error.
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) == "new" {
			return errors.New("permission denied")
		}
		return nil
	})

	n, err := carrier.ImportOnce()
	if err != nil {
		t.Fatalf("ImportOnce: %v", err)
	}
	if n < 1 {
		t.Fatal("ImportOnce consumed nothing — permanent claim failure must DLQ and count as consumed")
	}

	// DLQ receipt must exist.
	receiptPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "receipts", id+"__"+DefaultHandle+"__"+receipt.StageDLQ+".json")
	if _, err := os.Stat(receiptPath); err != nil {
		t.Fatalf("DLQ receipt not found at %s (611.22.42 — every DLQ site must emit a receipt): %v", receiptPath, err)
	}
	rc, rerr := receipt.Read(receiptPath)
	if rerr != nil {
		t.Fatalf("read DLQ receipt: %v", rerr)
	}
	if rc.Stage != receipt.StageDLQ {
		t.Fatalf("DLQ receipt stage=%q, want %q (611.22.42)", rc.Stage, receipt.StageDLQ)
	}
	// Message must be gone from new (no infinite loop).
	newPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "new", name)
	if _, err := os.Stat(newPath); err == nil {
		t.Fatal("message still in new after permanent claim failure (611.22.42 — must be DLQ'd, not retried forever)")
	}
	// And it must be in the DLQ (the envelope gets a DLQ id, so list the dir).
	dlqDir := filepath.Join(endpointRoot, "agents", DefaultHandle, "dlq")
	entries, err := os.ReadDir(dlqDir)
	if err != nil {
		t.Fatalf("read DLQ dir %s: %v (611.22.42)", dlqDir, err)
	}
	if len(entries) == 0 {
		t.Fatalf("DLQ dir %s is empty (611.22.42 — the permanent-claim message must be enveloped here)", dlqDir)
	}
}
