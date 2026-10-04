package core_test

import (
	"errors"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// ownerShare is the source of the owner's Buzz share: only it may answer an
// interaction of a request it submitted (agent-message-queue-611.46).
var ownerShare = core.Source{Host: "local", Origin: map[string]string{"carrier": "buzz", "body": "body-1", "channel": "dm-1"}}

// TestCancelBeforeSubmitOnBuzzHostIgnoresCase reproduces the Pro review of
// #934 (agent-message-queue-611.48): a local sender re-encoded a Buzz
// request's creator host as BUZZ- and left a tombstone that, on a
// case-insensitive filesystem, the owner's own submit would read back.
func TestCancelBeforeSubmitOnBuzzHostIgnoresCase(t *testing.T) {
	ep, _, _ := newB14aEndpoint(t)
	cmd := &protocol.Command{
		Schema:     protocol.SchemaCommand,
		Op:         protocol.OpRequestCancel,
		RequestRef: protocol.EncodeRef("BUZZ-0123456789abcdef", "fake", "11111111-1111-4111-8111-111111111148"),
		TargetID:   "fake",
		Epoch:      "e_1",
		NotAfter:   "2026-09-08T10:02:00Z",
	}
	_, err := ep.Handle(cmd, core.Source{Host: "local"})
	var r *protocol.Refusal
	if !errors.As(err, &r) || r.Code != protocol.CodeUnshared {
		t.Fatalf("cancel = %v, want unshared", err)
	}
}
