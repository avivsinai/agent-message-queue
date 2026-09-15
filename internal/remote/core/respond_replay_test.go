package core_test

import (
	"errors"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// TestRespondTransportErrorIsReplayedNotAlreadyResolved reproduces the gate
// finding on agent-message-queue-611.22.36 (union 42d923a4, Pro #20 reopened
// at the endpoint): the answer intent was persisted before the native call,
// the call failed with a transport error, and every retry short-circuited on
// the intent with already_resolved while the question stayed pending at the
// runtime. The retry must reach the runtime exactly once.
func TestRespondTransportErrorIsReplayedNotAlreadyResolved(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111111f2"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})

	ref := protocol.EncodeRef("local", "fake", id)
	answer := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: ref,
		TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: "yes",
	}
	rt.FailNextRespond(errors.New("app-server: write: broken pipe"))
	if _, err := ep.Handle(answer, core.Source{Host: "local"}); err == nil {
		t.Fatal("first answer reported success although the native call failed")
	}
	if answers := rt.Snapshot().Answers; len(answers) != 0 {
		t.Fatalf("failed answer reached the runtime: %+v", answers)
	}

	rep, err := ep.Handle(answer, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	reply, ok := rep.(protocol.Reply)
	if !ok || reply.Outcome.Code != "" {
		t.Fatalf("retry outcome = %T %+v, want a delivered answer with no code (not already_resolved)", rep, rep)
	}
	answers := rt.Snapshot().Answers
	if len(answers) != 1 || answers[0].InteractionID != "i_1" || answers[0].Option != "yes" {
		t.Fatalf("retry did not land exactly once at the runtime: %+v", answers)
	}
	// A third replay after the answer landed is a genuine replay.
	rep, err = ep.Handle(answer, core.Source{Host: "local"})
	if err != nil {
		t.Fatalf("replay after delivery: %v", err)
	}
	if reply, ok := rep.(protocol.Reply); !ok || reply.Outcome.Code != protocol.CodeAlreadyResolved {
		t.Fatalf("replay after delivery = %+v, want already_resolved", rep)
	}
	if answers := rt.Snapshot().Answers; len(answers) != 1 {
		t.Fatalf("replay re-sent the answer: %+v", answers)
	}
}
