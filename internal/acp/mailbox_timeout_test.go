package acp

import (
	"strings"
	"testing"
	"time"
)

// Regression: #964, a replay whose publication budget expired said not delivered
// even when the previous attempt's prompt remained in the inbox. The timeout
// must preserve uncertainty and must never invite a second send.
//
// On the direct transport the first attempt ends when the machine workers
// stop, which records nothing; the replay is the owner event's turn run
// again under the claim Owner wrote.
func TestMailboxExpiredReplayDoesNotDenyDelivery(t *testing.T) {
	f, b := mailboxRoute(t)
	s, root := f.s, b.Root
	in := f.ownerSays(t, machineChanA, "hello")
	event := in.Event.ID.Hex()
	stop := f.machine(t)
	s.Owner(in)
	promptOf(t, b)
	stop()
	s.cfg.TurnTimeout = time.Nanosecond
	turn := newTurn()
	turn.channel = machineChanA
	result, rpcErr := s.runRemote(routeSessionPrefix+machineChanA, in.Event.Content, event, turn, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	said := strings.Join(f.agentTexts(machineChanA), "|")
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
