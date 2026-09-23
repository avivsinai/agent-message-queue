package buzzio

import (
	"testing"

	"fiatjaf.com/nostr"
)

// codex #867 r1: a policy without parallelism read as present, but the
// pinned Desktop parser drops it, so the body was never listed.
func TestPolicyForMatchesDesktopParser(t *testing.T) {
	owner, body := nostr.Generate(), nostr.GetPublicKey(nostr.Generate()).Hex()
	policy := func(content string) nostr.Event {
		evt := nostr.Event{CreatedAt: nostr.Now(), Kind: KindManagedAgent, Content: content, Tags: nostr.Tags{{"d", body}}}
		if err := evt.Sign(owner); err != nil {
			t.Fatal(err)
		}
		return evt
	}
	if PolicyFor(policy(`{"name":"AMQ session","respond_to":"owner-only"}`), body) {
		t.Fatal("a policy without parallelism was accepted")
	}
	if !PolicyFor(policy(`{"name":"AMQ session","parallelism":1,"respond_to":"owner-only"}`), body) {
		t.Fatal("a complete policy was refused")
	}
}
