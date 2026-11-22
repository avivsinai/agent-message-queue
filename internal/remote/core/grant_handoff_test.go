package core_test

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Session grant contract, sections 2 and 3: a granted request is checked at
// the hand-over, never waits for an offline target, and never takes over a
// signed request's id.
func TestGrantedRequestAtTheHandover(t *testing.T) {
	rt := fake.New("fake", "e_1")
	ep, store, clk := newLinkEndpoint(t, rt)
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	reason := ""
	ep.SetGrant(func(host, target, d string) string {
		if host != hostA || target != "fake" || d != digest {
			return "grant_unknown: no such grant"
		}
		return reason
	})
	granted := linkSource(hostA, "fake")
	granted.GrantDigest = digest
	dispatches := func() int { return rt.Snapshot().Dispatches }
	keyOf := func(cmd *protocol.Command) requests.Key {
		return requests.Key{CreatorHost: hostA, TargetID: "fake", RequestID: cmd.RequestID}
	}

	// A signed request runs; a granted and a signed one sent meanwhile are
	// busy tombstones.
	running := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111f1")
	submitReply(t, ep, running, linkSource(hostA, "fake"))
	grantedCmd := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111f2")
	if r := submitReply(t, ep, grantedCmd, granted); r.Outcome.Code != protocol.CodeBusy {
		t.Fatalf("granted submit while busy = %q, want busy", r.Outcome.Code)
	}
	signedCmd := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111f3")
	if r := submitReply(t, ep, signedCmd, linkSource(hostA, "fake")); r.Outcome.Code != protocol.CodeBusy {
		t.Fatalf("signed submit while busy = %q, want busy", r.Outcome.Code)
	}
	rt.Complete(running.RequestID, "done")
	before := dispatches()

	// Attack finding: a granted send with the id, input and deadline of a
	// signed busy request would run it with no signature.
	if r := submitReply(t, ep, signedCmd, granted); r.Outcome.Code != protocol.CodeRequestConflict || dispatches() != before {
		t.Fatalf("granted send of a signed busy request = %q, dispatches %d -> %d; want request_conflict, no run", r.Outcome.Code, before, dispatches())
	}

	// The grant ends before the granted request is sent again: the hand-over
	// refuses it, and the request ends rejected consent_invalid with the
	// reason that request.get reads.
	reason = "grant_revoked: the owner revoked the grant"
	r := submitReply(t, ep, grantedCmd, granted)
	if r.Outcome.Code != protocol.CodeConsentInvalid || r.Outcome.Message != reason || dispatches() != before {
		t.Fatalf("granted resend after the grant ended = %q %q, dispatches %d -> %d; want consent_invalid with the reason, no run", r.Outcome.Code, r.Outcome.Message, before, dispatches())
	}
	out, err := ep.Handle(getCmd(r.Snapshot.RequestRef), linkSource(hostA, "fake"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := out.(protocol.Reply); got.Snapshot.State != protocol.StateRejected || got.Snapshot.Code != protocol.CodeConsentInvalid || got.Outcome.Message != reason {
		t.Fatalf("get of the refused granted request = %s/%s %q; want rejected consent_invalid with the reason", got.Snapshot.State, got.Snapshot.Code, got.Outcome.Message)
	}

	// An offline target answers busy and keeps no record.
	reason = ""
	rt.SetOffline(true)
	offline := linkSubmit(clk, "fake", "11111111-1111-4111-8111-1111111111f4")
	if r := submitReply(t, ep, offline, granted); r.Outcome.Code != protocol.CodeBusy {
		t.Fatalf("granted submit to an offline target = %q, want busy", r.Outcome.Code)
	}
	if _, exists, err := store.Get(keyOf(offline)); err != nil || exists {
		t.Fatalf("granted submit to an offline target left a record (%v, %v)", exists, err)
	}
}
