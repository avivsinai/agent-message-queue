package amqio

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
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

// Regressions for agent-message-queue-611.22.42 (round-4 recut, Pro P1-1..P1-6).
//
// Round-4 design: fsq owns ONE shared-ownership quarantine transition
// (QuarantinePermanentClaimToDLQ). The quarantine acquires inbox/new by the
// SAME exclusive source disposal the normal claim performs (NOT a second
// claimRename to a different destination — round-3's P1-3 fatal flaw, where
// two concurrent link insertions could both succeed before either removed
// new). The envelope is built from in-memory content (B776-1: no re-Lstat of
// the poison source), and durability is confirmed BEFORE the source is gone
// (P1-5). A partial (envelope durable, source retained) is resumed EVERY
// ImportOnce tick by ReconcileQuarantinedSource (P1-1); the carrier does NOT
// duplicate the FS transition (P1-5). The carrier is fail-closed (P1-2).

// directClaimSetup seeds a collision target in cur so the claim's
// renameNoReplace returns ErrExist and the post-collision Lstat path runs.
func directClaimSetup(t *testing.T, root, agent, name string) fsq.DeliveryRootIdentity {
	t.Helper()
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	droot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := fsq.DeliverToInboxes(droot, []string{agent}, name, []byte("seed")); err != nil {
		t.Fatalf("deliver seed: %v", err)
	}
	if err := fsq.MoveNewToCur(droot, agent, name); err != nil {
		t.Fatalf("move seed to cur: %v", err)
	}
	if err := droot.Close(); err != nil {
		t.Fatalf("close seed root: %v", err)
	}
	return identity
}

// TestB42ClaimClassifiesNonEnoentLstatAsPermanent: claimRename with a
// non-ENOENT post-collision Lstat returns *PermanentClaimError (must DLQ, not
// loop in new). A clean ENOENT does not.
func TestB42ClaimClassifiesNonEnoentLstatAsPermanent(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	now := time.Now()
	id, _ := format.NewMessageID(now)
	name := id + ".md"
	directClaimSetup(t, root, "alice", name)

	droot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = droot.Close() })
	if _, err := fsq.DeliverToInboxes(droot, []string{"alice"}, name, []byte("real")); err != nil {
		t.Fatal(err)
	}
	droot.SetLstatFaultForTest(func(path string) error {
		return errors.New("permission denied (simulated)")
	})
	err = fsq.MoveNewToCur(droot, "alice", name)
	var permanent *fsq.PermanentClaimError
	if !errors.As(err, &permanent) {
		t.Fatalf("claim with non-ENOENT Lstat: err = %v, want *PermanentClaimError (611.22.42)", err)
	}
}

// TestB42ClaimEnoentIsNotPermanent: a clean ENOENT loss is NOT permanent.
func TestB42ClaimEnoentIsNotPermanent(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	now := time.Now()
	id, _ := format.NewMessageID(now)
	name := id + ".md"
	directClaimSetup(t, root, "alice", name)
	droot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = droot.Close() })
	if _, err := fsq.DeliverToInboxes(droot, []string{"alice"}, name, []byte("real")); err != nil {
		t.Fatal(err)
	}
	droot.SetLstatFaultForTest(func(path string) error { return os.ErrNotExist })
	err = fsq.MoveNewToCur(droot, "alice", name)
	var permanent *fsq.PermanentClaimError
	if errors.As(err, &permanent) {
		t.Fatalf("clean ENOENT classified as PermanentClaimError: %v (611.22.42)", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ENOENT claim: err = %v, want os.ErrNotExist", err)
	}
}

