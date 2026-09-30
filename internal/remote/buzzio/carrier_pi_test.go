package buzzio

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	root := t.TempDir()
	bridge := filepath.Join(root, "agents", "pi-seat", "extensions", "pi-bridge")
	for _, sub := range []string{"requests", "receipts", "events"} {
		if err := os.MkdirAll(filepath.Join(bridge, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func() {
		rec := fmt.Sprintf(`{"protocol":%q,"live":true,"at":%q,"pid":%d,"surface":"rpc","session_generation":"gen-1","bridge_revision":4,"session_id":"sess-1"}`,
			pi.ProtocolV1, time.Now().UTC().Format(time.RFC3339Nano), os.Getpid())
		if err := os.WriteFile(filepath.Join(bridge, "bridge.liveness"), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stamp()
	// The extension's delivery: a receipt for every published request.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			entries, _ := os.ReadDir(filepath.Join(bridge, "requests"))
			for _, e := range entries {
				ref, ok := strings.CutSuffix(e.Name(), ".json")
				if !ok || strings.HasPrefix(ref, ".") {
					continue
				}
				rc := fmt.Sprintf(`{"protocol":%q,"ref":%q,"session_generation":"gen-1","delivered_at":%q,"pid":%d}`,
					pi.ProtocolV1, ref, time.Now().UTC().Format(time.RFC3339Nano), os.Getpid())
				path := filepath.Join(bridge, "receipts", ref+".json")
				if _, err := os.Stat(path); os.IsNotExist(err) {
					_ = os.WriteFile(path, []byte(rc), 0o600)
				}
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	att, err := pi.Factory(context.Background(), registry.FactoryConfig{Root: root, Target: "pi-1", Config: json.RawMessage(`{"handle":"pi-seat"}`)})
	if err != nil {
		t.Fatal(err)
	}
	store, err := requests.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	var c *Carrier
	ep := core.New(core.Config{Store: store, Publish: func(s protocol.Snapshot, origin map[string]string) error { return c.Publish(s, origin) }})
	defer func() { _ = ep.Close() }()
	ep.Register(att)

	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "pi-1", RelayHost: "relay",
		NativeSession: "sess-1", MinEvidence: protocol.EvidenceSubmitted}
	ledger, _ := OpenLedger(t.TempDir())
	c = NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), ep.NativeSessionID, ep.Handle)
	base := time.Now()
	var tick atomic.Int64
	c.now = func() time.Time { return base.Add(time.Duration(tick.Add(1)) * time.Second) }
	var sent []nostr.Event
	flush := func() {
		sent = nil
		if err := c.Flush(context.Background(), func(_ context.Context, evt nostr.Event) error { sent = append(sent, evt); return nil }, nil); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.Ingest(ownerEvent(t, owner, "dm-1", "run the tests", c.now())); err != nil {
		t.Fatal(err)
	}
	flush()
	if len(sent) == 0 || !strings.Contains(sent[0].Content, "running") || !strings.Contains(sent[0].Content, "its start is not proven") {
		t.Fatalf("result row = %+v, want a running row that says the start is not proven", sent)
	}
	ref := strings.SplitN(sent[0].Content, ":", 2)[0]

	stamp()
	line := fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"interaction","at":"x","interaction_id":"tool-1","kind":"approval","prompt":"run: go test ./...","options":["Allow once","Block"],"approve_option":"Allow once","reject_option":"Block","manifest_hash":"sha256:abc","expires_at":%q,"presence":"remote"}`,
		pi.ProtocolV1, ref, time.Now().Add(5*time.Minute).UTC().Format(time.RFC3339))
	appendLine(t, filepath.Join(bridge, "events", ref+".jsonl"), line)
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
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

	stamp()
	react := nostr.Event{CreatedAt: nostr.Timestamp(c.now().Unix()), Kind: KindReaction, Content: "✅", Tags: nostr.Tags{{"e", msg.ID.Hex()}}}
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

	stamp()
	appendLine(t, filepath.Join(bridge, "events", ref+".jsonl"),
		fmt.Sprintf(`{"protocol":%q,"ref":%q,"event":"interaction_resolved","at":"x","interaction_id":"tool-1","outcome":"answered","option":"Allow once"}`, pi.ProtocolV1, ref))
	if err := ep.Reconcile(); err != nil {
		t.Fatal(err)
	}
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
