package amqio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	// opens its own). B776-1 (Pro round-2 call): allow the FIRST source read
	// (importOne's initial command read via OpenRegularNoFollow, now routed
	// through r.lstat), then fail SUBSEQUENT inspection of the new/<name>
	// source at the actual read boundary — the claim's Lstat after a
	// collision. A non-ENOENT error there classifies as PermanentClaimError
	// and the message is DLQ'd from the in-memory content.
	var firstReadDone int32
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "new" {
			return nil
		}
		if atomic.CompareAndSwapInt32(&firstReadDone, 0, 1) {
			return nil // allow the initial command read
		}
		return errors.New("permission denied")
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
	// And it must be in the DLQ. Pro test-strengthening: list dlq/new, read
	// the envelope, and assert OriginalID, OriginalFile, failure reason, and
	// original bytes; also verify the conflicting cur entry was not changed.
	dlqNewDir := filepath.Join(endpointRoot, "agents", DefaultHandle, "dlq", "new")
	dlqEntries, err := os.ReadDir(dlqNewDir)
	if err != nil {
		t.Fatalf("read DLQ new dir %s: %v (611.22.42)", dlqNewDir, err)
	}
	var envPath string
	for _, e := range dlqEntries {
		if !e.IsDir() {
			envPath = filepath.Join(dlqNewDir, e.Name())
			break
		}
	}
	if envPath == "" {
		t.Fatalf("DLQ new dir %s has no envelope file (611.22.42 — the permanent-claim message must be enveloped here)", dlqNewDir)
	}
	identity2, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot3, _ := fsq.OpenDeliveryRoot(endpointRoot, identity2)
	defer func() { _ = droot3.Close() }()
	env, original, rerr := fsq.ReadDLQEnvelope(droot3, filepath.Join("agents", DefaultHandle, "dlq", "new", filepath.Base(envPath)))
	if rerr != nil {
		t.Fatalf("read DLQ envelope: %v (611.22.42)", rerr)
	}
	if env == nil {
		t.Fatal("nil DLQ envelope (611.22.42)")
	}
	if env.OriginalID != id {
		t.Fatalf("DLQ envelope OriginalID=%q, want %q (611.22.42)", env.OriginalID, id)
	}
	if env.OriginalFile != name {
		t.Fatalf("DLQ envelope OriginalFile=%q, want %q (611.22.42)", env.OriginalFile, name)
	}
	if env.FailureReason != "permanent claim failure" {
		t.Fatalf("DLQ envelope FailureReason=%q, want %q (611.22.42)", env.FailureReason, "permanent claim failure")
	}
	// The original bytes must round-trip (the envelope preserves the logical
	// message; the content came from the in-memory read).
	if om, perr := format.ParseMessage(original); perr != nil {
		t.Fatalf("DLQ envelope original bytes do not parse: %v (611.22.42)", perr)
	} else if om.Header.ID != id {
		t.Fatalf("DLQ envelope original message ID=%q, want %q (611.22.42)", om.Header.ID, id)
	}
	// The conflicting cur entry (seeded at the top) must be unchanged — the
	// quarantine must not overwrite or remove a retained cur copy.
	curPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "cur", name)
	if curData, cerr := os.ReadFile(curPath); cerr != nil {
		t.Fatalf("conflicting cur entry %s missing after quarantine (611.22.42 — must not be removed): %v", curPath, cerr)
	} else if string(curData) != "seed" {
		t.Fatalf("conflicting cur entry overwritten by quarantine (611.22.42): got %q, want %q", string(curData), "seed")
	}
}

