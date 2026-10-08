package core_test

import (
	"sync/atomic"
	"testing"
	"time"

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

// 611.42.6 (Pro review of #986 r1): answer A's native call refused and
// was paused before its rollback; answer B, the same option, recorded its
// intent and was delivered; A's rollback then compared only the option and
// erased B's intent. Answers to one interaction now run one at a time, so
// A's rollback undoes only its own intent and B's stands.
func TestRefusedAnswerRollbackKeepsALaterAnswer(t *testing.T) {
	store, now := openStore(t)
	rt := &pausedRefusal{Runtime: fake.New("fake", "e_1"), entered: make(chan struct{}), release: make(chan struct{})}
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111111e2"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})
	answer := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: protocol.EncodeRef("local", "fake", id),
		TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: "yes",
	}
	aDone, bDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := ep.Handle(answer, ownerShare); aDone <- err }()
	<-rt.entered
	go func() { _, err := ep.Handle(answer, ownerShare); bDone <- err }()
	select {
	case err := <-bDone: // B ran beside the paused A
		bDone <- err
	case <-time.After(300 * time.Millisecond): // B waits for A
	}
	close(rt.release)
	if err := <-aDone; err == nil {
		t.Fatal("A's refused answer reported success")
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("B never finished after A")
	}
	rec, _, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Answered["i_1"] != "yes" {
		t.Fatalf("answered = %v, want B's yes kept", rec.Answered)
	}
}

// pausedRefusal pauses the first answer until release closes and then
// refuses it; later answers reach the runtime.
type pausedRefusal struct {
	*fake.Runtime
	calls            atomic.Int32
	entered, release chan struct{}
}

func (p *pausedRefusal) Respond(key requests.Key, epoch, interactionID, option string) (protocol.Code, error) {
	if p.calls.Add(1) == 1 {
		close(p.entered)
		<-p.release
		return protocol.CodeNativeError, nil
	}
	return p.Runtime.Respond(key, epoch, interactionID, option)
}

func asRefusal(err error, target **protocol.Refusal) bool {
	if r, ok := err.(*protocol.Refusal); ok {
		*target = r
		return true
	}
	return false
}
