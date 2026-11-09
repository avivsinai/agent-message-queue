package acp

import (
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/wakecmd"
)

// The owner's "/compact" in Buzz is queued for the seat's wake as exactly
// "/compact", and the owner is told it was typed once the wake answers.
func TestOwnerSlashCommandIsQueuedForTheWake(t *testing.T) {
	s, _ := machineServer(t)
	b := liveSession(t, "alpha")
	b.Harness = "claude"
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, s, b, dmChannel)
	lr := agentRelayForTest(t)
	owner := nostr.Generate()
	buzzOwner = owner.Public().Hex()
	evt := signedBy(t, owner, "/compact", dmChannel)
	lr.Inject(evt)
	saved := commandPoll
	t.Cleanup(func() { commandPoll = saved })
	commandPoll = 10 * time.Millisecond

	cmd, ok := s.ownerCommandFor(dmChannel, evt.ID.Hex(), buzzPrompt(dmChannel, buzzOwner, "/compact"))
	if !ok || cmd.refusal != "" {
		t.Fatalf("ownerCommandFor = %+v, %v; want a queued command", cmd, ok)
	}
	agentDir := fsq.AgentBase(b.Root, b.Handle)
	reqs, err := wakecmd.Pending(agentDir)
	if err != nil || len(reqs) != 1 || reqs[0].Text != "/compact" {
		t.Fatalf("queued = %+v (err %v), want exactly /compact", reqs, err)
	}
	// The wake takes it and types it.
	if !wakecmd.Claim(agentDir, reqs[0].ID) {
		t.Fatal("the wake could not claim the command")
	}
	wakecmd.Finish(agentDir, reqs[0].ID, wakecmd.Typed, "")
	if got, want := s.commandReply(dmChannel, cmd), "Typed /compact into the terminal."; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}