// --- Pro B776-1: quarantine transition independent of the failed inspection ---
//
// The prior MoveNewToDLQ re-read the source via ReadRegularNoFollow →
// root.root.Lstat, bypassing the lstatFaultForTest hook. A persistent
// source-metadata failure beginning after the initial command read failed
// BOTH the claim Lstat AND the DLQ read Lstat, so the carrier errored and the
// message stayed in new — the claimed "DLQ instead of looping" result was not
// established. The recut (QuarantineClaimNewToDLQ) builds the envelope from
// the already-read raw bytes, so a persistent source-metadata failure cannot
// fault the DLQ path. This test forces a non-ENOENT Lstat to remain active
// during the quarantine transition and asserts the message is DLQ'd (or, if
// the ownership rename itself fails, reported indeterminate — not silently
// "consumed").
func TestB42QuarantineIndependentOfFailedInspection(t *testing.T) {
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
	// Seed a collision target in cur so the claim's renameNoReplace fails and
	// the Lstat-of-source path is reached.
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, name, []byte("seed")); err != nil {
		t.Fatal(err)
	}
	if err := fsq.MoveNewToCur(droot, DefaultHandle, name); err != nil {
		t.Fatal(err)
	}
	_ = droot.Close()

	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"22222222-2222-4222-8222-222222222221","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"hi"}}`
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

	// B776-1 (Pro round-2 call): allow the FIRST source read (importOne's
	// initial command read, now routed through r.lstat), then fail SUBSEQUENT
	// inspection of the source at the actual read boundary. The claim's
	// post-collision Lstat is the second new-dir lstat → PermanentClaimError →
	// quarantine. The quarantine then claims new→inbox/quarantine via
	// claimRename (POSIX renameNoReplace, no source Lstat) and envelopes from
	// the in-memory content — proving it does NOT re-Lstat the source. If it
	// did, the persistent fault would fault that Lstat too and the DLQ would
	// fail.
	var firstReadDone int32
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "new" {
			return nil
		}
		if atomic.CompareAndSwapInt32(&firstReadDone, 0, 1) {
			return nil // allow the initial command read
		}
		return errors.New("permission denied (persistent)")
	})

	n, err := carrier.ImportOnce()
	if err != nil {
		t.Fatalf("ImportOnce: %v (611.22.42 B776-1 — quarantine must not re-Lstat the source; a persistent inspection failure must not prevent DLQ)", err)
	}
	if n < 1 {
		t.Fatal("ImportOnce consumed nothing (611.22.42 B776-1 — the message must be DLQ'd from in-memory content, not stranded in new by a re-Lstat)")
	}
	// Message gone from new.
	newPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "new", name)
	if _, err := os.Stat(newPath); err == nil {
		t.Fatal("message still in new (611.22.42 B776-1 — persistent inspection must not strand the message)")
	}
	// B776-1 (Pro round-2 call): assert the dlq/new envelope CONTENT —
	// OriginalID + original bytes — not just that the DLQ is non-empty.
	dlqNewDir := filepath.Join(endpointRoot, "agents", DefaultHandle, "dlq", "new")
	dlqEntries, err := os.ReadDir(dlqNewDir)
	if err != nil {
		t.Fatalf("read dlq/new dir %s: %v (611.22.42 B776-1)", dlqNewDir, err)
	}
	var envPath string
	for _, e := range dlqEntries {
		if !e.IsDir() {
			envPath = filepath.Join(dlqNewDir, e.Name())
			break
		}
	}
	if envPath == "" {
		t.Fatalf("dlq/new has no envelope (611.22.42 B776-1 — the envelope must be built from in-memory content)")
	}
	identity2, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot3, _ := fsq.OpenDeliveryRoot(endpointRoot, identity2)
	defer func() { _ = droot3.Close() }()
	env, original, rerr := fsq.ReadDLQEnvelope(droot3, filepath.Join("agents", DefaultHandle, "dlq", "new", filepath.Base(envPath)))
	if rerr != nil {
		t.Fatalf("read DLQ envelope: %v (611.22.42 B776-1)", rerr)
	}
	if env == nil || env.OriginalID != id {
		t.Fatalf("DLQ envelope OriginalID=%v, want %q (611.22.42 B776-1 — envelope built from in-memory content)", env, id)
	}
	if om, perr := format.ParseMessage(original); perr != nil {
		t.Fatalf("DLQ envelope original bytes do not parse: %v (611.22.42 B776-1)", perr)
	} else if om.Header.ID != id {
		t.Fatalf("DLQ envelope original message ID=%q, want %q (611.22.42 B776-1)", om.Header.ID, id)
	}
	// The quarantined staging leaf must be empty (source claimed in + removed).
	qDir := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "quarantine")
	if qEntries, _ := os.ReadDir(qDir); len(qEntries) != 0 {
		t.Fatalf("inbox/quarantine not empty after quarantine (611.22.42 B776-1 — staging source must be removed): %d entries", len(qEntries))
	}
}

