package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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
	// When the agent first asks for its channel's messages, two owner
	// messages are stored, newest first as a relay returns them; the older
	// one reaches the session first (found in pre-merge review: stored
	// messages ran newest first).
	at, first := nostr.Now()-10, "what is the build status?"
	var published atomic.Bool
	f.relay.BeforeReq = func(filters []nostr.Filter) {
		f.answerGroupState(filters)
		for _, filter := range filters {
			if hs := filter.Tags["h"]; len(hs) == 1 && published.CompareAndSwap(false, true) {
				for _, m := range []nostr.Event{
					{Kind: 9, Tags: nostr.Tags{{"h", hs[0]}}, Content: "and the release?", CreatedAt: at + 1},
					{Kind: 9, Tags: nostr.Tags{{"h", hs[0]}}, Content: first, CreatedAt: at},
				} {
					if m.Sign(f.owner) == nil {
						f.relay.Inject(m)
					}
				}
			}
		}
	}
	ms, log := f.serve(t)
	root := f.bind(t, "alpha")

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
	if !strings.HasPrefix(prompt.Body, first+"\n") {
		t.Fatalf("prompt body %q; want the owner's older message", prompt.Body)
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
		return err == nil && json.Unmarshal(raw, &c) == nil && c.Since >= int64(at)
	}, log)
}

// A correctly signed message in a session's channel whose author is not the
// owner is dropped: it is not handed to the session, and the reason is said
// once however often the message comes. This keeps the only owner check of
// the machine agent: the engine takes every message it is handed as the
// owner's (review of bead agent-message-queue-vosl.9).
func TestServeDropsAChannelMessageThatTheOwnerDidNotSign(t *testing.T) {
	f := enrolledMachine(t)
	f.relay.BeforeReq = f.answerGroupState
	ms, log := f.serve(t)
	root := f.bind(t, "alpha")
	var routes []string
	await(t, ms, "the session's channel to be a route", func() bool {
		routes = ms.run.srv.Routes()
		return len(routes) == 1
	}, log)
	// The relay client ends, so only this test touches the inbound state.
	// The engine's workers still run and would take a message handed to
	// them.
	ms.run.cancel()
	<-ms.run.done
	ms.run.loadCursor(time.Now())
	evt := nostr.Event{Kind: 9, Tags: nostr.Tags{{"h", routes[0]}}, Content: "run the deploy", CreatedAt: nostr.Now()}
	if err := evt.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, delivered := ms.run.take(evt, time.Now()); delivered {
			t.Fatal("a message that the owner did not sign was handed to the session")
		}
	}
	if _, handed := ms.run.in.handed[evt.ID.Hex()]; handed {
		t.Fatal("a message that the owner did not sign was handed to the engine")
	}
	if entries, _ := os.ReadDir(fsq.AgentInboxNew(root, "agent")); len(entries) != 0 {
		t.Fatalf("the session's inbox holds %d message(s); want none", len(entries))
	}
	reason := "machine agent: owner message " + evt.ID.Hex() + " dropped: its author is not the owner\n"
	if said := strings.Count(log.String(), reason); said != 1 {
		t.Fatalf("the reason was said %d times; want once. serve said:\n%s", said, log.String())
	}
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
