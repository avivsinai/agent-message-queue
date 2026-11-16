package acp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/relay/relaytest"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzdirect"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
	"github.com/avivsinai/agent-message-queue/internal/remote/mirror"
	"github.com/avivsinai/agent-message-queue/internal/remote/outbox"
)

// standaloneFixture is a standalone machine agent on the direct transport,
// connected to an in-process relay.
type standaloneFixture struct {
	s        *Server
	relay    *relaytest.Relay
	relayKey nostr.SecretKey // signs the relay's group state
	owner    nostr.SecretKey
	out      *outbox.Outbox
	// reconnect uses a new transport for the same relay and outbox, as a
	// client that comes back after a connection drops.
	reconnect func() Transport
}

func standalone(t *testing.T, opts ...func(*relaytest.Relay)) *standaloneFixture {
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
	for _, opt := range opts {
		opt(lr)
	}
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
	f := &standaloneFixture{relay: lr, relayKey: nostr.Generate(), owner: nostr.Generate(), out: out}
	id := buzzop.Identity{Agent: body.PublicKeyHex(), Owner: f.owner.Public().Hex(), RelaySelf: f.relayKey.Public().Hex(),
		RelayURL: buzzop.NormalizeRelay(url), RelayKey: buzzop.RelayKey(url)}
	port, err := buzzdirect.New(buzzdirect.Config{Identity: id, Secret: body.Secret(),
		Conn: func() buzzdirect.Conn { return conn }, Outbox: out, ReadTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.s = NewStandalone(Standalone{Identity: id, Transport: port}, "test")
	f.reconnect = func() Transport {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		next, err := relay.Connect(ctx, relay.Config{URL: id.RelayURL, Secret: body.Secret(), AuthTag: authTag})
		t.Cleanup(next.Close)
		if err != nil {
			t.Fatal(err)
		}
		nextPort, err := buzzdirect.New(buzzdirect.Config{Identity: id, Secret: body.Secret(),
			Conn: func() buzzdirect.Conn { return next }, Outbox: out, ReadTimeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		f.s.direct, f.s.posts = nextPort, nextPort
		return nextPort
	}
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

// ownerSays signs text as the owner's message in channel.
func (f *standaloneFixture) ownerSays(t *testing.T, channel, text string) buzzop.Inbound {
	t.Helper()
	evt := nostr.Event{Kind: 9, Tags: nostr.Tags{{"h", channel}}, Content: text, CreatedAt: nostr.Now()}
	if err := evt.Sign(f.owner); err != nil {
		t.Fatal(err)
	}
	return buzzop.Inbound{Channel: channel, Event: evt}
}

// posts are the agent's messages in channel whose content is text.
func (f *standaloneFixture) posts(channel, text string) int {
	n := 0
	for _, evt := range f.relay.Events() {
		if evt.Kind == 9 && evt.PubKey.Hex() == f.s.id.Agent && evt.Tags.Find("h")[1] == channel && evt.Content == text {
			n++
		}
	}
	return n
}

// waitClosed fails the test unless done closes within 5 s.
func waitClosed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the owner message was not handled within 5 s")
	}
}

// The owner's message in a session's channel reaches the session's AMQ
// inbox with its signed event, and the session's reply is posted in the
// channel.
func TestStandaloneOwnerMessageReachesTheSessionAndItsReplyIsPosted(t *testing.T) {
	f := standalone(t)
	b := liveSession(t, "alpha")
	activeChannel(t, f.s, b, machineChanA)
	stop, ok := f.s.StartMachine()
	if !ok {
		t.Fatal("the machine agent did not start")
	}
	t.Cleanup(stop)
	in := f.ownerSays(t, machineChanA, "what is the build status?")

	done := f.s.Owner(in)
	var ids []string
	for deadline := time.Now().Add(5 * time.Second); len(ids) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		ids = inboxPrompts(t, b.Root)
	}
	if len(ids) != 1 {
		t.Fatalf("inbox prompts = %v; want one", ids)
	}
	prompt, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(b.Root, "agent"), ids[0]+".md"))
	if err != nil {
		t.Fatal(err)
	}
	signed, _ := json.Marshal(prompt.Header.Context)
	if prompt.Header.From != mailboxSender || !strings.HasPrefix(prompt.Body, in.Event.Content+"\n") || !strings.Contains(string(signed), in.Event.ID.Hex()) {
		t.Fatalf("prompt from %s, context %s, body %q; want the owner's text and signed event from %s", prompt.Header.From, signed, prompt.Body, mailboxSender)
	}
	// While the session works on the answer, the channel shows the agent
	// as working: a typing pulse with the channel's h tag alone.
	pulsed := func() bool {
		return slices.ContainsFunc(f.relay.Events(), func(evt nostr.Event) bool {
			return evt.Kind == 20002 && evt.PubKey.Hex() == f.s.id.Agent && evt.Content == "" &&
				len(evt.Tags) == 1 && evt.Tags[0][0] == "h" && evt.Tags[0][1] == machineChanA
		})
	}
	for deadline := time.Now().Add(5 * time.Second); !pulsed() && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if !pulsed() {
		t.Fatal("no typing pulse in the channel while the turn ran")
	}
	replyAs(t, b.Root, prompt.Header.Thread, ids[0], format.KindAnswer, "green on main")
	waitClosed(t, done)
	if n := f.posts(machineChanA, "green on main"); n != 1 {
		t.Fatalf("%d posts of the reply in the channel; want 1", n)
	}
}

