package core_test

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestRefusedAnswerDoesNotPoisonValidAnswer reproduces bead 611.22.12 (B07):
// an answer that the attachment positively refuses (here: a non-offered
// option) must not be persisted as consumed — the follow-up answer with a
// valid option must still reach the attachment. Before the fix the endpoint
// persisted Answered[interactionID]=option BEFORE the native call, so the
// refused answer permanently consumed the interaction and the valid retry
// was refused already_resolved.
func TestRefusedAnswerDoesNotPoisonValidAnswer(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111111e1"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})

	ref := protocol.EncodeRef("local", "fake", id)
	wrong := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: ref,
		TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: "maybe",
	}
	_, rerr := ep.Handle(wrong, ownerShare)
	if rerr == nil {
		t.Fatal("refused answer returned no error")
	}
	var refusal *protocol.Refusal
	if !asRefusal(rerr, &refusal) || refusal.Code != protocol.CodeInvalid {
		t.Fatalf("refused answer err = %v, want code_invalid refusal", rerr)
	}
	if answers := rt.Snapshot().Answers; len(answers) != 0 {
		t.Fatalf("refused answer reached the attachment: %+v", answers)
	}

	// The poison assertion: the same interaction, now with a VALID option,
	// must still be answerable.
	right := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: ref,
		TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: "yes",
	}
	if _, err := ep.Handle(right, ownerShare); err != nil {
		t.Fatalf("valid answer after refusal: %v", err)
	}
	answers := rt.Snapshot().Answers
	if len(answers) != 1 || answers[0].InteractionID != "i_1" || answers[0].Option != "yes" {
		t.Fatalf("valid answer did not land exactly once: %+v", answers)
	}
}

// retiringAttachment says the recorded allow was refused by the runtime,
// and refuses the deny that replaces it as already resolved: a new allow
// was written there first.
type retiringAttachment struct{ *delegatingAttachment }

func (retiringAttachment) AnswerRetired(requests.Key, string, string) bool { return true }
func (retiringAttachment) Respond(_ requests.Key, _, _, option string) (protocol.Code, error) {
	if option == "deny" {
		return protocol.CodeAlreadyResolved, nil
	}
	return "", nil
}

// 611.42.6 (independent review of #936 r3): after the runtime refused an
// allow, a ✅ and a ❌ arrived at once. The ❌ replaced the recorded allow,
// the runtime refused the ❌ because the new allow was there first, and the
// refusal cleared the record: Answered ended empty while the allow
// applied. A refused answer now restores the answer it replaced.
func TestRefusedReplacementKeepsTheAnswerItReplaced(t *testing.T) {
	store, now := openStore(t)
	rt := retiringAttachment{&delegatingAttachment{inner: fake.New("fake", "e_1")}}
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111111e2"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.inner.(*fake.Runtime).Question(id, "i_1", []string{"allow", "deny"})
	ref := protocol.EncodeRef("local", "fake", id)
	for _, option := range []string{"allow", "deny"} {
		_, _ = ep.Handle(&protocol.Command{
			Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: ref,
			TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: option,
		}, ownerShare)
	}
	rec, _, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Answered["i_1"]; got != "allow" {
		t.Fatalf("Answered = %q, want the allow the runtime holds", got)
	}
}

func asRefusal(err error, target **protocol.Refusal) bool {
	if r, ok := err.(*protocol.Refusal); ok {
		*target = r
		return true
	}
	return false
}
