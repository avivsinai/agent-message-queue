package buzzio

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Pro review of #926, 2026-09-30, #1: the Buzz approval path to pi was
// unreachable. The carrier's fence needs the pi session id, and its submit
// needed admitted evidence, which pi never gives. Through the real carrier,
// endpoint and pi adapter over a revision-4 bridge: a share at submitted
// evidence submits, the approval posts, a ✅ writes the answer file, and
// interaction_resolved answered edits the message with the outcome.
func TestPiApprovalAnsweredFromBuzz(t *testing.T) {
	// Pro review of #926 r2, 2026-09-30, #6: synthetic clocks only. The pi
	// adapter, the endpoint and the liveness record stand at base; the
	// carrier steps one second per call, so its edits are strictly later.
	// The extension's receipt is laid down before the submit, so nothing
	// here depends on how fast the machine runs.
	base := time.Now().Truncate(time.Second)
	clock := func() time.Time { return base }
	var tick atomic.Int64
	advance := func() time.Time { return base.Add(time.Duration(tick.Add(1)) * time.Second) }

	root := t.TempDir()
	bridge := filepath.Join(root, "agents", "pi-seat", "extensions", "pi-bridge")
	for _, sub := range []string{"requests", "receipts", "events"} {
		if err := os.MkdirAll(filepath.Join(bridge, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	func() {
		at := base
		rec := fmt.Sprintf(`{"protocol":%q,"live":true,"at":%q,"pid":%d,"surface":"rpc","session_generation":"gen-1","bridge_revision":4,"session_id":"sess-1"}`,
			pi.ProtocolV1, at.UTC().Format(time.RFC3339Nano), os.Getpid())
		path := filepath.Join(bridge, "bridge.liveness")
		if err := os.WriteFile(path, []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}()

	att, err := pi.Factory(context.Background(), registry.FactoryConfig{Root: root, Target: "pi-1", Config: json.RawMessage(`{"handle":"pi-seat"}`)})
	if err != nil {
		t.Fatal(err)
	}
	att.(*pi.Attachment).SetNow(clock)
	store, err := requests.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	var c *Carrier
	ep := core.New(core.Config{Store: store, Now: clock, Publish: func(s protocol.Snapshot, origin map[string]string) error { return c.Publish(s, origin) }})
	defer func() { _ = ep.Close() }()
	ep.Register(att)

	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "pi-1", RelayHost: "relay",
		NativeSession: "sess-1", MinEvidence: protocol.EvidenceSubmitted}
	ledger, _ := OpenLedger(t.TempDir())
	c = NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), ep.NativeSessionID, ep.Handle)
	c.now = advance
	var sent []nostr.Event
	flush := func() {
		sent = nil
		if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil); err != nil {
			t.Fatal(err)
		}
	}

	dm := ownerEvent(t, owner, "dm-1", "run the tests", clock())
	n, err := Normalize(dm, b, clock())
	if err != nil {
		t.Fatal(err)
	}
	// The extension's delivery of this request, written ahead of the submit.
	want := protocol.EncodeRef(c.source(dm.ID.Hex(), "").Host, "pi-1", n.RequestID)
	rc := fmt.Sprintf(`{"protocol":%q,"ref":%q,"session_generation":"gen-1","delivered_at":%q,"pid":%d}`,
		pi.ProtocolV1, want, clock().UTC().Format(time.RFC3339Nano), os.Getpid())
	if err := os.WriteFile(filepath.Join(bridge, "receipts", want+".json"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pro review of #926 r3, 2026-09-30, #3: publication runs on whichever
	// goroutine owns the record, so Reconcile can return before the owed
	// output is prepared. Each step waits, bounded, until the output it
	// asserts on is in the outbox, then flushes.
	await := func(key string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			if _, ok, err := ledger.Prepared(key); err != nil || ok {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s was never prepared", key)
			}
			if err := ep.Reconcile(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := c.Ingest(dm); err != nil {
		t.Fatal(err)
	}
	await(rootKey(want))
	flush()
	if len(sent) == 0 || !strings.Contains(sent[0].Content, "running") || !strings.Contains(sent[0].Content, "its start is not proven") {
		t.Fatalf("result row = %+v, want a running row that says the start is not proven", sent)
	}
	ref := want

	line := fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"interaction","at":"x","interaction_id":"tool-1","kind":"approval","prompt":"run: go test ./...","options":["Allow once","Block"],"approve_option":"Allow once","reject_option":"Block","manifest_hash":"sha256:abc","expires_at":%q,"presence":"remote"}`,
		pi.ProtocolV1, ref, clock().Add(5*time.Minute).UTC().Format(time.RFC3339))
	appendLine(t, filepath.Join(bridge, "events", ref+".jsonl"), line)
	await(approvalKey(ref, "tool-1"))
	flush()
	var msg nostr.Event
	for _, evt := range sent {
		if strings.Contains(evt.Content, "Approval needed") {
			msg = evt
		}
	}
	if !strings.Contains(msg.Content, "go test ./...") || !strings.Contains(msg.Content, "React ✅") {
		t.Fatalf("approval message = %q, want the call and how to answer", msg.Content)
	}

	react := nostr.Event{CreatedAt: nostr.Timestamp(advance().Unix()), Kind: KindReaction, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
	if err := react.Sign(owner); err != nil {
		t.Fatal(err)
	}
	if err := c.IngestReaction(react); err != nil {
		t.Fatal(err)
	}
	answers, _ := os.ReadDir(filepath.Join(bridge, "answers"))
	if len(answers) != 1 {
		t.Fatalf("answers = %v, want one answer file", answers)
	}
	data, _ := os.ReadFile(filepath.Join(bridge, "answers", answers[0].Name()))
	var ans map[string]string
	if err := json.Unmarshal(data, &ans); err != nil || ans["ref"] != ref || ans["interaction_id"] != "tool-1" || ans["option"] != "Allow once" || ans["manifest_hash"] != "sha256:abc" {
		t.Fatalf("answer = %s (%v), want Allow once for tool-1 of %s", data, err, ref)
	}

	appendLine(t, filepath.Join(bridge, "events", ref+".jsonl"),
		fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"interaction_resolved","at":"x","interaction_id":"tool-1","outcome":"answered","option":"Allow once"}`, pi.ProtocolV1, ref))
	await(approvalKey(ref, "tool-1") + "/outcome")
	flush()
	edited := false
	for _, evt := range sent {
		edited = edited || evt.Kind == KindEdit && tagValue(evt, "e") == msg.ID.Hex() && strings.Contains(evt.Content, "Approve was sent from Buzz")
	}
	if !edited {
		t.Fatalf("sent = %+v, want the approval message edited with the sent answer", sent)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := fmt.Fprintln(f, line); err != nil {
		t.Fatal(err)
	}
}