// The owner's "mirror off" in a mirrored session's channel switches the
// mirror off and is acknowledged once; the same message again does nothing.
func TestStandaloneOwnerMirrorOffIsAcknowledgedOnce(t *testing.T) {
	f := standalone(t)
	b := liveSession(t, "alpha")
	b.Harness = "claude"
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, f.s, b, machineChanA)
	stop, ok := f.s.StartMachine()
	if !ok {
		t.Fatal("the machine agent did not start")
	}
	t.Cleanup(stop)
	in := f.ownerSays(t, machineChanA, "mirror off")

	waitClosed(t, f.s.Owner(in))
	waitClosed(t, f.s.Owner(in))
	if dir, err := mirror.DirFor("alpha"); err != nil || !mirror.Off(dir) {
		t.Fatalf("the mirror is not off (err %v)", err)
	}
	if n := f.posts(machineChanA, mirrorOffReply); n != 1 {
		t.Fatalf("%d acknowledgements; want 1", n)
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

// Archive, public, ownership change and an owner who left are failures
// that relay-signed state must show before they suspend the route.
func TestJudgeRoute(t *testing.T) {
	id := buzzop.Identity{Agent: "agent", Owner: "owner"}
	sound := buzzop.ChannelFacts{Private: buzzop.TriYes, Archived: buzzop.TriNo,
		AdminsEvent: "x", Roles: map[string]string{"agent": "owner", "owner": "admin"}}
	for _, tc := range []struct {
		name   string
		change func(*buzzop.ChannelFacts)
		fault  string
		sound  bool
	}{
		{"sound", func(f *buzzop.ChannelFacts) {}, "", true},
		{"archived", func(f *buzzop.ChannelFacts) { f.Archived = buzzop.TriYes }, "the channel is archived", false},
		{"public", func(f *buzzop.ChannelFacts) { f.Private = buzzop.TriNo }, "the channel is not private", false},
		{"agent not owner", func(f *buzzop.ChannelFacts) { f.Roles["agent"] = "admin" }, "the machine agent is not the channel's owner", false},
		{"owner left", func(f *buzzop.ChannelFacts) { delete(f.Roles, "owner") }, "the owner is not an admin of the channel", false},
		{"no admin state read", func(f *buzzop.ChannelFacts) { f.AdminsEvent, f.Roles = "", nil }, "", false},
		{"no facts", func(f *buzzop.ChannelFacts) { *f = buzzop.ChannelFacts{} }, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := sound
			facts.Roles = map[string]string{}
			for k, v := range sound.Roles {
				facts.Roles[k] = v
			}
			tc.change(&facts)
			got, ready := judgeRoute(facts, id)
			if got != tc.fault || ready != tc.sound {
				t.Fatalf("judgeRoute = %q, %v; want %q, %v", got, ready, tc.fault, tc.sound)
			}
		})
	}
}