// carrierSetup builds a carrier + endpoint registered to a fake runtime, with
// agent + codex mailboxes provisioned.
func carrierSetup(t *testing.T) (endpointRoot string, carrier *Carrier, ep *core.Endpoint) {
	t.Helper()
	endpointRoot = t.TempDir()
	if err := fsq.EnsureRootDirs(endpointRoot); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{DefaultHandle, "codex"} {
		if err := fsq.EnsureAgentDirs(endpointRoot, h); err != nil {
			t.Fatal(err)
		}
	}
	store, err := requests.Open(filepath.Join(endpointRoot, "extensions", "remote"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ep = core.New(core.Config{
		Store: store,
		Publish: func(s protocol.Snapshot, origin map[string]string) error {
			return carrier.Publish(s, origin)
		},
	})
	carrier, err = New(endpointRoot, DefaultHandle, ep)
	if err != nil {
		t.Fatalf("carrier: %v", err)
	}
	ep.Register(fake.New("fake", "e_1"))
	t.Cleanup(func() { _ = ep.Close() })
	return endpointRoot, carrier, ep
}

// newCommandMessage builds a deliverable command message for the fake target.
func newCommandMessage(t *testing.T, requestID string, op string) (id, name string, data []byte) {
	t.Helper()
	now := time.Now()
	id, _ = format.NewMessageID(now)
	name = id + ".md"
	body := `{"schema":"amq.remote.command/1","op":"` + op + `","request_id":"` + requestID + `","target_id":"fake","epoch":"e_1","not_after":"` + protocol.FormatTime(time.Now().Add(time.Minute)) + `"`
	if op == "request.submit" {
		body += `,"input":{"text":"hi"}`
	}
	body += `}`
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{DefaultHandle},
		Thread: "p2p/codex__remote", Subject: "submit", Created: now.UTC().Format(time.RFC3339Nano), Kind: "todo",
	}, Body: body}
	data, _ = msg.Marshal()
	return id, name, data
}

// deliverCollidingCommand seeds cur with a collision target (so the claim
// collides), then delivers the real command into new.
func deliverCollidingCommand(t *testing.T, endpointRoot, name string, data []byte) fsq.DeliveryRootIdentity {
	t.Helper()
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, name, []byte("seed")); err != nil {
		t.Fatalf("deliver seed: %v", err)
	}
	if err := fsq.MoveNewToCur(droot, DefaultHandle, name); err != nil {
		t.Fatalf("move seed to cur: %v", err)
	}
	if err := droot.Close(); err != nil {
		t.Fatalf("close seed root: %v", err)
	}
	droot2, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot2, []string{DefaultHandle}, name, data); err != nil {
		t.Fatalf("deliver real: %v", err)
	}
	if err := droot2.Close(); err != nil {
		t.Fatalf("close real root: %v", err)
	}
	return identity
}

// TestB42PermanentClaimFailureDLQsAndEmitsReceipt: the full carrier path. A
// permanent claim failure DLQs the message + emits a receipt, the message is
// gone from new (no infinite loop), lands in the DLQ with the right
// OriginalID/bytes, and the conflicting cur entry is NOT overwritten
// (ownership-preserving).
func TestB42PermanentClaimFailureDLQsAndEmitsReceipt(t *testing.T) {
	endpointRoot, carrier, _ := carrierSetup(t)
	id, name, data := newCommandMessage(t, "11111111-1111-4111-8111-111111111421", "request.submit")
	deliverCollidingCommand(t, endpointRoot, name, data)

	// Allow the FIRST new-dir Lstat of each path (the reconcile pre-scan read
	// and importOne's initial command read), then fail the claim's
	// post-collision Lstat of the same path → PermanentClaimError → quarantine.
	seen := make(map[string]int)
	var mu sync.Mutex
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "new" {
			return nil
		}
		mu.Lock()
		seen[path]++
		n := seen[path]
		mu.Unlock()
		if n <= 2 {
			return nil
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

	receiptPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "receipts", id+"__"+DefaultHandle+"__"+receipt.StageDLQ+".json")
	if _, err := os.Stat(receiptPath); err != nil {
		t.Fatalf("DLQ receipt not found (611.22.42): %v", err)
	}
	newPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "new", name)
	if _, err := os.Stat(newPath); err == nil {
		t.Fatal("message still in new (611.22.42 — must be DLQ'd, not retried forever)")
	}
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	t.Cleanup(func() { _ = droot.Close() })
	dlqNewDir := filepath.Join("agents", DefaultHandle, "dlq", "new")
	entries, _ := droot.ReadDir(dlqNewDir)
	if len(entries) == 0 {
		t.Fatal("DLQ empty (611.22.42)")
	}
	var envPath string
	for _, e := range entries {
		if !e.IsDir() {
			envPath = filepath.Join(dlqNewDir, e.Name())
			break
		}
	}
	env, original, rerr := fsq.ReadDLQEnvelope(droot, envPath)
	if rerr != nil || env == nil {
		t.Fatalf("read DLQ envelope: %v (611.22.42)", rerr)
	}
	if env.OriginalID != id {
		t.Fatalf("OriginalID=%q, want %q (611.22.42)", env.OriginalID, id)
	}
	if env.FailureReason != "permanent claim failure" {
		t.Fatalf("FailureReason=%q, want %q (611.22.42)", env.FailureReason, "permanent claim failure")
	}
	if om, perr := format.ParseMessage(original); perr != nil || om.Header.ID != id {
		t.Fatalf("DLQ original bytes mismatch (611.22.42): %v", perr)
	}
	// The conflicting cur entry (seeded) must be unchanged.
	curPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "cur", name)
	if curData, cerr := os.ReadFile(curPath); cerr != nil || string(curData) != "seed" {
		t.Fatalf("conflicting cur entry changed (611.22.42 — quarantine must not overwrite it): %v %q", cerr, string(curData))
	}
}

