package linkio

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/pi"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// In the 2026-11-24 live run, a grant signed before the first pi receipt
// stopped admitting tasks as soon as the adapter read that receipt.
func TestPiGrantSurvivesFirstReceipt(t *testing.T) {
	view, key := goldenView(t)
	doc, err := DecodeGrant(readLinkFixture(t, "grant/document.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc.NativeSessionID = "pi-session-1"
	root := t.TempDir()
	dir := filepath.Join(root, "agents", "demo", "extensions", "pi-bridge")
	if err := os.MkdirAll(filepath.Join(dir, "receipts"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Script the bridge's liveness and delivery receipts on its file seam.
	if err := os.WriteFile(filepath.Join(dir, "bridge.liveness"), mustJSON(map[string]any{
		"protocol": pi.ProtocolV1, "live": true, "pid": 42, "surface": "tui",
		"session_generation": "gen-1", "session_id": doc.NativeSessionID,
		"bridge_revision": pi.GateBridgeRevision, "gate": true,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	att, err := pi.Factory(context.Background(), registry.FactoryConfig{Root: root, Target: doc.TargetID, Config: pi.Config("demo", "")})
	if err != nil {
		t.Fatal(err)
	}
	runtime := att.(*pi.Attachment)
	binding := func() Binding {
		b := view.Bindings[doc.Binding]
		b.Epoch = runtime.Inspect().Epoch
		b.NativeSessionID = runtime.NativeSessionID()
		return b
	}
	device, err := MintDeviceKey(t.TempDir(), "example")
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := New(Config{
		Name: "example", Pin: Pin{URL: "wss://link.example.invalid/v1", ServerID: view.ServerID},
		StoreID: view.StoreID, Key: device, Now: grantClock().now,
		Grants:      &memGrants{g: Grants{doc.Binding: {Generation: doc.GrantGeneration}}},
		Bindings:    func() []Binding { return []Binding{binding()} },
		ConsentKeys: func() ([]ConsentKey, error) { return []ConsentKey{key}, nil },
		GrantInfoFor: func(Binding) (bool, string, int) {
			asks, why := runtime.SessionAsks()
			return asks, why, protocol.MaxInputBytes
		},
		Handle: func(cmd *protocol.Command, src core.Source) (any, error) {
			k := requests.Key{CreatorHost: src.Host, TargetID: cmd.TargetID, RequestID: cmd.RequestID}
			ref := protocol.EncodeRef(k.CreatorHost, k.TargetID, k.RequestID)
			receipt := mustJSON(map[string]any{"protocol": pi.ProtocolV1, "ref": ref, "session_generation": "gen-1", "pid": 42})
			if err := os.WriteFile(filepath.Join(dir, "receipts", ref+".json"), receipt, 0o600); err != nil {
				return nil, err
			}
			ev, err := att.Lookup(k, cmd.Epoch)
			if err != nil || !ev.Admitted {
				t.Fatalf("pi receipt for %s: %+v, %v", cmd.RequestID, ev, err)
			}
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit, Evidence: protocol.EvidenceSubmitted}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc.Epoch = binding().Epoch
	raw, err := jcs.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	signed := SignedGrant(signDoc(t, key, raw))
	signed.Schema = SchemaSignedGrant
	reply, err := carrier.acceptGrant(signed, doc.Binding)
	if err != nil {
		t.Fatal(err)
	}
	digest := reply.(GrantReply).Grant.Digest
	for i, marker := range []string{"GRANT-PI-FIRST", "GRANT-PI-SECOND"} {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		cmd := protocol.Command{Op: protocol.OpRequestSubmit, RequestID: id, TargetID: doc.TargetID, Epoch: binding().Epoch, NotAfter: "2026-10-09T14:11:00Z",
			Input: &protocol.SubmitInput{Text: marker, Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn, MinEvidence: protocol.EvidenceSubmitted}}
		if _, err := carrier.admitGranted(digest, &cmd); err != nil {
			t.Fatalf("granted submit %s: %v", id, err)
		}
	}
}
