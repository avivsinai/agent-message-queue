package acp

import (
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// A session whose process ended and that came back in a new process under
// the same session id gets its binding and its channel back by itself: the
// new process's own harness record (Claude's session registry, a pi
// bridge's liveness) names the session id the ended binding recorded. The
// Claude row starts from a pruned binding (a tombstone), as the field saw
// it; the pi row from an ended binding not yet pruned.
func TestMachineReattachesASessionResumedInANewProcess(t *testing.T) {
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	ended := self
	ended.ProcessStart = laterStart(t, self.ProcessStart)
	savedClaude, savedPi := claudeLiveSessions, piLiveBridge
	t.Cleanup(func() { claudeLiveSessions, piLiveBridge = savedClaude, savedPi })

	for _, tc := range []struct {
		name  string
		prune bool
		bound func(root string) binding.Binding
	}{
		{"claude", true, func(root string) binding.Binding {
			claudeLiveSessions = func(string) ([]claude.LiveSession, error) {
				return []claude.LiveSession{{PID: self.PID, Session: claude.Session{ID: "s-1", Cwd: root}}}, nil
			}
			return binding.Binding{Root: root, Target: "claude:1", NativeSession: "s-1", Harness: "claude",
				Identity: binding.NativeIdentity("claude", root, root), ConfigDir: root, Holder: ended,
				Label: "claude · proj @ box", Name: "claude-proj"}
		}},
		{"pi", false, func(root string) binding.Binding {
			piLiveBridge = func(string, string, time.Time) (int, string, int, bool) { return self.PID, "pi-s-1", 0, true }
			return binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", NativeSession: "pi-s-1",
				Harness: "pi", Holder: ended, Label: "pi · proj @ box", Name: "agent-proj"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec := machineServer(t)
			root := canonicalTempDir(t)
			if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
				t.Fatal(err)
			}
			b := tc.bound(root)
			name, _, err := binding.WriteSession(b, b.Name, binding.Binding.Liveness)
			if err != nil {
				t.Fatal(err)
			}
			b.Name, b.Identity = name, b.SessionIdentity()
			activeChannel(t, s, b, machineChanA)
			if tc.prune {
				if _, err := binding.PruneEnded(binding.Binding.Liveness); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			tick(s, now)
			tick(s, now.Add(time.Minute))

			got, err := binding.ReadNamed(b.Name)
			if err != nil || got.Holder != self || got.RecordedSession() != b.RecordedSession() {
				t.Fatalf("binding = %+v, %v; want the session bound to the new process", got, err)
			}
			if tc.name == "claude" && got.Target != "claude:"+strconv.Itoa(self.PID) {
				t.Fatalf("target = %q; want the new process", got.Target)
			}
			if r := channelState(t, s, b.Name); r.State != channelActive || r.ChannelID != machineChanA {
				t.Fatalf("record = %+v; want the channel restored", r)
			}
			if calls := rec.list(); !slices.Contains(calls, "channels unarchive --channel "+machineChanA) ||
				!slices.Contains(calls, "messages send --channel "+machineChanA+" --content Connected again: "+sessionAgentName(b)+".") {
				t.Fatalf("buzz calls = %q; want the channel unarchived and connected again", calls)
			}
		})
	}
}
