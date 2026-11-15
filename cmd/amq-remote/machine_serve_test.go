package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
)

// With one enrolled machine credential, serve runs the machine agent on the
// relay the grant names: its status says serving with the relay
// authenticated, and a session that attaches gets its channel created by
// the agent key.
func TestServeRunsTheEnrolledMachineAgent(t *testing.T) {
	if !lock.AdvisoryLockAvailable() {
		t.Skip("no advisory lock on this platform")
	}
	holder, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Skip("no process identity on this platform")
	}
	dir := canonicalTempDir(t)
	t.Setenv("HOME", dir)
	t.Setenv(binding.EnvPath, filepath.Join(dir, "remote", "binding.json"))
	home, err := amqhome.EnsureDir()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := machinecred.Mint(home)
	if err != nil {
		t.Fatal(err)
	}
	var owner [32]byte
	owner[31] = 7
	tag, err := bodykey.SignAuthTag(owner, agent, bodykey.MachineConditions(time.Now().Add(time.Hour).Unix()))
	if err != nil {
		t.Fatal(err)
	}
	lr, srv, url := relaytest.Start(agent, []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()})
	t.Cleanup(srv.Close)
	lr.Self = nostr.Generate().Public().Hex()
	grant, err := machinecred.EncodeGrant(agent, url, *tag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := machinecred.Enroll(home, grant); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var log bytes.Buffer
	ms := newMachineSet(ctx, &log)
	t.Cleanup(ms.close)
	statusPath := filepath.Join(home, "remote", "machine", agent, machineStatusFile)
	var st machineStatus
	await(t, ms, "the status to say serving with the relay authenticated", func() bool {
		raw, err := os.ReadFile(statusPath)
		return err == nil && json.Unmarshal(raw, &st) == nil && st.State == "serving" && st.RelayState == string(relay.StateAuthenticated)
	}, &log)

	root := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", Holder: holder,
		Label: "claude · proj/alpha @ box", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	await(t, ms, "the relay to hold the channel create signed by the agent key", func() bool {
		for _, evt := range lr.Events() {
			if evt.Kind == 9007 && evt.PubKey.Hex() == agent && evt.VerifySignature() {
				return true
			}
		}
		return false
	}, &log)
}

// await ticks ms until cond holds, for at most 10 s.
func await(t *testing.T, ms *machineSet, what string, cond func() bool, log *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; serve said:\n%s", what, log.String())
		}
		ms.tick(time.Now())
		time.Sleep(20 * time.Millisecond)
	}
}
