package core_test

import (
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// TestRespondCrashReplayAnswersOnce pins the answer-INTENT half of bead
// 611.22.12: a crash between the durable intent and the native Respond call
// leaves the replay hitting Answered, so the attachment is invoked exactly
// once even across the crash — the refused-then-valid regression must not
// reintroduce a double-answer hole.
func TestRespondCrashReplayAnswersOnce(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)

	id := "11111111-1111-4111-8111-1111111111e2"
	if _, err := ep.Handle(submitCmd(id), core.Source{Host: "local"}); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})
	time.Sleep(50 * time.Millisecond)

	ref := protocol.EncodeRef("local", "fake", id)
	respond := &protocol.Command{
		Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: ref,
		TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: "yes",
	}
	if _, err := ep.Handle(respond, core.Source{Host: "local"}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	// Replay after the (simulated) crash: must not answer twice.
	if _, err := ep.Handle(respond, core.Source{Host: "local"}); err != nil {
		t.Fatalf("respond replay: %v", err)
	}
	answers := rt.Snapshot().Answers
	if len(answers) != 1 || answers[0].InteractionID != "i_1" || answers[0].Option != "yes" {
		t.Fatalf("interaction answered %d times: %+v", len(answers), answers)
	}
}
