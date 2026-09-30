package core_test

import (
	"errors"
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
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
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
	if _, err := ep.Handle(answer, ownerShare); err != nil {
		t.Fatalf("answer: %v", err)
	}

	rec, _, err = store.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Interaction != nil {
		t.Fatalf("resolved question still pending in the durable snapshot: %+v", rec.Interaction)
	}
	// 611.42: the record says how it ended, so the DM can show the outcome.
	if want := (protocol.Resolution{InteractionID: "i_1", Outcome: protocol.ResolutionAnswered, Option: "yes"}); len(rec.Resolved) != 1 || rec.Resolved[0] != want {
		t.Fatalf("resolved = %+v, want %+v", rec.Resolved, want)
	}
	status := &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: ref}
	rep, err := ep.Handle(status, ownerShare)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if reply, ok := rep.(protocol.Reply); !ok || reply.Snapshot.Interaction != nil {
		t.Fatalf("request.get still reports a pending interaction: %T %+v", rep, rep)
	}
}

// PR #919 review: a second approval that replaced a pending one recorded
// the first as answered elsewhere, though the harness still held it open.
// Only a clear resolves an interaction.
func TestReplacedInteractionIsNotResolved(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	id := "11111111-1111-4111-8111-1111111111f2"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})
	rt.Question(id, "i_2", []string{"yes", "no"})
	rec, _, err := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Interaction == nil || rec.Interaction.InteractionID != "i_2" || len(rec.Resolved) != 0 {
		t.Fatalf("interaction = %+v resolved = %+v, want i_2 pending and nothing resolved", rec.Interaction, rec.Resolved)
	}
}

// PR #919 review round 3: while one answer could still be with the
// runtime, a different answer replaced its durable intent, and the record
// later named the second answer for what the first one did. A different
// answer is refused until the first is settled.
func TestSecondAnswerDoesNotReplaceLiveIntent(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	id := "11111111-1111-4111-8111-1111111111f3"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})
	answer := func(option string) error {
		_, err := ep.Handle(&protocol.Command{
			Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: protocol.EncodeRef("local", "fake", id),
			TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: option,
		}, ownerShare)
		return err
	}
	rt.FailNextRespond(errors.New("transport lost after write"))
	if err := answer("yes"); err == nil {
		t.Fatal("setup: the first answer should fail in transport")
	}
	if err := answer("no"); err == nil {
		t.Fatal("a different answer replaced an intent that may already be applied")
	}
	if err := answer("yes"); err != nil {
		t.Fatalf("replay of the first answer: %v", err)
	}
	rec, _, _ := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if want := (protocol.Resolution{InteractionID: "i_1", Outcome: protocol.ResolutionAnswered, Option: "yes"}); len(rec.Resolved) != 1 || rec.Resolved[0] != want {
		t.Fatalf("resolved = %+v, want %+v", rec.Resolved, want)
	}
}

// PR #919 review round 4: the run ended between our answer's send and its
// resolution, and the record said the run ended before an answer.
func TestRunEndAfterSentAnswerRecordsTheAnswer(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	id := "11111111-1111-4111-8111-1111111111f4"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})
	rt.FailNextRespond(errors.New("transport lost after write"))
	if _, err := ep.Handle(&protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: protocol.EncodeRef("local", "fake", id),
		TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: "yes",
	}, ownerShare); err == nil {
		t.Fatal("setup: the answer should fail in transport")
	}
	rt.Complete(id, "done")
	rec, _, _ := store.Get(requests.Key{CreatorHost: "local", TargetID: "fake", RequestID: id})
	if want := (protocol.Resolution{InteractionID: "i_1", Outcome: protocol.ResolutionAnswered, Option: "yes"}); len(rec.Resolved) != 1 || rec.Resolved[0] != want {
		t.Fatalf("resolved = %+v, want %+v", rec.Resolved, want)
	}
}
