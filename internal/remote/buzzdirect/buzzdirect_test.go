package buzzdirect_test

import (
	"context"
	"crypto/rand"
	"reflect"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzdirect"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/outbox"
)

// posts is the posts interface of internal/acp; the port must satisfy it.
type posts interface {
	Send(ctx context.Context, m buzzop.Message) buzzop.Result
	Edit(ctx context.Context, key buzzop.Key, channel, eventID, content string) buzzop.Result
	Reactions(ctx context.Context, eventID string) ([]buzzop.Reaction, error)
}

var _ posts = (*buzzdirect.Port)(nil)

const channel = "e4788682-3227-4e30-9e96-34684b749593"

type fixture struct {
	port     *buzzdirect.Port
	relay    *relaytest.Relay
	out      *outbox.Outbox
	live     *buzzdirect.Conn
	relayKey nostr.SecretKey
	agent    string
}

// setup starts an in-process relay, connects to it as the agent, and
// returns a port whose connection is f.live (the connection to start with).
func setup(t *testing.T) *fixture {
	t.Helper()
	body, err := bodykey.Mint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var owner [32]byte
	if _, err := rand.Read(owner[:]); err != nil {
		t.Fatal(err)
	}
	tag, err := bodykey.SignAuthTag(owner, body.PublicKeyHex(), bodykey.ShareConditions(9, time.Now().Add(time.Hour).Unix()))
	if err != nil {
		t.Fatal(err)
	}
	authTag := []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()}
	lr, srv, url := relaytest.Start(body.PublicKeyHex(), authTag)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := relay.Connect(ctx, relay.Config{URL: url, Secret: body.Secret(), AuthTag: authTag})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	out, err := outbox.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	relayKey := nostr.Generate()
	var live buzzdirect.Conn = conn
	f := &fixture{relay: lr, out: out, live: &live, relayKey: relayKey, agent: body.PublicKeyHex()}
	f.port, err = buzzdirect.New(buzzdirect.Config{
		Identity:    buzzop.Identity{Agent: body.PublicKeyHex(), RelaySelf: relayKey.Public().Hex()},
		Secret:      body.Secret(),
		Conn:        func() buzzdirect.Conn { return *f.live },
		Outbox:      out,
		ReadTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// inject stores an event signed by key on the relay.
func (f *fixture) inject(t *testing.T, key nostr.SecretKey, kind nostr.Kind, tags nostr.Tags, content string) nostr.Event {
	t.Helper()
	evt := nostr.Event{Kind: kind, Tags: tags, Content: content, CreatedAt: nostr.Now()}
	if err := evt.Sign(key); err != nil {
		t.Fatal(err)
	}
	f.relay.Inject(evt)
	return evt
}

// stored waits until the relay has stored eventID: a lookup on the same
// connection is served after the publish that stored it.
func (f *fixture) stored(t *testing.T, eventID string) {
	t.Helper()
	found, err := f.port.Lookup(context.Background(), eventID)
	if err != nil || !found {
		t.Fatalf("lookup %s: found=%v err=%v", eventID, found, err)
	}
}

func TestSendIsAcceptedOnceForAKey(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	key := buzzop.Key("send/reg1/1")

	first := f.port.Send(ctx, buzzop.Message{Key: key, Channel: channel, Content: "hello"})
	if first.Outcome != buzzop.Accepted || first.EventID == "" {
		t.Fatalf("send = %+v, want accepted with an event id", first)
	}
	f.stored(t, first.EventID)
	rec, ok, err := f.out.Get(string(key))
	if err != nil || !ok || rec.Outcome != buzzop.Accepted || rec.EventID != first.EventID {
		t.Fatalf("record = %+v ok=%v err=%v, want accepted %s", rec, ok, err, first.EventID)
	}

	again := f.port.Send(ctx, buzzop.Message{Key: key, Channel: channel, Content: "other"})
	if again.Outcome != buzzop.Accepted || again.EventID != first.EventID {
		t.Fatalf("second send = %+v, want accepted %s", again, first.EventID)
	}
	events := f.relay.Events()
	if len(events) != 1 || events[0].Kind != 9 || events[0].Tags.Find("h")[1] != channel || events[0].ID.Hex() != first.EventID {
		t.Fatalf("relay stored %+v, want one kind 9 event %s in %s", events, first.EventID, channel)
	}
}

func TestReplaySendsTheStoredEventOnceConnected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	key := buzzop.Key("send/reg1/1")
	conn := *f.live
	*f.live = nil

	held := f.port.Send(ctx, buzzop.Message{Key: key, Channel: channel, Content: "hello"})
	if held.Outcome != buzzop.NotSent || held.EventID == "" {
		t.Fatalf("send with no connection = %+v, want not_sent with the prepared event id", held)
	}
	rec, ok, err := f.out.Get(string(key))
	if err != nil || !ok || rec.Outcome != buzzop.NotSent || rec.EventID != held.EventID {
		t.Fatalf("record = %+v ok=%v err=%v, want not_sent %s", rec, ok, err, held.EventID)
	}

	*f.live = conn
	replayed := f.port.Replay(ctx, key)
	if replayed.Outcome != buzzop.Accepted || replayed.EventID != held.EventID {
		t.Fatalf("replay = %+v, want accepted %s", replayed, held.EventID)
	}
	f.stored(t, held.EventID)
}

func TestChannelFactsReadsRelaySignedState(t *testing.T) {
	f := setup(t)
	owner := nostr.Generate().Public().Hex()
	f.inject(t, f.relayKey, 39000, nostr.Tags{{"d", channel}, {"name", "amq-w0-gate-e4788682"},
		{"private"}, {"restricted"}, {"closed"}, {"t", "stream"}, {"P", f.agent}}, "")
	f.inject(t, f.relayKey, 39001, nostr.Tags{{"d", channel}, {"t", "stream"}, {"P", f.agent},
		{"p", f.agent, "owner"}, {"p", owner, "admin"}}, "")
	f.inject(t, f.relayKey, 39002, nostr.Tags{{"d", channel}, {"t", "stream"}, {"P", f.agent},
		{"p", f.agent, "", "owner"}, {"p", owner, "", "admin"}}, "")

	got, err := f.port.ChannelFacts(context.Background(), channel)
	if err != nil {
		t.Fatal(err)
	}
	if got.Private != buzzop.TriYes || got.Archived != buzzop.TriNo ||
		!reflect.DeepEqual(got.Roles, map[string]string{f.agent: "owner", owner: "admin"}) ||
		!reflect.DeepEqual(got.Members, []string{f.agent, owner}) {
		t.Fatalf("facts = %+v, want private, not archived, agent owner, owner admin, both members", got)
	}

	other := "00000000-0000-4000-8000-000000000000"
	none, err := f.port.ChannelFacts(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(none, buzzop.ChannelFacts{Channel: other}) {
		t.Fatalf("facts of a channel with no state = %+v, want all unknown", none)
	}
}

func TestReactionsGroupAuthorsByEmoji(t *testing.T) {
	f := setup(t)
	post := nostr.Generate().Public().Hex() // any 64 hex event id
	a, b := nostr.Generate(), nostr.Generate()
	f.inject(t, a, 7, nostr.Tags{{"e", post}}, "👍")
	f.inject(t, b, 7, nostr.Tags{{"e", post}}, "👍")
	// A reaction on another event that also names post in an earlier e tag
	// is not a reaction on post: the last e tag is the target. Found in the
	// lead's pre-merge review of this package (an approval must not be read
	// from a reaction on a different message).
	other := nostr.Generate().Public().Hex()
	f.inject(t, nostr.Generate(), 7, nostr.Tags{{"e", post}, {"e", other}}, "✅")

	got, err := f.port.Reactions(context.Background(), post)
	if err != nil {
		t.Fatal(err)
	}
	want := []buzzop.Reaction{{Emoji: "👍", PubKeys: []string{a.Public().Hex(), b.Public().Hex()}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reactions = %+v, want %+v", got, want)
	}
}