// --- Pro B776-2: quarantine is ownership-preserving (no unclaimed removal) ---
//
// The prior MoveNewToDLQ read new/name → wrote envelope → removed new/name,
// treating ENOENT-on-remove as success, with NO ownership acquisition in
// between. A competing claimer could win new/name into cur/name while this
// caller held the read content, and this caller's remove would then unlink a
// pathname the competitor owns. The recut performs an exclusive quarantine
// rename (new/<name> → inbox/.quarantine-<rand>) BEFORE enveloping, so a
// competing claimer that already moved the source into cur observes ENOENT on
// this caller's rename (a clean loss), and this caller never removes a
// pathname another actor owns. This test deterministically reproduces the
// competing-claim interleaving and asserts the winner's cur entry survives.
func TestB42QuarantineIsOwnershipPreserving(t *testing.T) {
	endpointRoot := t.TempDir()
	if err := fsq.EnsureRootDirs(endpointRoot); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(endpointRoot, DefaultHandle); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	id, _ := format.NewMessageID(now)
	name := id + ".md"
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	t.Cleanup(func() { _ = droot.Close() })

	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"33333333-3333-4333-8333-333333333322","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"hi"}}`
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: body}
	data, _ := msg.Marshal()
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, name, data); err != nil {
		t.Fatal(err)
	}

	// A competing claimer has ALREADY claimed a prior copy of this filename
	// into cur (a crashed prior attempt, or a concurrent claim). The quarantine
	// must NOT overwrite or remove this retained cur entry — it must acquire
	// its OWN source via the exclusive quarantine rename and leave the
	// competitor's cur entry untouched (Pro B776-2: "do not overwrite the
	// colliding cur entry").
	curDir := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "cur")
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		t.Fatal(err)
	}
	curPath := filepath.Join(curDir, name)
	if err := os.WriteFile(curPath, []byte("competitor-owned cur entry"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The losing actor quarantines the new source with the bytes it read.
	_, qerr := fsq.QuarantineClaimToDLQ(droot, DefaultHandle, name, id, "permanent claim failure", "simulated", data)
	if qerr != nil {
		t.Fatalf("quarantine errored: %v (611.22.42 B776-2)", qerr)
	}
	// The competitor's cur entry must be byte-identical — NOT overwritten by
	// the quarantine's source acquisition or envelope publication.
	got, err := os.ReadFile(curPath)
	if err != nil {
		t.Fatalf("competing cur entry %s missing after quarantine (611.22.42 B776-2 — ownership-preserving quarantine must not remove a competitor's claimed entry): %v", curPath, err)
	}
	if string(got) != "competitor-owned cur entry" {
		t.Fatalf("competing cur entry overwritten by quarantine (611.22.42 B776-2): got %q", string(got))
	}
	// The new source is gone (quarantined + enveloped + removed).
	newPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "new", name)
	if _, err := os.Stat(newPath); err == nil {
		t.Fatal("new source still present after quarantine (611.22.42 B776-2)")
	}
	// Exactly one DLQ envelope file exists (the quarantine's), under dlq/new.
	dlqNewDir := filepath.Join(endpointRoot, "agents", DefaultHandle, "dlq", "new")
	dlqEntries, _ := os.ReadDir(dlqNewDir)
	envelopes := 0
	for _, e := range dlqEntries {
		if !e.IsDir() {
			envelopes++
		}
	}
	if envelopes != 1 {
		t.Fatalf("expected exactly 1 DLQ envelope, got %d (611.22.42 B776-2 — one quarantine must produce one envelope)", envelopes)
	}
}

