package acp

import (
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

const dmChannel = "3f2b1c4d-0000-4000-8000-000000000001"

// agentRelayForTest serves an in-process relay that admits this agent and
// makes it the agent's relay identity for the test.
func agentRelayForTest(t *testing.T) *relaytest.Relay {
	t.Helper()
	body, err := bodykey.Mint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	grant, err := bodykey.SignAuthTag(owner, body.PublicKeyHex(), bodykey.ShareConditions(9, time.Now().Add(time.Hour).Unix()))
	if err != nil {
		t.Fatal(err)
	}
	tag := []string{"auth", grant.OwnerPubKey, grant.Conditions, grant.SigHex()}
	lr, srv, url := relaytest.Start(body.PublicKeyHex(), tag)
	t.Cleanup(srv.Close)
	old := buzzRelay
	buzzRelay = relay.Config{URL: url, Secret: body.Secret(), AuthTag: tag}
	t.Cleanup(func() { buzzRelay = old })
	return lr
}

// signedMessage is an event of kind with the given tags, signed by a fresh
// author key.
func signedMessage(t *testing.T, kind nostr.Kind, content string, tags ...nostr.Tag) nostr.Event {
	t.Helper()
	evt := nostr.Event{CreatedAt: nostr.Now(), Kind: kind, Content: content, Tags: tags}
	if err := evt.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	return evt
}

// deliverBuzzEvent publishes a prompt for eventID that arrived on channel and
// returns the message the handle received.
func deliverBuzzEvent(t *testing.T, eventID, channel string) format.Message {
	t.Helper()
	root := canonicalTempDir(t)
	if err := os.MkdirAll(fsq.AgentInboxNew(root, "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := format.NewMessageID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent"}
	if err := publishOnce(b, "t", id, eventID, signedEventContext(eventID, channel), time.Now(), "allow"); err != nil {
		t.Fatal(err)
	}
	msg, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "agent"), id+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// A Buzz DM whose event the relay serves as a plaintext message in the
// prompt's channel reaches the handle with that signed event in
// context.buzz.event, verifiable again offline.
func TestBuzzMailCarriesTheVerifiedSignedEvent(t *testing.T) {
	lr := agentRelayForTest(t)
	evt := signedMessage(t, 9, "allow", nostr.Tag{"h", dmChannel})
	lr.Inject(evt)

	msg := deliverBuzzEvent(t, evt.ID.Hex(), dmChannel)
	buzz, _ := msg.Header.Context["buzz"].(map[string]any)
	raw, err := json.Marshal(buzz["event"])
	if err != nil {
		t.Fatal(err)
	}
	var got nostr.Event
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("context.buzz.event %s: %v", raw, err)
	}
	if got.ID != evt.ID || got.PubKey != evt.PubKey || got.Content != "allow" || !got.CheckID() || !got.VerifySignature() {
		t.Fatalf("context.buzz.event = %s, want the signed event %s", raw, evt.ID.Hex())
	}
}

// A prompt whose event cannot be fetched, or is not the plaintext message
// in the prompt's channel, is still delivered, with no verified event.
func TestBuzzMailWithoutAVerifiedEventHasNoKey(t *testing.T) {
	cases := []struct {
		name string
		// stored is what the relay holds; asked is the event id the prompt
		// names, the stored one when empty.
		stored  nostr.Event
		asked   string
		offline bool
	}{
		{name: "relay unreachable", stored: signedMessage(t, 9, "allow", nostr.Tag{"h", dmChannel}), offline: true},
		{name: "another event id", stored: signedMessage(t, 9, "allow", nostr.Tag{"h", dmChannel}), asked: strings.Repeat("a", 64)},
		{name: "another channel", stored: signedMessage(t, 9, "allow", nostr.Tag{"h", "3f2b1c4d-0000-4000-8000-000000000002"})},
		{name: "an edit, not a message", stored: signedMessage(t, 40003, "allow", nostr.Tag{"h", dmChannel})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lr := agentRelayForTest(t)
			lr.Inject(tc.stored)
			if tc.offline {
				down := httptest.NewServer(nil)
				down.Close()
				buzzRelay.URL = "ws" + strings.TrimPrefix(down.URL, "http")
			}
			asked := tc.asked
			if asked == "" {
				asked = tc.stored.ID.Hex()
			}
			msg := deliverBuzzEvent(t, asked, dmChannel)
			if _, ok := msg.Header.Context["buzz"]; ok || msg.Body == "" {
				t.Fatalf("context = %v body = %q, want a delivered message with no verified event", msg.Header.Context, msg.Body)
			}
		})
	}
}

// Found live: Buzz Desktop names no event id in the ACP _meta, so no real
// DM ever carried its signed event. The triggering event's id is read from
// the prompt's <buzz-event> block, and the relay proves it.
func TestBuzzMailWithoutAMetaIDCarriesTheTriggeringEvent(t *testing.T) {
	s, root := mailboxServer(t)
	s.cfg.TurnTimeout = 300 * time.Millisecond
	lr := agentRelayForTest(t)
	evt := signedMessage(t, 9, "continue please", nostr.Tag{"h", dmChannel})
	lr.Inject(evt)
	prompt := "<context>\nScope: channel\nChannel: agent (#" + dmChannel + ")\n</context>\n" +
		"<buzz-event type=\"all\">\nEvent ID: " + evt.ID.Hex() + "\nKind: 9\nContent: continue please\n" +
		"Tags: [[\"h\",\"" + dmChannel + "\"]]\n</buzz-event>\n"
	turn := newTurn()
	turn.channel = buzzChannel(prompt)
	if _, rpcErr := s.runRemote("s", prompt, "", turn, func(any) error { return nil }); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	ids := inboxPrompts(t, root)
	if len(ids) != 1 {
		t.Fatalf("prompts = %v, want 1", ids)
	}
	msg, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, "agent"), ids[0]+".md"))
	if err != nil {
		t.Fatal(err)
	}
	buzz, _ := msg.Header.Context["buzz"].(map[string]any)
	got, _ := buzz["event"].(map[string]any)
	if got["id"] != evt.ID.Hex() || !slices.Contains(msg.Header.Labels, "nostr:"+evt.ID.Hex()) {
		t.Fatalf("context = %v labels = %v, want the triggering event %s", msg.Header.Context, msg.Header.Labels, evt.ID.Hex())
	}
}
