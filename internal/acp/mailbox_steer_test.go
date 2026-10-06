package acp

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Bead agent-message-queue-cfy: a follow-up DM during an open mailbox turn
// reaches the bound handle as an urgent steer from buzz on the turn's thread
// that refs the prompt, and the turn still ends on the final reply.
func TestFollowUpDMSteersAnOpenMailboxTurn(t *testing.T) {
	s, root := mailboxServer(t)
	thread := cockpitThread("session/s")
	turn := newTurn()
	s.sessions["s"] = &sessionState{ID: "s", Thread: thread, turn: turn}
	var steer format.Message
	var steered *steeringResult
	result, rpcErr := s.runRemote("s", "build the thing", strings.Repeat("1", 64), turn, func(any) error {
		ids := inboxPrompts(t, root)
		if steered != nil || len(ids) != 1 {
			return nil
		}
		resp, _ := s.handle([]byte(steeringRequest(7, "s", "use the smaller design")))
		if resp.Error != nil {
			t.Fatalf("steering refused: %+v", resp.Error)
		}
		got := resp.Result.(steeringResult)
		steered = &got
		var err error
		steer, err = format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "agent"), got.Meta.AMQ.MessageID+".md"))
		if err != nil {
			t.Fatal(err)
		}
		replyAs(t, root, thread, ids[0], format.KindAnswer, "done, smaller design")
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := result.(remotePromptResult); got.StopReason != StopReasonEndTurn {
		t.Fatalf("result = %+v, want end_turn", got)
	}
	if steered == nil || steered.Outcome != SteeringInjected {
		t.Fatalf("steering result = %+v, want %s", steered, SteeringInjected)
	}
	prompt := slices.DeleteFunc(inboxPrompts(t, root), func(id string) bool { return id == steer.Header.ID })
	h := steer.Header
	if h.From != mailboxSender || h.Thread != thread || h.Priority != format.PriorityUrgent || len(prompt) != 1 || !slices.Contains(h.Refs, prompt[0]) {
		t.Fatalf("steer header = %+v, want urgent from buzz on %s with refs to %v", h, thread, prompt)
	}
}
