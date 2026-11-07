package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// A session whose process ended and that came back in a new process under
// the same session id gets its binding and its channel back by itself: for
// pi, the pi bridge of its root and handle names the session id the ended
// binding recorded; for a Claude mailbox seat, the live wake owner of its
// root and handle has a registry entry that names it. One pi row starts
// from a pruned binding (a tombstone), as the field saw it; the other from
// an ended binding not yet pruned.
//
// Regression (review of the reattach branch): a Claude process whose
// registry entry named the session id took a mailbox binding without proof
// that it ran as that handle at that root. Only the wake owner is rebound.
// Regression (round-2 review of the reattach branch): /amq-remote off after
// the binding was pruned matched nothing, so a pi resume came back.
func TestMachineReattachesASessionResumedInANewProcess(t *testing.T) {
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	ended := self
	ended.ProcessStart = laterStart(t, self.ProcessStart)
	other, err := launch.HolderOf(os.Getppid())
	if err != nil {
		t.Skip("no parent process identity")
	}
	savedPi, savedOwner := piLiveBridge, reattachWakeOwner
	t.Cleanup(func() { piLiveBridge, reattachWakeOwner = savedPi, savedOwner })
	piLiveBridge = func(string, string, time.Time) (int, string, int, bool) { return self.PID, "pi-s-1", 0, true }

	for _, tc := range []struct {
		name     string
		harness  string
		prune    bool
		off      bool // the owner turns the session off after the prune
		notOwner bool // another process is the handle's wake owner
		want     bool
	}{
		{"pi", "pi", false, false, false, true},
		{"pi, pruned", "pi", true, false, false, true},
		{"pi, pruned, then off", "pi", true, true, false, false},
		{"claude", "claude", true, false, false, true},
		{"claude, not the wake owner", "claude", true, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := self
			if tc.notOwner {
				owner = other
			}
			reattachWakeOwner = func(string, string) (launch.HolderIdentity, bool) { return owner, true }
			s, rec := machineServer(t)
			root := canonicalTempDir(t)
			if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
				t.Fatal(err)
			}
			b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", Harness: tc.harness,
				Holder: ended, Label: tc.harness + " · proj @ box", Name: "agent-proj"}
			if tc.harness == "pi" {
				b.HarnessSession = "pi-s-1"
			} else {
				b.ConfigDir = canonicalTempDir(t)
				b.Transcript = claudeSessionFiles(t, b.ConfigDir, self.PID, "s-1")
			}
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
			if tc.off {
				same := func(o binding.Binding) bool { return o.Name == b.Name }
				if _, _, err := binding.Detach(same, same); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			tick(s, now)
			tick(s, now.Add(time.Minute))
			tick(s, now.Add(2*time.Minute))

			got, err := binding.ReadNamed(b.Name)
			if !tc.want {
				if err == nil {
					t.Fatalf("binding = %+v; want none", got)
				}
				return
			}
			if err != nil || got.Holder != self || got.RecordedSession() != b.RecordedSession() {
				t.Fatalf("binding = %+v, %v; want the session bound to the new process", got, err)
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

// claudeSessionFiles writes the Claude session registry entry of pid, which
// names session id, and its transcript under configDir; it returns the
// transcript.
func claudeSessionFiles(t *testing.T, configDir string, pid int, id string) string {
	t.Helper()
	transcript := filepath.Join(configDir, "projects", "-w", id+".jsonl")
	raw, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": id, "cwd": "/w", "updatedAt": time.Now().UnixMilli()})
	for path, data := range map[string][]byte{filepath.Join(configDir, "sessions", strconv.Itoa(pid)+".json"): raw, transcript: nil} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return transcript
}