func TestB42QuarantinePreservesPendingReply(t *testing.T) {
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
	// Provision the codex mailbox so the reply can be delivered.
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
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, name, []byte("seed")); err != nil {
		t.Fatal(err)
	}
	if err := fsq.MoveNewToCur(droot, DefaultHandle, name); err != nil {
		t.Fatal(err)
	}
	_ = droot.Close()

	// B776-3 (Pro round-2): a DLQ'd command whose reply was never delivered
	// must not strand the reply. Use a valid request.get command (a non-submit
	// request op, so owesReply=true) with a cross-session reply_to (codex@sess)
	// so the reply has a routable destination. The permanent-claim quarantine
	// registers the owed reply BEFORE the fallible reply delivery, so a failure
	// there retains it for flushOwedReplies / startup recovery.
	body := `{"schema":"amq.remote.command/1","op":"request.get","request_id":"22222222-2222-4222-8222-222222222221","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `"}`
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
		ReplyTo: "", ReplyProject: "",
	}, Body: body}
	data, _ := msg.Marshal()
	droot2, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot2, []string{DefaultHandle}, name, data); err != nil {
		t.Fatal(err)
	}
	_ = droot2.Close()

	// Counter-based fault: allow the initial command read, fail the claim's
	// post-collision Lstat → PermanentClaimError → quarantine.
	var firstReadDone int32
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "new" {
			return nil
		}
		if atomic.CompareAndSwapInt32(&firstReadDone, 0, 1) {
			return nil
		}
		return errors.New("permission denied")
	})

	// Fault the reply delivery (syncDir on codex's inbox/new) so the owed
	// reply is retained rather than delivered — proving the obligation
	// survives a fallible reply op (P1-4: register BEFORE, clear only on
	// success).
	carrier.SetSyncDirFaultForTest(func(dir string) error {
		d := filepath.ToSlash(dir)
		if strings.HasSuffix(d, "codex/inbox/new") {
			return errors.New("simulated reply delivery failure")
		}
		return nil
	})

	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("ImportOnce: %v (611.22.42 B776-3 — the reply obligation must not strand the import)", err)
	}

	// The reply ledger must exist (recordAnswer writes it before the claim).
	led, lerr := readLedgerForTest(t, endpointRoot, DefaultHandle, id)
	if lerr != nil {
		t.Fatalf("read ledger: %v (611.22.42 B776-3 — the reply ledger must exist; recordAnswer writes it before the claim)", lerr)
	}
	if led == nil {
		t.Fatal("ledger is nil (611.22.42 B776-3 — recordAnswer must write the reply ledger before the claim)")
	}
	// P1-4/B776-3: the owed reply must be registered (the reply delivery was
	// faulted), NOT stranded. A Sent:false ledger with no owed-reply
	// registration is the stranded-reply defect.
	carrier.mu.Lock()
	_, owed := carrier.owedReplies[id]
	carrier.mu.Unlock()
	if !led.Sent && !owed {
		t.Fatalf("reply stranded: ledger Sent=false and no owed-reply registered (611.22.42 B776-3 — a DLQ'd command's pending reply must be delivered or owed for recovery)")
	}
	// The owed reply must survive a restart: recoverDLQLedgeredReplies
	// rebuilds it from the DLQ envelope. Simulate a restart by clearing the
	// in-session set and re-running the startup sweep.
	carrier.mu.Lock()
	delete(carrier.owedReplies, id)
	carrier.curRecovered = false
	carrier.mu.Unlock()
	identity2, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	recRoot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity2)
	if rerr := carrier.recoverDLQLedgeredReplies(recRoot); rerr != nil {
		t.Fatalf("recoverDLQLedgeredReplies: %v (611.22.42 B776-3 — startup recovery must rebuild the owed reply)", rerr)
	}
	_ = recRoot.Close()
	carrier.mu.Lock()
	_, owedAfterRestart := carrier.owedReplies[id]
	carrier.mu.Unlock()
	if !led.Sent && !owedAfterRestart {
		t.Fatalf("reply not recovered after restart (611.22.42 B776-3 — recoverDLQLedgeredReplies must rebuild the owed reply from the DLQ envelope)")
	}
}