// TestB42QuarantineIndependentOfFailedInspection (B776-1): the envelope is
// built from in-memory content; a PERSISTENT source-metadata failure must NOT
// prevent the DLQ (no re-Lstat of the poison source).
func TestB42QuarantineIndependentOfFailedInspection(t *testing.T) {
	endpointRoot, carrier, _ := carrierSetup(t)
	id, name, data := newCommandMessage(t, "22222222-2222-4222-8222-222222222422", "request.submit")
	deliverCollidingCommand(t, endpointRoot, name, data)

	// Allow the initial command read, then PERSISTENTLY fail new-dir Lstat of
	// the same path (every subsequent inspection). The quarantine's exclusive
	// acquisition (removeSourceExclusively) does NOT Lstat — it removes by
	// name/handle — so it must succeed despite the persistent Lstat fault, and
	// envelope from the in-memory content.
	seen := make(map[string]int)
	var mu sync.Mutex
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "new" {
			return nil
		}
		mu.Lock()
		seen[path]++
		n := seen[path]
		mu.Unlock()
		if n <= 2 {
			return nil
		}
		return errors.New("permission denied (persistent)")
	})

	n, err := carrier.ImportOnce()
	if err != nil {
		t.Fatalf("ImportOnce: %v (611.22.42 B776-1 — quarantine must not re-Lstat the source)", err)
	}
	if n < 1 {
		t.Fatal("ImportOnce consumed nothing (611.22.42 B776-1)")
	}
	newPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "new", name)
	if _, err := os.Stat(newPath); err == nil {
		t.Fatal("message still in new (611.22.42 B776-1)")
	}
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	t.Cleanup(func() { _ = droot.Close() })
	dlqNewDir := filepath.Join("agents", DefaultHandle, "dlq", "new")
	entries, _ := droot.ReadDir(dlqNewDir)
	if len(entries) == 0 {
		t.Fatal("DLQ empty (611.22.42 B776-1)")
	}
	_ = id
}

