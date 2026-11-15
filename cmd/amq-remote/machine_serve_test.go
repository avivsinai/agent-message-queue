package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/machinecred"
)

// enrolledAgent is one enrolled machine credential whose grant names an
// in-process relay.
type enrolledAgent struct {
	relay    *relaytest.Relay
	relayKey nostr.SecretKey // the relay's self key, which signs group state
	owner    nostr.SecretKey
	home     string
	agent    string
	holder   launch.HolderIdentity
}

func enrolledMachine(t *testing.T) *enrolledAgent {
	t.Helper()
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
	f := &enrolledAgent{relay: lr, relayKey: nostr.Generate(), owner: nostr.SecretKey(owner), home: home, agent: agent, holder: holder}
	if f.owner.Public().Hex() != tag.OwnerPubKey {
		t.Fatal("the owner's signing key is not the grant's owner")
	}
	lr.Self = f.relayKey.Public().Hex()
	grant, err := machinecred.EncodeGrant(agent, url, *tag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := machinecred.Enroll(home, grant); err != nil {
		t.Fatal(err)
	}
	return f
}

// serve starts a machine set and waits until its status says serving with
// the relay authenticated.
func (f *enrolledAgent) serve(t *testing.T) (*machineSet, *bytes.Buffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var log bytes.Buffer
	ms := newMachineSet(ctx, &log)
	t.Cleanup(ms.close)
	statusPath := filepath.Join(f.home, "remote", "machine", f.agent, machineStatusFile)
	var st machineStatus
	await(t, ms, "the status to say serving with the relay authenticated", func() bool {
		raw, err := os.ReadFile(statusPath)
		return err == nil && json.Unmarshal(raw, &st) == nil && st.State == "serving" && st.RelayState == string(relay.StateAuthenticated)
	}, &log)
	return ms, &log
}

// bind attaches a mailbox session named name with handle "agent" and
// returns its AMQ root.
func (f *enrolledAgent) bind(t *testing.T, name string) string {
	t.Helper()
	root := canonicalTempDir(t)
	if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := binding.WriteNamed(binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", Holder: f.holder,
		Label: "claude · proj/" + name + " @ box", Name: name}); err != nil {
		t.Fatal(err)
	}
	return root
}

// answerGroupState is a relay hook: asked for the group state of a channel
// the agent created, the relay publishes it first, signed by its self key:
// private, owned by the agent and administered by the owner.
func (f *enrolledAgent) answerGroupState(filters []nostr.Filter) {
	stored := f.relay.Events()
	for _, filter := range filters {
		for _, ch := range filter.Tags["d"] {
			created := slices.ContainsFunc(stored, func(evt nostr.Event) bool {
				return evt.Kind == 9007 && evt.PubKey.Hex() == f.agent && evt.Tags.Find("h") != nil && evt.Tags.Find("h")[1] == ch
			})
			if !created {
				continue
			}
			agent, owner := f.agent, f.owner.Public().Hex()
			for _, st := range []struct {
				kind nostr.Kind
				tags nostr.Tags
			}{
				{39000, nostr.Tags{{"d", ch}, {"private"}, {"restricted"}, {"closed"}, {"t", "stream"}, {"P", agent}}},
				{39001, nostr.Tags{{"d", ch}, {"t", "stream"}, {"P", agent}, {"p", agent, "owner"}, {"p", owner, "admin"}}},
				{39002, nostr.Tags{{"d", ch}, {"t", "stream"}, {"P", agent}, {"p", agent, "", "owner"}, {"p", owner, "", "admin"}}},
			} {
				evt := nostr.Event{Kind: st.kind, Tags: st.tags, CreatedAt: nostr.Now()}
				if evt.Sign(f.relayKey) == nil {
					f.relay.Inject(evt)
				}
			}
		}
	}
}

// With one enrolled machine credential, serve runs the machine agent on the
// relay the grant names: its status says serving with the relay
// authenticated, and a session that attaches gets its channel created by
// the agent key.
func TestServeRunsTheEnrolledMachineAgent(t *testing.T) {
	f := enrolledMachine(t)
	ms, log := f.serve(t)
	f.bind(t, "alpha")
	await(t, ms, "the relay to hold the channel create signed by the agent key", func() bool {
		for _, evt := range f.relay.Events() {
			if evt.Kind == 9007 && evt.PubKey.Hex() == f.agent && evt.VerifySignature() {
				return true
			}
		}
		return false
	}, log)
}

// The owner's message in a session's channel reaches the session's AMQ
// inbox, the session's answer is posted in the channel, and the inbound
// cursor then stands at or after the message.
func TestServeHandsTheOwnersChannelMessageToTheSession(t *testing.T) {
	f := enrolledMachine(t)
	f.relay.BeforeReq = f.answerGroupState
	ms, log := f.serve(t)
	root := f.bind(t, "alpha")
	var channel string
	await(t, ms, "the session's channel to be routed", func() bool {
		if routes := ms.run.srv.Routes(); len(routes) == 1 {
			channel = routes[0]
		}
		return channel != ""
	}, log)

	msg := nostr.Event{Kind: 9, Tags: nostr.Tags{{"h", channel}}, Content: "what is the build status?", CreatedAt: nostr.Now()}
	if err := msg.Sign(f.owner); err != nil {
		t.Fatal(err)
	}
	f.relay.Inject(msg)
	var prompt format.Message
	inbox := fsq.AgentInboxNew(root, "agent")
	await(t, ms, "the owner's message in the session's inbox", func() bool {
		entries, _ := os.ReadDir(inbox)
		if len(entries) != 1 {
			return false
		}
		m, err := format.ReadMessageFile(filepath.Join(inbox, entries[0].Name()))
		prompt = m
		return err == nil
	}, log)
	if !strings.HasPrefix(prompt.Body, msg.Content+"\n") {
		t.Fatalf("prompt body %q; want the owner's text", prompt.Body)
	}

	now := time.Now()
	id, _ := format.NewMessageID(now)
	answer := format.Message{Header: format.Header{Schema: format.CurrentSchema, ID: id, From: "agent", To: []string{prompt.Header.From},
		Thread: prompt.Header.Thread, Subject: "re", Created: now.UTC().Format(time.RFC3339Nano), Kind: format.KindAnswer,
		Refs: []string{prompt.Header.ID}}, Body: "green on main"}
	data, err := answer.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := fsq.SnapshotDeliveryRoot(root)
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if _, err := fsq.DeliverToInboxes(dr, []string{prompt.Header.From}, id+".md", data); err != nil {
		t.Fatal(err)
	}
	await(t, ms, "the session's answer in the channel", func() bool {
		return slices.ContainsFunc(f.relay.Events(), func(evt nostr.Event) bool {
			return evt.Kind == 9 && evt.PubKey.Hex() == f.agent && evt.Content == "green on main"
		})
	}, log)
	cursor := filepath.Join(filepath.Dir(os.Getenv(binding.EnvPath)), "acp", "inbound", ms.run.id.RelayKey, f.agent+".json")
	await(t, ms, "the inbound cursor at or after the owner's message", func() bool {
		raw, err := os.ReadFile(cursor)
		var c inboundCursor
		return err == nil && json.Unmarshal(raw, &c) == nil && c.Since >= int64(msg.CreatedAt)
	}, log)
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