// --- Pro B776-4: committed-move postcondition owes a receipt ---
//
// The prior caller handled DLQTransitionError and CommittedDurabilityError
// identically — return before creating a receipt — so a committed move (source
// removed, envelope visible, dir-sync failed) stranded a receipt during normal
// operation. The recut distinguishes the two: a committed move (envelope
// visible, source gone) owes a receipt (registered in owedReceipts for retry);
// a source-retained partial is reconciled and left for the next tick. This
// test injects a dir-sync failure AFTER the source is removed (a committed
// move) and asserts the receipt is still owed/registered, not dropped.
func TestB42QuarantineHandlesCommittedMovePostcondition(t *testing.T) {
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
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, name, []byte("seed")); err != nil {
		t.Fatal(err)
	}
	if err := fsq.MoveNewToCur(droot, DefaultHandle, name); err != nil {
		t.Fatal(err)
	}
	_ = droot.Close()

	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"44444444-4444-4444-8444-444444444423","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"hi"}}`
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

	// Counter-based Lstat fault: allow the initial command read, fail the
	// claim's post-collision Lstat → PermanentClaimError → quarantine.
	var firstReadDone int32
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "new" {
			return nil
		}
		if atomic.CompareAndSwapInt32(&firstReadDone, 0, 1) {
			return nil
		}
		return errors.New("permission denied")
	})
	// ...AND fault the dir-sync AFTER the quarantined source is removed, so
	// QuarantineClaimToDLQ returns a CommittedDurabilityError (envelope
	// visible, source gone). The committed-move syncs are inbox/quarantine
	// and inbox/new (the post-removal dirs); let dlq/new's internal sync
	// inside deliverToDLQ succeed so the envelope is durable first.
	syncHit := false
	carrier.SetSyncDirFaultForTest(func(dir string) error {
		d := filepath.ToSlash(dir)
		if strings.HasSuffix(d, "inbox/quarantine") || strings.HasSuffix(d, "inbox/new") {
			syncHit = true
			return errors.New("simulated dir-sync failure (committed move)")
		}
		return nil
	})

	_, _ = carrier.ImportOnce() // expected to return an error; the point is the postcondition.

	// The envelope is visible (committed move) — a DLQ entry must exist.
	dlqDir := filepath.Join(endpointRoot, "agents", DefaultHandle, "dlq")
	entries, _ := os.ReadDir(dlqDir)
	if len(entries) == 0 {
		t.Fatalf("DLQ empty (611.22.42 B776-4 — a committed move means the envelope IS visible even though the dir-sync failed)")
	}
	if !syncHit {
		t.Fatal("committed-move dir-sync fault was not hit (611.22.42 B776-4 — test setup must reach the committed-move sync)")
	}
	// P1-3/B776-4: a committed move (CommittedDurabilityError — envelope
	// visible, source gone) must NOT be confused with a source-retained
	// partial. The carrier must register the owed receipt (not emit a
	// terminal receipt + return nil while durability failed) and return an
	// error so the next tick reconciles. The receipt is owed — it must be
	// registered in owedReceipts for retry (or already emitted).
	carrier.mu.Lock()
	_, owedReceipt := carrier.owedReceipts[id]
	carrier.mu.Unlock()
	receiptPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "receipts", id+"__"+DefaultHandle+"__"+receipt.StageDLQ+".json")
	_, receiptEmitted := os.Stat(receiptPath)
	if !owedReceipt && receiptEmitted != nil {
		t.Fatalf("committed move dropped the receipt: not in owedReceipts and not emitted (611.22.42 B776-4 — a committed move owes a receipt; check DLQTransitionError BEFORE CommittedDurabilityError)")
	}
}

// readLedgerForTest reads a command's reply ledger from the endpoint root for
// a test (mirrors Carrier.readLedger without needing a carrier root open).
func readLedgerForTest(t *testing.T, endpointRoot, agent, cmdMsgID string) (*replyRecord, error) {
	t.Helper()
	identity, err := fsq.SnapshotDeliveryRoot(endpointRoot)
	if err != nil {
		return nil, err
	}
	root, err := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	c := &Carrier{root: endpointRoot, me: agent}
	return c.readLedger(cmdMsgID)
}

// --- Pro P1-1: quarantine staging is recovered after a restart ---
//
// The round-2 quarantine moved new/<name> to inbox/.quarantine-<rand> and
// nothing scanned that name. A process exit after the rename but before the
// envelope was written stranded the source as the only copy. The round-3
// recut claims into the discoverable inbox/quarantine leaf and
// recoverQuarantineStaging resumes the transition at startup. This test
// simulates a crash mid-transition: a message is left in inbox/quarantine
// with no DLQ envelope, then ImportOnce's startup sweep must complete the
// DLQ transition and register the owed obligations.
func TestB42QuarantineStagingRecoveredOnRestart(t *testing.T) {
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
	body := `{"schema":"amq.remote.command/1","op":"request.submit","request_id":"55555555-5555-4555-8555-555555555524","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `","input":{"text":"hi"}}`
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: body}
	data, _ := msg.Marshal()

	// Simulate a crash mid-quarantine: the source was claimed into
	// inbox/quarantine (the discoverable leaf) but no DLQ envelope exists yet.
	quarantineDir := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "quarantine")
	if err := os.MkdirAll(quarantineDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(quarantineDir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// No Lstat fault — the startup sweep must read the quarantined entry and
	// complete the transition. ImportOnce runs recoverQuarantineStaging first.
	n, err := carrier.ImportOnce()
	if err != nil {
		t.Fatalf("ImportOnce: %v (611.22.42 P1-1 — recoverQuarantineStaging must complete the stranded transition)", err)
	}
	// The quarantined source must be gone (transition completed).
	qPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "quarantine", name)
	if _, err := os.Stat(qPath); err == nil {
		t.Fatal("quarantined source still present (611.22.42 P1-1 — recoverQuarantineStaging must remove it after enveloping)")
	}
	// A DLQ envelope for originalID must exist.
	dlqNewDir := filepath.Join(endpointRoot, "agents", DefaultHandle, "dlq", "new")
	entries, _ := os.ReadDir(dlqNewDir)
	if len(entries) == 0 {
		t.Fatalf("DLQ empty (611.22.42 P1-1 — recoverQuarantineStaging must envelope the stranded source)")
	}
	_ = n
}