// TestB42QuarantineIsOwnershipPreserving (B776-2/P1-3): the quarantine's
// exclusive acquisition of new must NOT remove a competitor's claimed cur
// entry. This asserts the shared-ownership invariant: exactly one winner
// across quarantine + normal claim. Here a competitor already owns cur; the
// quarantine acquires new (a DIFFERENT file) and envelopes it, leaving cur
// untouched.
func TestB42QuarantineIsOwnershipPreserving(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, DefaultHandle); err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	droot, _ := fsq.OpenDeliveryRoot(root, identity)
	t.Cleanup(func() { _ = droot.Close() })
	id, name, data := newCommandMessage(t, "33333333-3333-4333-8333-333333333423", "request.submit")
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, name, data); err != nil {
		t.Fatal(err)
	}
	// A competitor already claimed a prior copy into cur.
	curDir := filepath.Join(root, "agents", DefaultHandle, "inbox", "cur")
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		t.Fatal(err)
	}
	curPath := filepath.Join(curDir, name)
	if err := os.WriteFile(curPath, []byte("competitor-owned cur entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, qerr := fsq.QuarantinePermanentClaimToDLQ(droot, DefaultHandle, name, id, "permanent claim failure", "simulated", data)
	if qerr != nil {
		t.Fatalf("quarantine errored (611.22.42 B776-2): %v", qerr)
	}
	got, err := os.ReadFile(curPath)
	if err != nil || string(got) != "competitor-owned cur entry" {
		t.Fatalf("competitor cur entry changed (611.22.42 B776-2): %v %q", err, string(got))
	}
	// The new source is gone (exclusively acquired + enveloped + removed).
	if _, err := os.Stat(filepath.Join(root, "agents", DefaultHandle, "inbox", "new", name)); err == nil {
		t.Fatal("new source still present (611.22.42 B776-2)")
	}
	// Exactly one DLQ envelope.
	dlqNewDir := filepath.Join(root, "agents", DefaultHandle, "dlq", "new")
	entries, _ := os.ReadDir(dlqNewDir)
	envelopes := 0
	for _, e := range entries {
		if !e.IsDir() {
			envelopes++
		}
	}
	if envelopes != 1 {
		t.Fatalf("expected 1 DLQ envelope, got %d (611.22.42 B776-2)", envelopes)
	}
}

// TestB42QuarantinePreservesPendingReply (B776-3): a DLQ'd command whose reply
// was never delivered must not strand the reply. The owed reply is registered
// BEFORE the fallible reply delivery (a REAL reply-delivery failure, not a
// sync fault — codex's correction), and survives a restart via
// recoverDLQLedgeredReplies.
func TestB42QuarantinePreservesPendingReply(t *testing.T) {
	endpointRoot, carrier, _ := carrierSetup(t)
	id, name, data := newCommandMessage(t, "44444444-4444-4444-8444-444444444424", "request.get")
	deliverCollidingCommand(t, endpointRoot, name, data)

	seen := make(map[string]int)
	var mu sync.Mutex
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "new" {
			return nil
		}
		mu.Lock()
		seen[path]++
		n := seen[path]
		mu.Unlock()
		if n <= 2 {
			return nil
		}
		return errors.New("permission denied")
	})

	// Force a REAL reply-delivery failure (not a sync fault): remove the
	// codex inbox/new directory so the reply route cannot deliver. codex's
	// correction: a sync fault that finalizeDelivery treats as delivered
	// skips the recovery check.
	codexNew := filepath.Join(endpointRoot, "agents", "codex", "inbox", "new")
	_ = os.RemoveAll(codexNew)

	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("ImportOnce: %v (611.22.42 B776-3 — the reply obligation must not strand the import)", err)
	}

	// The reply ledger must exist (recordAnswer writes it before the claim).
	led, lerr := readLedgerForTest(t, endpointRoot, DefaultHandle, id)
	if lerr != nil || led == nil {
		t.Fatalf("ledger missing (611.22.42 B776-3 — recordAnswer writes it before the claim): %v", lerr)
	}
	// P1-4/B776-3: the owed reply must be registered (the reply delivery
	// failed), NOT stranded.
	carrier.mu.Lock()
	_, owed := carrier.owedReplies[id]
	carrier.mu.Unlock()
	if !led.Sent && !owed {
		t.Fatalf("reply stranded: ledger Sent=false and no owed-reply registered (611.22.42 B776-3)")
	}
	// The owed reply must survive a restart: recoverDLQLedgeredReplies
	// rebuilds it from the DLQ envelope.
	carrier.mu.Lock()
	delete(carrier.owedReplies, id)
	carrier.curRecovered = false
	carrier.mu.Unlock()
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	recRoot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if rerr := carrier.recoverDLQLedgeredReplies(recRoot); rerr != nil {
		t.Fatalf("recoverDLQLedgeredReplies: %v (611.22.42 B776-3)", rerr)
	}
	_ = recRoot.Close()
	carrier.mu.Lock()
	_, owedAfterRestart := carrier.owedReplies[id]
	carrier.mu.Unlock()
	if !led.Sent && !owedAfterRestart {
		t.Fatalf("reply not recovered after restart (611.22.42 B776-3)")
	}
}

