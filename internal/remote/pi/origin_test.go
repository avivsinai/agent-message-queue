package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// A record whose carrier named its source delivers that origin to the
// bridge, so the harness can show "from <name>" (agent-message-queue-9dx.7,
// ruling x). Routing fields such as the sink stay in AMQ.
func TestSubmitDeliversTheRecordOrigin(t *testing.T) {
	dir := newExtDir(t)
	store, err := requests.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	ep := core.New(core.Config{Store: store})
	defer func() { _ = ep.Close() }()
	ep.Register(mustAttach(t, dir))
	src := core.Source{Host: "link-1", Origin: map[string]string{"carrier": "link", "sink": "link-1", "name": "example"}}
	id := "00000000-0000-4000-8000-000000000043"
	ref := protocol.EncodeRef(src.Host, "pi-1", id)
	writeReceipt(t, dir, ref, "gen-1", fixedNow)
	if _, err := ep.Handle(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestSubmit, RequestID: id, TargetID: "pi-1",
		Epoch: addressEpoch("gen-1"), NotAfter: protocol.FormatTime(time.Now().Add(time.Hour)),
		Input: &protocol.SubmitInput{Text: "hi", Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn, MinEvidence: protocol.EvidenceSubmitted}}, src); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "requests", ref+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Origin json.RawMessage `json:"origin"`
	}
	if err := json.Unmarshal(data, &req); err != nil || string(req.Origin) != `{"carrier":"link","name":"example"}` {
		t.Fatalf("request origin = %s (%v), want {carrier: link, name: example}", req.Origin, err)
	}
}
