package pi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestGoldenExtensionVector reads the bytes the TypeScript extension
// produces (integrations/pi/testdata/pi-bridge, asserted by its node:test)
// through the adapter's own readers, so neither side is proven only against
// its own fake.
func TestGoldenExtensionVector(t *testing.T) {
	src := filepath.Join("..", "..", "..", "integrations", "pi", "testdata", "pi-bridge")
	dir := t.TempDir()
	for _, rel := range []string{
		"bridge.liveness",
		"receipts/amqr1_nbxxg5brabygsljraaydambqgaydambngaydambngqydambnhaydambngaydambqgaydambqgayq.json",
		"receipts/amqr1_nbxxg5brabygsljraaydambqgaydambngaydambngqydambnhaydambngaydambqgaydambqgaza.json",
		"events/amqr1_nbxxg5brabygsljraaydambqgaydambngaydambngqydambnhaydambngaydambqgaydambqgayq.jsonl",
		"events/amqr1_nbxxg5brabygsljraaydambqgaydambngaydambngqydambnhaydambngaydambqgaydambqgaza.jsonl",
	} {
		data, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(dir, "bridge.liveness"), fixedNow, fixedNow); err != nil {
		t.Fatal(err)
	}
	bd := bridgeDir{dir: dir, names: piWire}

	live := bd.liveness(fixedNow)
	// Bead agent-message-queue-611.60: the reference bridge's session id is
	// the native session id a relay share pins.
	if !live.live || live.pid != 4242 || live.gen != "golden-generation" || live.revision != MinBridgeRevision || live.sessionID != "pi-session-1" {
		t.Fatalf("liveness = %+v, want live pid 4242 generation golden-generation revision %d session pi-session-1", live, MinBridgeRevision)
	}

	key := requests.Key{CreatorHost: "host1", TargetID: "pi-1", RequestID: "00000000-0000-4000-8000-000000000001"}
	rc, err := bd.readReceipt(clientRef(key))
	if err != nil || rc == nil || rc.SessionGeneration != "golden-generation" {
		t.Fatalf("readReceipt = %+v, %v", rc, err)
	}

	a, err := New("pi-1", "pi-seat", bd)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := a.Lookup(key, "")
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Admitted || ev.State != protocol.StateCompleted || ev.Result == nil || ev.Result.Text != "two files changed" {
		t.Fatalf("Lookup(completed) = %+v result %+v", ev, ev.Result)
	}

	// The request vector is the adapter's bytes, which the extension's
	// node:test delivers: the adapter writes the bridge_revision the
	// extension checks.
	out := t.TempDir()
	if err := (bridgeDir{dir: out, names: piWire}).publishRequest(deliverRequest{
		Ref: clientRef(key), Text: "summarize the diff", DeliverAs: "followUp", NotAfter: "2036-01-01T00:00:00Z",
		EpochHint: "golden-generation", CreatedAt: "2000-01-01T00:00:00Z", BridgeRevision: MinBridgeRevision,
	}); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("requests", clientRef(key)+".json")
	got, err := os.ReadFile(filepath.Join(out, rel))
	if err != nil {
		t.Fatal(err)
	}
	if want, err := os.ReadFile(filepath.Join(src, rel)); err != nil || string(got) != string(want) {
		t.Fatalf("request bytes = %s, want golden %s (%v)", got, want, err)
	}
	// The publish leaves only the request: no temp file is left behind.
	if entries, err := os.ReadDir(filepath.Join(out, "requests")); err != nil || len(entries) != 1 {
		t.Fatalf("requests dir = %v (%d entries), want only the published request", err, len(entries))
	}

	orphan := requests.Key{CreatorHost: "host1", TargetID: "pi-1", RequestID: "00000000-0000-4000-8000-000000000002"}
	ev, err = a.Lookup(orphan, "")
	if err != nil {
		t.Fatal(err)
	}
	// Orphan recovery has no native outcome, so the record stays uncertain.
	if ev.Class != core.EvidenceUnknown || ev.State != protocol.StateUncertain {
		t.Fatalf("Lookup(orphan) = %+v, want unknown evidence in state uncertain", ev)
	}
}
