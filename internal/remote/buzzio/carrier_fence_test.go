package buzzio

import (
	"crypto/rand"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Regression: #926, the fence saw the approved session,
// then the session switched before the epoch was read, and the submit went
// to the unshared session. The identity is checked again with the epoch
// fixed, and nothing is dispatched.
func TestSessionSwitchBeforeEpochDispatchesNothing(t *testing.T) {
	var owner, body [32]byte
	_, _ = rand.Read(owner[:])
	_, _ = rand.Read(body[:])
	b := Binding{Owner: nostr.GetPublicKey(owner).Hex(), Body: nostr.GetPublicKey(body).Hex(), Channel: "dm-1", Target: "pi-1", RelayHost: "relay", NativeSession: "sess-1"}
	ledger, _ := OpenLedger(t.TempDir())
	session := "sess-1"
	submitted := false
	c := NewCarrier(ledger, b, body, ownerGrant(t, owner, b.Body, KindDM, KindEdit), func(string) string { return session }, func(cmd *protocol.Command, src core.Source) (any, error) {
		switch cmd.Op {
		case protocol.OpSessionInspect:
			session = "sess-2" // /new between the fence and the epoch
			return protocol.Session{TargetID: "pi-1", Epoch: "unpinned.gen-2"}, nil
		case protocol.OpRequestSubmit:
			submitted = true
		}
		return protocol.Reply{}, nil
	})
	if err := c.Ingest(ownerEvent(t, owner, "dm-1", "run the tests", time.Now())); err != nil {
		t.Fatal(err)
	}
	if submitted {
		t.Fatal("submitted into a session that was not approved for sharing")
	}
}