// The real relay state suspends an archived channel and unarchive alone
// returns the same record to service. Unknown state never makes the route
// a failure.
func TestOwnerArchiveSuspendsTheRouteAndUnarchiveReturnsIt(t *testing.T) {
	f := standalone(t)
	b := liveSession(t, "alpha")
	activeChannel(t, f.s, b, machineChanA)
	f.groupState(t, machineChanA)
	archived := nostr.Event{Kind: 39000, Tags: nostr.Tags{{"d", machineChanA}, {"archived", "true"}, {"private"}, {"P", f.s.id.Agent}}, CreatedAt: nostr.Now() + 1}
	if err := archived.Sign(f.relayKey); err != nil {
		t.Fatal(err)
	}
	f.relay.Inject(archived)
	f.s.judgeRoutes(time.Now())
	r := channelState(t, f.s, "alpha")
	if r.Suspended != "the channel is archived" {
		t.Fatalf("stored suspension = %+v", r)
	}
	if got := f.s.Routes(); len(got) != 0 || len(f.s.machineTargets()) != 0 {
		t.Fatalf("suspended route still routed: %v / %+v", got, f.s.machineTargets())
	}
	unarchived := nostr.Event{Kind: 39000, Tags: nostr.Tags{{"d", machineChanA}, {"private"}, {"restricted"}, {"closed"}, {"P", f.s.id.Agent}}, CreatedAt: nostr.Now() + 2}
	if err := unarchived.Sign(f.relayKey); err != nil {
		t.Fatal(err)
	}
	f.relay.Inject(unarchived)
	f.s.judgeRoutes(time.Now().Add(time.Second))
	r = channelState(t, f.s, "alpha")
	if r.Suspended != "" || r.Resumed == "" {
		t.Fatalf("record after unarchive = %+v; want resumed", r)
	}
	if got := f.s.Routes(); !slices.Equal(got, []string{machineChanA}) {
		t.Fatalf("routes after unarchive = %v", got)
	}
}

// ownerSignedAt signs text as the owner at at, with optional thread tags.
func (f *standaloneFixture) ownerSignedAt(t *testing.T, channel, content string, at time.Time, tags ...nostr.Tag) nostr.Event {
	t.Helper()
	evt := nostr.Event{Kind: 9, Tags: append(nostr.Tags{{"h", channel}}, tags...), Content: content,
		CreatedAt: nostr.Timestamp(at.Unix())}
	if err := evt.Sign(f.owner); err != nil {
		t.Fatal(err)
	}
	return evt
}

// ownerReacts injects an owner-signed reaction of emoji on eventID.
func (f *standaloneFixture) ownerReacts(t *testing.T, channel string, eventID nostr.ID, emoji string) {
	t.Helper()
	evt := nostr.Event{Kind: 7, Tags: nostr.Tags{{"h", channel}, {"e", eventID.Hex()}}, Content: emoji, CreatedAt: nostr.Now()}
	if err := evt.Sign(f.owner); err != nil {
		t.Fatal(err)
	}
	f.relay.Inject(evt)
}

// agentMessages are the agent's ordinary channel messages in stored order.
func (f *standaloneFixture) agentMessages(channel string) []nostr.Event {
	var out []nostr.Event
	for _, evt := range f.relay.Events() {
		if evt.Kind == 9 && evt.PubKey.Hex() == f.s.id.Agent {
			if h := evt.Tags.Find("h"); len(h) > 1 && h[1] == channel {
				out = append(out, evt)
			}
		}
	}
	return out
}

// agentEdits are the agent's edits of eventID in stored order.
func (f *standaloneFixture) agentEdits(channel string, eventID nostr.ID) []nostr.Event {
	var out []nostr.Event
	for _, evt := range f.relay.Events() {
		if evt.Kind != 40003 || evt.PubKey.Hex() != f.s.id.Agent {
			continue
		}
		if h, e := evt.Tags.Find("h"), evt.Tags.Find("e"); len(h) > 1 && h[1] == channel && len(e) > 1 && e[1] == eventID.Hex() {
			out = append(out, evt)
		}
	}
	return out
}

// lastAgentPost is the content of the agent's last channel message.
func (f *standaloneFixture) lastAgentPost(channel string) string {
	messages := f.agentMessages(channel)
	if len(messages) == 0 {
		return ""
	}
	return messages[len(messages)-1].Content
}

// startMachine starts this fixture's workers and stops them on cleanup.
func (f *standaloneFixture) startMachine(t *testing.T) {
	t.Helper()
	stop, ok := f.s.StartMachine()
	if !ok {
		t.Fatal("the machine agent did not start")
	}
	t.Cleanup(stop)
}

