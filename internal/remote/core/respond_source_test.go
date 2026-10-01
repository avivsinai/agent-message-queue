package core_test

import (
	"errors"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/fake"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// agent-message-queue-611.46 (design review of 611.42 slice 1b): respond
// ignored the command source, so a local sender, the asking agent included,
// could answer an approval over the AMQ mailbox or the IPC socket. Only the
// owner's Buzz share that submitted the request may answer.
func TestOnlyOwnerShareAnswersAnInteraction(t *testing.T) {
	store, now := openStore(t)
	rt := fake.New("fake", "e_1")
	ep := core.New(core.Config{Store: store, Now: now})
	ep.Register(rt)
	id := "11111111-1111-4111-8111-1111111111f5"
	if _, err := ep.Handle(submitCmd(id), ownerShare); err != nil {
		t.Fatal(err)
	}
	rt.Question(id, "i_1", []string{"yes", "no"})
	answer := func(src core.Source) error {
		_, err := ep.Handle(&protocol.Command{
			Schema: protocol.SchemaCommand, Op: protocol.OpInteractionRespond, RequestRef: protocol.EncodeRef("local", "fake", id),
			TargetID: "fake", Epoch: "e_1", InteractionID: "i_1", Option: "yes",
		}, src)
		return err
	}
	for name, src := range map[string]core.Source{
		"amq mailbox": {Host: "local", Origin: map[string]string{"carrier": "amq", "handle": "codex"}},
		"ipc socket":  {Host: "local"},
		"other share": {Host: "local", Origin: map[string]string{"carrier": "buzz", "body": "body-2", "channel": "dm-1"}},
	} {
		var ref *protocol.Refusal
		if err := answer(src); !errors.As(err, &ref) || ref.Code != protocol.CodeUnshared {
			t.Fatalf("%s answer = %v, want refused unshared", name, err)
		}
	}
	if n := len(rt.Snapshot().Answers); n != 0 {
		t.Fatalf("the runtime saw %d answers from refused sources", n)
	}
	if err := answer(ownerShare); err != nil {
		t.Fatalf("owner share answer: %v", err)
	}
}
