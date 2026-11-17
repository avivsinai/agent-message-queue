package acp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/wakecmd"
)

// The owner's "/compact" in Buzz is queued for the seat's wake as exactly
// "/compact", and the owner is told it was typed once the wake answers.
func TestOwnerSlashCommandIsQueuedForTheWake(t *testing.T) {
	f := standalone(t)
	s := f.s
	b := liveSession(t, "alpha")
	b.Harness = "claude"
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, s, b, machineChanA)
	f.groupState(t, machineChanA)
	evt := signedBy(t, f.owner, "/compact", machineChanA)
	saved := commandPoll
	t.Cleanup(func() { commandPoll = saved })
	commandPoll = 10 * time.Millisecond
	cmd, ok := s.ownerCommandOf(f.ownerMsg(machineChanA, evt))
	if !ok || cmd.refusal != "" {
		t.Fatalf("ownerCommandFor = %+v, %v", cmd, ok)
	}
	agentDir := fsq.AgentBase(b.Root, b.Handle)
	reqs := queuedCommands(t, agentDir)
	if len(reqs) != 1 || reqs[0].Text != "/compact" || reqs[0].Target != b.SessionIdentity() {
		t.Fatalf("queued = %+v, want exactly /compact for the seat", reqs)
	}
	if err := os.WriteFile(filepath.Join(wakecmd.Dir(agentDir), reqs[0].ID+wakecmd.OutSuffix), wakecmd.OutcomeLine(wakecmd.Typed, ""), 0600); err != nil {
		t.Fatal(err)
	}
	if got, want := s.commandReply(machineChanA, cmd), "Typed /compact into the terminal."; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}

// A channel made for one session never queues an owner command for a
// later binding that took the same name.
func TestOldChannelCannotQueueCommandForReplacementSeat(t *testing.T) {
	f := standalone(t)
	s := f.s
	old := liveSession(t, "alpha")
	old.Harness = "claude"
	if err := binding.WriteNamed(old); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, s, old, machineChanA)
	f.groupState(t, machineChanA)
	replacement := liveSession(t, "alpha")
	replacement.Harness = "claude"
	if err := binding.WriteNamed(replacement); err != nil {
		t.Fatal(err)
	}
	evt := signedBy(t, f.owner, "/compact", machineChanA)
	cmd, ok := s.ownerCommandOf(f.ownerMsg(machineChanA, evt))
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
