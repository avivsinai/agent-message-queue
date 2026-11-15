package acp

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzdirect"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/outbox"
)

// standaloneFixture is a standalone machine agent on the direct transport,
// connected to an in-process relay.
type standaloneFixture struct {
	s        *Server
	relay    *relaytest.Relay
	relayKey nostr.SecretKey // signs the relay's group state
	owner    nostr.SecretKey
}

func standalone(t *testing.T) *standaloneFixture {
	t.Helper()
	if !lock.AdvisoryLockAvailable() {
		t.Skip("no advisory lock on this platform")
	}
	t.Setenv(binding.EnvPath, filepath.Join(canonicalTempDir(t), "binding.json"))
	saved := workerIdentity
	t.Cleanup(func() { workerIdentity = saved })
	body, err := bodykey.Mint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var grantor [32]byte
	if _, err := rand.Read(grantor[:]); err != nil {
		t.Fatal(err)
	}
	grant, err := bodykey.SignAuthTag(grantor, body.PublicKeyHex(), bodykey.ShareConditions(9, time.Now().Add(time.Hour).Unix()))
	if err != nil {
		t.Fatal(err)
	}
	authTag := []string{"auth", grant.OwnerPubKey, grant.Conditions, grant.SigHex()}
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
	f := &standaloneFixture{relay: lr, relayKey: nostr.Generate(), owner: nostr.Generate()}
	id := buzzop.Identity{Agent: body.PublicKeyHex(), Owner: f.owner.Public().Hex(), RelaySelf: f.relayKey.Public().Hex(),
		RelayURL: buzzop.NormalizeRelay(url), RelayKey: buzzop.RelayKey(url)}
	port, err := buzzdirect.New(buzzdirect.Config{Identity: id, Secret: body.Secret(),
		Conn: func() buzzdirect.Conn { return conn }, Outbox: out, ReadTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.s = NewStandalone(Standalone{Identity: id, Transport: port}, "test")
	t.Cleanup(f.s.resign)
	return f
}

// creates are the channel creates the relay stored.
func (f *standaloneFixture) creates() []nostr.Event {
	var out []nostr.Event
	for _, evt := range f.relay.Events() {
		if evt.Kind == 9007 {
			out = append(out, evt)
		}
	}
	return out
}

// groupState stores the relay-signed state of a private channel that the
// agent owns and the owner administers, as the relay publishes it.
func (f *standaloneFixture) groupState(t *testing.T, channel string) {
	t.Helper()
	agent, owner := f.s.id.Agent, f.s.id.Owner
	for _, st := range []struct {
		kind nostr.Kind
		tags nostr.Tags
	}{
		{39000, nostr.Tags{{"d", channel}, {"private"}, {"restricted"}, {"closed"}, {"t", "stream"}, {"P", agent}}},
		{39001, nostr.Tags{{"d", channel}, {"t", "stream"}, {"P", agent}, {"p", agent, "owner"}, {"p", owner, "admin"}}},
		{39002, nostr.Tags{{"d", channel}, {"t", "stream"}, {"P", agent}, {"p", agent, "", "owner"}, {"p", owner, "", "admin"}}},
	} {
		evt := nostr.Event{Kind: st.kind, Tags: st.tags, CreatedAt: nostr.Now()}
		if err := evt.Sign(f.relayKey); err != nil {
			t.Fatal(err)
		}
		f.relay.Inject(evt)
	}
}

// A session that attaches gets a channel with the id the agent chose, and
// the channel is routed only once the relay-signed state shows it private,
// owned by the agent and administered by the owner.
func TestStandaloneChannelBecomesActiveOnlyOnRelayState(t *testing.T) {
	f := standalone(t)
	liveSession(t, "alpha")
	now := time.Now()

	tick(f.s, now)
	r := channelState(t, f.s, "alpha")
	creates := f.creates()
	if len(creates) != 1 || r.Chosen == "" || creates[0].Tags.Find("h")[1] != r.Chosen || r.ChannelID != r.Chosen {
		t.Fatalf("record %+v, creates %+v; want one create of the chosen id", r, creates)
	}
	if r.State == channelActive || len(f.s.Routes()) != 0 {
		t.Fatalf("record %+v is routed before the relay showed its state", r)
	}

	tick(f.s, now.Add(time.Second))
	if r := channelState(t, f.s, "alpha"); r.State == channelActive || len(f.creates()) != 1 {
		t.Fatalf("record %+v, %d creates; want it waiting with one create", r, len(f.creates()))
	}

	f.groupState(t, r.Chosen)
	tick(f.s, now.Add(channelStepGap+time.Second))
	if r := channelState(t, f.s, "alpha"); r.State != channelActive || len(f.creates()) != 1 {
		t.Fatalf("record %+v, %d creates; want it active with one create", r, len(f.creates()))
	}
	if routes := f.s.Routes(); !slices.Equal(routes, []string{r.Chosen}) {
		t.Fatalf("routes = %v; want %s", routes, r.Chosen)
	}
}

// A dialog of a routed session is posted into its channel, and the owner's
// ✅ on that post answers it with option 1.
func TestStandaloneDialogIsAnsweredByTheOwnersReaction(t *testing.T) {
	f := standalone(t)
	b := liveSession(t, "alpha")
	activeChannel(t, f.s, b, machineChanA)
	dir, err := dialog.DirFor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	req := dialog.Request{ID: dialog.NewID(), Harness: "claude", Tool: "Bash", Text: "$ ls", OpenedAt: time.Now(),
		Options: []dialog.Option{{N: 1, Label: "Allow once", Value: "allow"}, {N: 2, Label: "Deny", Value: "deny", Deny: true}}}
	if err := dialog.Open(dir, req); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	f.s.dialogTick(now)
	p, found := readDialogPost(filepath.Join(f.s.dialogPostDir("alpha"), req.ID+".json"))
	if !found || p.Event == "" || p.Channel != machineChanA {
		t.Fatalf("dialog post = %+v (found %v); want it posted in %s", p, found, machineChanA)
	}
	reaction := nostr.Event{Kind: 7, Tags: nostr.Tags{{"h", machineChanA}, {"e", p.Event}}, Content: "✅", CreatedAt: nostr.Now()}
	if err := reaction.Sign(f.owner); err != nil {
		t.Fatal(err)
	}
	f.relay.Inject(reaction)

	f.s.dialogTick(now)
	if _, _, a, _, _ := dialog.Lookup(dir, req.ID); a == nil || a.Option != 1 {
		t.Fatalf("answer = %+v; want option 1", a)
	}
}
