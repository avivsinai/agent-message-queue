package acp

import (
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// Review of #964: a replay whose publication budget expired said not delivered
// even when the previous attempt's prompt remained in the inbox. The timeout
// must preserve uncertainty and must never invite a second send.
func TestMailboxExpiredReplayDoesNotDenyDelivery(t *testing.T) {
	s, root := mailboxServer(t)
	event := strings.Repeat("9", 64)
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent"}
	r := &remoteTurn{s: s, sessionID: "s", eventID: event, turn: newTurn()}
	if _, err := s.publishClaimed(r, time.Now().Add(time.Second), b, "cockpit/session/s", "hello"); err != nil {
		t.Fatal(err)
	}
	s.cfg.TurnTimeout = time.Nanosecond
	var said string
	result, rpcErr := s.runRemote("s", "hello", event, newTurn(), func(v any) error {
		note := v.(sessionUpdateNotification)
		if note.Params.Update.SessionUpdate == "agent_message_chunk" {
			said = note.Params.Update.Content.Text
		}
		return nil
	})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if got := result.(remotePromptResult); got.Meta.Remote.State != remoteUncertain || !strings.Contains(said, "Do not resend") || strings.Contains(said, "Not delivered") {
		t.Fatalf("expired replay result=%+v said=%q", got, said)
	}
	if prompts := inboxPrompts(t, root); len(prompts) != 1 {
		t.Fatalf("inbox prompts=%v; want the single earlier delivery", prompts)
	}
	if _, ok := s.mailboxAnswered(event); ok {
		t.Fatal("a publication timeout decided the event's final outcome")
	}
}
