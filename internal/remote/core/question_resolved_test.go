package core_test

import (
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// TestQuestionResolvedClearsDurableInteraction reproduces Pro round 2 #21
// (bead agent-message-queue-611.22.36): EventQuestionResolved carries no
// interaction, and the evidence-only transition treated a nil interaction as
// "leave unchanged", so a successfully answered question stayed pending in
// the durable snapshot and in request.get.
func TestQuestionResolvedClearsDurableInteraction(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111111f1"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})

	key := requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id}
	rec, ok, err := store.Get(key)
	if err != nil || !ok {
		t.Fatalf("record after question: ok=%v err=%v", ok, err)
	}
	if rec.Interaction == nil || rec.Interaction.InteractionID != "i_1" {
		t.Fatalf("question not recorded as pending: %+v", rec.Interaction)
	}

	ref := protocol.EncodeRef("local", "fake", id)
	answer := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: ref,
		TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: "yes",
	}
	if _, err := ep.Handle(answer, core.Source{Host: "local"}); err != nil {
		t.Fatalf("answer: %v", err)
	}

	rec, _, err = store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Interaction != nil {
		t.Fatalf("resolved question still pending in the durable snapshot: %+v", rec.Interaction)
	}
	status := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}
	rep, err := ep.Handle(status, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if reply, ok := rep.(protocol.Reply); !ok || reply.Snapshot.Interaction != nil {
		t.Fatalf("request.get still reports a pending interaction: %T %+v", rep, rep)
	}
}