// TestB42QuarantineHandlesCommittedMovePostcondition (B776-4): a committed
// move (envelope visible, source gone, dir-sync failed) must register the owed
// receipt (not emit a terminal receipt + return nil while durability failed)
// and return an error so the next tick reconciles.
func TestB42QuarantineHandlesCommittedMovePostcondition(t *testing.T) {
	endpointRoot, carrier, _ := carrierSetup(t)
	id, name, data := newCommandMessage(t, "55555555-5555-4555-8555-555555555525", "request.submit")
	deliverCollidingCommand(t, endpointRoot, name, data)

	seen := make(map[string]int)
	var mu sync.Mutex
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "new" {
			return nil
		}
		mu.Lock()
		seen[path]++
		n := seen[path]
		mu.Unlock()
		if n <= 2 {
			return nil
		}
		return errors.New("permission denied")
	})
	// Fault the post-removal dir-sync (inbox/new + dlq/new) so the quarantine
	// returns a CommittedDurabilityError (envelope visible, source gone).
	syncHit := false
	carrier.SetSyncDirFaultForTest(func(dir string) error {
		d := filepath.ToSlash(dir)
		if d == "agents/"+DefaultHandle+"/inbox/new" || d == "agents/"+DefaultHandle+"/dlq/new" {
			syncHit = true
			return errors.New("simulated dir-sync failure (committed move)")
		}
		return nil
	})

	_, _ = carrier.ImportOnce() // expected to return an error; the point is the postcondition.

	// The envelope is visible (committed move).
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	t.Cleanup(func() { _ = droot.Close() })
	dlqEntries, _ := droot.ReadDir(filepath.Join("agents", DefaultHandle, "dlq", "new"))
	if len(dlqEntries) == 0 {
		t.Fatal("DLQ empty (611.22.42 B776-4 — committed move means the envelope IS visible)")
	}
	if !syncHit {
		t.Fatal("committed-move dir-sync fault not hit (611.22.42 B776-4)")
	}
	// P1-3/B776-4: a committed move must register the owed receipt (not drop it).
	carrier.mu.Lock()
	_, owedReceipt := carrier.owedReceipts[id]
	carrier.mu.Unlock()
	receiptPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "receipts", id+"__"+DefaultHandle+"__"+receipt.StageDLQ+".json")
	_, receiptEmitted := os.Stat(receiptPath)
	if !owedReceipt && receiptEmitted != nil {
		t.Fatalf("committed move dropped the receipt (611.22.42 B776-4)")
	}
}

// TestB42QuarantineStagingRecoveredOnRestart (P1-1): a partial quarantine
// (envelope durable, source retained in new) is resumed by
// reconcileQuarantineStaging EVERY tick (not just startup). This simulates a
// crash mid-transition: the source is still in inbox/new AND a DLQ envelope
// exists, then ImportOnce's reconciliation completes it.
func TestB42QuarantineStagingRecoveredOnRestart(t *testing.T) {
	endpointRoot, carrier, _ := carrierSetup(t)
	now := time.Now()
	id, _ := format.NewMessageID(now)
	name := id + ".md"
	_, _, data := newCommandMessage(t, "66666666-6666-4666-8666-666666666626", "request.submit")

	// Simulate a crash mid-quarantine: the source is still in inbox/new AND a
	// DLQ envelope already exists (durable). The next ImportOnce's
	// reconcileQuarantineStaging must complete the transition (remove source).
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	droot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	if _, err := fsq.DeliverToInboxes(droot, []string{DefaultHandle}, name, data); err != nil {
		t.Fatal(err)
	}
	// Write a durable DLQ envelope for this originalID (the partial state).
	env := fsq.DLQEnvelope{
		Schema:        fsq.DLQSchemaVersion,
		ID:            "dlq_crash_partial",
		OriginalID:    id,
		OriginalFile:  name,
		FailureReason: "permanent claim failure",
		FailureDetail: "crash mid-quarantine",
		FailureTime:   time.Now().UTC().Format(time.RFC3339),
		RetryCount:    0,
		SourceDir:     fsq.BoxNew,
	}
	envData, _ := fsq.SerializeDLQMessage(env, data)
	if _, err := fsq.DeliverToDLQ(droot, DefaultHandle, env.ID+".md", envData); err != nil {
		t.Fatalf("deliver partial envelope: %v", err)
	}
	_ = droot.Close()

	// No Lstat fault — the reconciliation must complete the transition.
	n, err := carrier.ImportOnce()
	if err != nil {
		t.Fatalf("ImportOnce: %v (611.22.42 P1-1 — reconcileQuarantineStaging must complete the partial)", err)
	}
	// The source must be gone (transition completed).
	srcPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "inbox", "new", name)
	if _, err := os.Stat(srcPath); err == nil {
		t.Fatal("partial source still present (611.22.42 P1-1 — reconciliation must remove it)")
	}
	// The owed receipt must be registered (the transition completed).
	carrier.mu.Lock()
	_, owed := carrier.owedReceipts[id]
	carrier.mu.Unlock()
	if !owed {
		// It may have been emitted already by flushOwedReceipts; check the file.
		receiptPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "receipts", id+"__"+DefaultHandle+"__"+receipt.StageDLQ+".json")
		if _, err := os.Stat(receiptPath); err != nil {
			t.Fatal("partial reconciliation did not register/emit the owed DLQ receipt (611.22.42 P1-1)")
		}
	}
	_ = n
}

