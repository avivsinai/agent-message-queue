package acp

import (
	"os"
	"path/filepath"
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
	reqs := queuedCommands(t, agentDir)
	if len(reqs) != 1 || reqs[0].Text != "/compact" || reqs[0].Target != b.SessionIdentity() {
		t.Fatalf("queued = %+v, want exactly /compact for the seat", reqs)
	}
	// The wake takes it and types it.
	if err := os.WriteFile(filepath.Join(wakecmd.Dir(agentDir), reqs[0].ID+wakecmd.OutSuffix), wakecmd.OutcomeLine(wakecmd.Typed, ""), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := s.commandReply(dmChannel, cmd), "Typed /compact into the terminal."; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}

// Review of Buzz slash commands: a channel made for one session never
// queues a command for a later binding of the same name.
func TestOldChannelCannotQueueCommandForReplacementSeat(t *testing.T) {
	s, _ := machineServer(t)
	old := liveSession(t, "alpha")
	old.Harness = "claude"
	if err := binding.WriteNamed(old); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, s, old, dmChannel)
	replacement := liveSession(t, "alpha")
	replacement.Harness = "claude"
	if err := binding.WriteNamed(replacement); err != nil {
		t.Fatal(err)
	}
	lr := agentRelayForTest(t)
	owner := nostr.Generate()
	buzzOwner = owner.Public().Hex()
	evt := signedBy(t, owner, "/compact", dmChannel)
	lr.Inject(evt)

	cmd, ok := s.ownerCommandFor(dmChannel, evt.ID.Hex(), buzzPrompt(dmChannel, buzzOwner, "/compact"))
	if queued := queuedCommands(t, fsq.AgentBase(replacement.Root, replacement.Handle)); len(queued) != 0 {
		t.Fatalf("the old channel queued for the replacement seat: ok=%v refusal=%q queue=%+v", ok, cmd.refusal, queued)
	}
}

// queuedCommands are the requests in agentDir's command queue.
func queuedCommands(t *testing.T, agentDir string) []wakecmd.Request {
	t.Helper()
	entries, err := os.ReadDir(wakecmd.Dir(agentDir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var reqs []wakecmd.Request
	for _, e := range entries {
		id, ok := wakecmd.RequestID(e.Name())
		if !ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(wakecmd.Dir(agentDir), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		req, err := wakecmd.Parse(id, raw)
		if err != nil {
			t.Fatal(err)
		}
		reqs = append(reqs, req)
	}
	return reqs
}