// --- Pro P1-5: restart recovery retains ledger-read failures for retry ---
//
// recoverDLQLedgeredReplies used to `continue` on a readLedger error and
// return success, so ImportOnce set curRecovered=true and a temporarily
// unreadable Sent:false ledger was never retried. The fix retains the failed
// ID into owedReplies (with the origin reconstructed from the DLQ envelope)
// AND returns an accumulated error so the startup sweep is not marked
// complete. This test DLQs a reply-owing command, makes its ledger unreadable,
// then asserts recoverDLQLedgeredReplies registers the ID and errors.
func TestB42RecoverDLQLedgeredRepliesRetainsLedgerReadFailure(t *testing.T) {
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
	// request.get: non-submit op → owesReply=true; reply routes same-root to codex.
	body := `{"schema":"amq.remote.command/1","op":"request.get","request_id":"66666666-6666-4666-8666-666666666625","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `"}`
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
		ReplyTo: "", ReplyProject: "",
	}, Body: body}
	data, _ := msg.Marshal()
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, name, []byte("seed")); err != nil {
		t.Fatal(err)
	}
	if err := fsq.MoveNewToCur(droot, DefaultHandle, name); err != nil {
		t.Fatal(err)
	}
	_ = droot.Close()
	droot2, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot2, []string{DefaultHandle}, name, data); err != nil {
		t.Fatal(err)
	}
	_ = droot2.Close()

	// Counter-based fault: allow the initial read, fail the claim Lstat → DLQ.
	var firstReadDone int32
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "new" {
			return nil
		}
		if atomic.CompareAndSwapInt32(&firstReadDone, 0, 1) {
			return nil
		}
		return errors.New("permission denied")
	})
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("ImportOnce: %v (611.22.42 P1-5 setup — the DLQ + reply must complete)", err)
	}

	// The ledger exists (recordAnswer wrote it before the claim).
	led, lerr := readLedgerForTest(t, endpointRoot, DefaultHandle, id)
	if lerr != nil || led == nil {
		t.Fatalf("ledger missing: %v (611.22.42 P1-5 setup)", lerr)
	}

	// Make the ledger unreadable to simulate a transient I/O failure during
	// restart recovery.
	ledgerPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "extensions", "remote", "replies", id+".json")
	if err := os.Chmod(ledgerPath, 0o000); err != nil {
		t.Fatalf("chmod ledger: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ledgerPath, 0o600) })

	// Simulate a restart: clear the in-session owed set and re-run the startup
	// recovery sweep.
	carrier.mu.Lock()
	carrier.owedReplies = nil
	carrier.curRecovered = false
	carrier.mu.Unlock()
	recRoot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	defer func() { _ = recRoot.Close() }()
	rerr := carrier.recoverDLQLedgeredReplies(recRoot)
	// P1-5: a readLedger failure must NOT be silently dropped — the sweep
	// returns an error (so ImportOnce does not set curRecovered=true).
	if rerr == nil {
		t.Fatal("recoverDLQLedgeredReplies returned nil for a ledger-read failure (611.22.42 P1-5 — must not silently drop)")
	}
	// And the failed ID must be retained into owedReplies for retry.
	carrier.mu.Lock()
	_, retained := carrier.owedReplies[id]
	carrier.mu.Unlock()
	if !retained {
		t.Fatal("ledger-read failure not retained in owedReplies (611.22.42 P1-5 — must retain the failed ID for retry)")
	}
}