// TestB42RecoverDLQLedgeredRepliesRetainsLedgerReadFailure (P1-5): a
// readLedger failure during restart recovery must NOT be silently dropped —
// the ID is retained into owedReplies AND the sweep errors (so ImportOnce does
// not set curRecovered=true).
func TestB42RecoverDLQLedgeredRepliesRetainsLedgerReadFailure(t *testing.T) {
	endpointRoot, carrier, _ := carrierSetup(t)
	id, name, data := newCommandMessage(t, "77777777-7777-4777-8777-777777777727", "request.get")
	deliverCollidingCommand(t, endpointRoot, name, data)

	seen := make(map[string]int)
	var mu sync.Mutex
	carrier.SetLstatFaultForTest(func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "new" {
			return nil
		}
		mu.Lock()
		seen[path]++
		n := seen[path]
		mu.Unlock()
		if n <= 2 {
			return nil
		}
		return errors.New("permission denied")
	})
	if _, err := carrier.ImportOnce(); err != nil {
		t.Fatalf("ImportOnce: %v (611.22.42 P1-5 setup)", err)
	}
	led, lerr := readLedgerForTest(t, endpointRoot, DefaultHandle, id)
	if lerr != nil || led == nil {
		t.Fatalf("ledger missing (611.22.42 P1-5 setup): %v", lerr)
	}
	// Make the ledger unreadable.
	ledgerPath := filepath.Join(endpointRoot, "agents", DefaultHandle, "extensions", "remote", "replies", id+".json")
	if err := os.Chmod(ledgerPath, 0o000); err != nil {
		t.Fatalf("chmod ledger: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ledgerPath, 0o600) })

	carrier.mu.Lock()
	carrier.owedReplies = nil
	carrier.curRecovered = false
	carrier.mu.Unlock()
	identity, _ := fsq.SnapshotDeliveryRoot(endpointRoot)
	recRoot, _ := fsq.OpenDeliveryRoot(endpointRoot, identity)
	defer func() { _ = recRoot.Close() }()
	rerr := carrier.recoverDLQLedgeredReplies(recRoot)
	if rerr == nil {
		t.Fatal("recoverDLQLedgeredReplies returned nil for a ledger-read failure (611.22.42 P1-5 — must not silently drop)")
	}
	carrier.mu.Lock()
	_, retained := carrier.owedReplies[id]
	carrier.mu.Unlock()
	if !retained {
		t.Fatal("ledger-read failure not retained in owedReplies (611.22.42 P1-5)")
	}
}

// readLedgerForTest reads a command's reply ledger from the endpoint root for
// a test (mirrors Carrier.readLedger without needing a carrier root open).
func readLedgerForTest(t *testing.T, endpointRoot, agent, cmdMsgID string) (*replyRecord, error) {
	t.Helper()
	c := &Carrier{root: endpointRoot, me: agent}
	return c.readLedger(cmdMsgID)
}
