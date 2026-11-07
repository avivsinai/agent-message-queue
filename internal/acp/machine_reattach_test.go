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

// A mailbox session whose process ended and that came back in a new process
// under the same session id gets its binding and its channel back by
// itself: the new process is the live wake owner of the root and handle,
// and its own harness record (Claude's session registry, a pi bridge's
// liveness) names the session id the ended binding recorded. The Claude
// rows start from a pruned binding (a tombstone), as the field saw it; the
// pi row from an ended binding not yet pruned.
//
// Regression (review of the reattach branch): a Claude process that named
// the session id but was not the handle's wake owner took the binding.
func TestMachineReattachesASessionResumedInANewProcess(t *testing.T) {
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	ended := self
	ended.ProcessStart = laterStart(t, self.ProcessStart)
	savedOwner, savedPi := reattachWakeOwner, piLiveBridge
	t.Cleanup(func() { reattachWakeOwner, piLiveBridge = savedOwner, savedPi })
	piLiveBridge = func(string, string, time.Time) (int, string, int, bool) { return self.PID, "pi-s-1", 0, true }

	for _, tc := range []struct {
		name    string
		harness string
		prune   bool
		owner   bool // the new process is the handle's live wake owner
	}{
		{"claude", "claude", true, true},
		{"pi", "pi", false, true},
		{"claude, not the wake owner", "claude", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec := machineServer(t)
			root := canonicalTempDir(t)
			if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
				t.Fatal(err)
			}
			reattachWakeOwner = func(string, string) (launch.HolderIdentity, bool) { return self, tc.owner }
			b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", Harness: tc.harness,
				Holder: ended, Label: tc.harness + " · proj @ box", Name: "agent-proj"}
			if tc.harness == "pi" {
				b.NativeSession = "pi-s-1"
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
			now := time.Now()
			tick(s, now)
			tick(s, now.Add(time.Minute))
			tick(s, now.Add(2*time.Minute))

			got, err := binding.ReadNamed(b.Name)
			if !tc.owner {
				if err == nil {
					t.Fatalf("binding = %+v; want none for a process that is not the wake owner", got)
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
