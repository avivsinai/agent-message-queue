package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
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
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})
	time.Sleep(50 * time.Millisecond)

	ref := protocol.EncodeRef("local", "fake", id)
	wrong := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: ref,
		TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: "maybe",
	}
	_, rerr := ep.Handle(wrong, core.Source{Host: "local"})
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
	if _, err := ep.Handle(right, core.Source{Host: "local"}); err != nil {
		t.Fatalf("valid answer after refusal: %v", err)
	}
	answers := rt.Snapshot().Answers
	if len(answers) != 1 || answers[0].InteractionID != "i_1" || answers[0].Option != "yes" {
		t.Fatalf("valid answer did not land exactly once: %+v", answers)
	}
}

func asRefusal(err error, target **protocol.Refusal) bool {
	if r, ok := err.(*protocol.Refusal); ok {
		*target = r
		return true
	}
	return false
}
