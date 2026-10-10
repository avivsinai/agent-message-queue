package core_test

import (
	"context"
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

// The user's own terminal keeps amq-remote status and wait on a Buzz
// request (9dx.3 review S5, ruling s); another Buzz share cannot read it.
func TestLocalReadsABuzzRequestAndAnotherShareCannot(t *testing.T) {
	ep, _, _ := newB14aEndpoint(t)
	id := "11111111-1111-4111-8111-1111111111f6"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	ref := protocol.EncodeRef("local", "fake", id)
	get := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}
	if _, err := ep.Handle(get, core.Source{Host: "local"}); err != nil {
		t.Fatalf("local get of a Buzz request: %v", err)
	}
	if _, err := ep.WaitAfter(context.Background(), ref, new(int64)); err != nil {
		t.Fatalf("local wait on a Buzz request: %v", err)
	}
	other := core.Source{Host: "local", Origin: map[string]string{"carrier": "buzz", "body": "body-2", "channel": "dm-1"}}
	var r *protocol.Refusal
	if _, err := ep.Handle(get, other); !errors.As(err, &r) || r.Code != protocol.CodeUnshared {
		t.Fatalf("another share's get = %v, want unshared", err)
	}
}