// ownerMsg is the selection of a direct, signed owner event. Another
// signature cannot become the owner through paraphrased text.
func (f *standaloneFixture) ownerMsg(channel, text string, evt nostr.Event) ownerMsg {
	return ownerMsg{channel: channel, words: dropMentions(text), verified: func() (nostr.Event, error) {
		if evt.PubKey.Hex() != f.s.id.Owner {
			return nostr.Event{}, errors.New("not a verified owner event")
		}
		return evt, nil
	}}
}

// signedBy is a plaintext channel message with any tags a test gives it.
// eventMeta on the direct path supplies bytes through the relay, not text.
func signedBy(t *testing.T, key nostr.SecretKey, content, channel string, tags ...nostr.Tag) nostr.Event {
	t.Helper()
	evt := nostr.Event{CreatedAt: nostr.Now(), Kind: 9, Content: content, Tags: append(nostr.Tags{{"h", channel}}, tags...)}
	if err := evt.Sign(key); err != nil {
		t.Fatal(err)
	}
	return evt
}

// writeTag is the value of evt's first tag called name, or "".
func writeTag(evt nostr.Event, name string) string {
	if tag := evt.Tags.Find(name); len(tag) > 1 {
		return tag[1]
	}
	return ""
}

// relayState stores, for an accepted channel write of the agent, the
// relay-signed state that the write changes, as a Buzz relay does: a create
// makes the channel private with the agent as its creator and owner, an
// add-member seats the member with its role, and an archive or unarchive
// sets the archived flag. Each new state event is newer than the last one
// of its kind for the channel.
func (f *standaloneFixture) relayState(t *testing.T, evt nostr.Event) {
	channel := writeTag(evt, "h")
	if channel == "" {
		return
	}
	latest := map[nostr.Kind]nostr.Event{}
	for _, e := range f.relay.Events() {
		if e.PubKey != f.relayKey.Public() || writeTag(e, "d") != channel {
			continue
		}
		if cur, ok := latest[e.Kind]; !ok || e.CreatedAt > cur.CreatedAt || (e.CreatedAt == cur.CreatedAt && e.ID.Hex() < cur.ID.Hex()) {
			latest[e.Kind] = e
		}
	}
	// keep is the tags of the latest event of kind, without those drop names.
	keep := func(kind nostr.Kind, drop func(nostr.Tag) bool) nostr.Tags {
		tags := nostr.Tags{{"d", channel}}
		for _, tag := range latest[kind].Tags {
			if len(tag) > 0 && tag[0] != "d" && !drop(tag) {
				tags = append(tags, tag)
			}
		}
		return tags
	}
	store := func(kind nostr.Kind, tags nostr.Tags) {
		at := nostr.Now()
		if prev, ok := latest[kind]; ok && prev.CreatedAt >= at {
			at = prev.CreatedAt + 1
		}
		state := nostr.Event{Kind: kind, Tags: tags, CreatedAt: at}
		if err := state.Sign(f.relayKey); err != nil {
			t.Error(err)
			return
		}
		f.relay.Inject(state)
	}
	agent := f.s.id.Agent
	switch evt.Kind {
	case 9007:
		store(39000, nostr.Tags{{"d", channel}, {"private"}, {"restricted"}, {"closed"}, {"t", "stream"}, {"P", agent}})
		store(39001, nostr.Tags{{"d", channel}, {"t", "stream"}, {"P", agent}, {"p", agent, "owner"}})
		store(39002, nostr.Tags{{"d", channel}, {"t", "stream"}, {"P", agent}, {"p", agent, "", "owner"}})
	case 9000:
		member, role := writeTag(evt, "p"), writeTag(evt, "role")
		same := func(tag nostr.Tag) bool { return len(tag) > 1 && tag[0] == "p" && tag[1] == member }
		store(39001, append(keep(39001, same), nostr.Tag{"p", member, role}))
		store(39002, append(keep(39002, same), nostr.Tag{"p", member, "", role}))
	case 9002:
		tags := keep(39000, func(tag nostr.Tag) bool { return tag[0] == "archived" })
		if writeTag(evt, "archived") == "true" {
			tags = append(tags, nostr.Tag{"archived", "true"})
		}
		store(39000, tags)
	}
}

// reconnectAll moves every operation of the agent, its channel lifecycle
// too, to a new connection to the same relay and outbox.
func (f *standaloneFixture) reconnectAll() {
	next := f.reconnect()
	f.s.buzz = directOps{s: f.s, t: next}
}
