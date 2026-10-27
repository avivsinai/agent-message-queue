package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// writeMarker writes one amq-acp marker under the agent directory dirName.
func writeMarker(t *testing.T, dir, dirName string, marker map[string]any) {
	t.Helper()
	markers := filepath.Join(dir, "remote", "acp", "agents", dirName)
	if err := os.MkdirAll(markers, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markers, marker["pubkey"].(string)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// attach reports the session's channel: while the machine agent runs and
// has made the channel, the JSON names it so the owner opens it.
func TestAttachReportsTheMachineChannel(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	root := filepath.Join(dir, "p", ".agent-mail", "session3")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	pub, relayKey := strings.Repeat("e", 64), strings.Repeat("f", 64)
	writeMarker(t, dir, machineMarkerDir, map[string]any{"name": "_machine", "pubkey": pub, "display_name": "AMQ Remote @ box",
		"relay_key": relayKey, "worker": self, "updated": time.Now().UTC().Format(time.RFC3339)})
	records := filepath.Join(dir, "remote", "acp", "channels", relayKey, pub)
	if err := os.MkdirAll(records, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "claude"}.SessionIdentity()
	record, _ := json.Marshal(map[string]any{"name": "claude-p-session3", "identity": identity, "label": "claude · p/session3 @ box",
		"reg": "r1", "state": "active", "channel_id": "6eff60e4-32ab-48ec-bd3d-f4c97872f370"})
	if err := os.WriteFile(filepath.Join(records, "claude-p-session3.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	pinMailboxSession(t, root)
	var out bytes.Buffer
	if code, err := attach([]string{"--self", "--json"}, &out, io.Discard); code != 0 || err != nil {
		t.Fatalf("attach: code=%d err=%v", code, err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["machine"] != "running" || got["channel"] != "active" || got["channel_name"] != "claude · p/session3 @ box" {
		t.Fatalf("attach JSON = %v", got)
	}

	// Regression (PR #1279 review): with the machine agent stopped, an
	// uncertain or failed channel told the owner that AMQ keeps looking or
	// tries again, although no worker runs. Starting the agent comes first.
	for _, ch := range []acp.MachineChannel{{State: "uncertain", Error: "relay timeout"}, {State: "failed", Error: "refused", Retry: true}} {
		var text bytes.Buffer
		connected(&text, attachView{name: "claude-p-session3", label: "claude · p/session3 @ box", agent: agentInfo{State: agentMissing},
			machine: agentInfo{State: agentOffline, Display: "AMQ Remote @ box"}, served: true, channel: &ch})
		if out := text.String(); !strings.Contains(out, `Start "AMQ Remote @ box"`) || strings.Contains(out, "keeps looking") || strings.Contains(out, "tries again") {
			t.Fatalf("%s channel with the machine agent offline: %q", ch.State, out)
		}
	}
}

// Regression (D2): buzz-acp tears an idle lazy agent's worker down after
// 900 s, and its marker went stale, so status read the agent as offline and
// told the owner to start it. A stale marker whose recorded parent buzz-acp
// still runs is idle: a DM starts it.
func TestStaleMarkerWithALiveParentReadsIdle(t *testing.T) {
	dir := canonicalTempDir(t)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	parent, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	worker := parent
	worker.PID = 1 << 30 // no such process: the worker is gone
	writeMarker(t, dir, "alpha", map[string]any{"name": "alpha", "pubkey": "ab", "worker": worker, "parent": parent,
		"updated": time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)})
	if got := agentOf("alpha", time.Now()).State; got != agentIdle {
		t.Fatalf("agent = %s, want idle", got)
	}
}
