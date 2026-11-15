package buzzwire

import (
	"reflect"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

// Channel, keys and tag lists of the live gate run on 2026-11-14.
const (
	gateChannel = "e4788682-3227-4e30-9e96-34684b749593"
	gateAgent   = "a0f0d8648c53ccbf209ed7441da3314624686d5758f0efc4510c48ca2ef6fea5"
	gateOwner   = "4f0604e805fc7c3fd63e1784226a141b5215b92b81d1648557130778304c6c32"
	postID      = "4e9e8eea00000000000000000000000000000000000000000000000000000000"
)

type shape struct {
	Kind      nostr.Kind
	Tags      nostr.Tags
	Content   string
	CreatedAt nostr.Timestamp
}

func TestBuildersAndParsers(t *testing.T) {
	at := time.Unix(1794689428, 0)
	ts := nostr.Timestamp(1794689428)
	relayKey := nostr.MustSecretKeyFromHex("0000000000000000000000000000000000000000000000000000000000000001").Public()
	built := func(evt nostr.Event, err error) shape {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return shape{evt.Kind, evt.Tags, evt.Content, evt.CreatedAt}
	}
	state := func(kind nostr.Kind, tags nostr.Tags) nostr.Event {
		return nostr.Event{Kind: kind, PubKey: relayKey, Tags: tags, CreatedAt: ts}
	}
	meta := nostr.Tags{{"d", gateChannel}, {"name", "amq-w0-gate-e4788682"}, {"about", "amq-w0-gate " + gateChannel},
		{"private"}, {"restricted"}, {"closed"}, {"t", "stream"}, {"P", gateAgent}, {"created_at", "1794689428"}, {"join_role", "member"}}
	admins := state(KindGroupAdmins, nostr.Tags{{"d", gateChannel}, {"t", "stream"}, {"P", gateAgent},
		{"p", gateAgent, "owner"}, {"p", gateOwner, "admin"}})
	members := state(KindGroupMembers, nostr.Tags{{"d", gateChannel}, {"t", "stream"}, {"P", gateAgent},
		{"p", gateAgent, "", "owner"}, {"p", gateOwner, "", "admin"}})
	facts := func(metaTags nostr.Tags) buzzop.ChannelFacts {
		t.Helper()
		f, err := Facts(gateChannel, relayKey.Hex(), state(KindGroupMetadata, metaTags), admins, members)
		if err != nil {
			t.Fatal(err)
		}
		f.MetaEvent, f.AdminsEvent, f.MembersEvent = "", "", ""
		return f
	}
	wantFacts := buzzop.ChannelFacts{
		Channel: gateChannel, Name: "amq-w0-gate-e4788682", About: "amq-w0-gate " + gateChannel, Creator: gateAgent,
		Private: buzzop.TriYes, Archived: buzzop.TriNo,
		Roles:   map[string]string{gateAgent: "owner", gateOwner: "admin"},
		Members: []string{gateAgent, gateOwner},
	}
	archivedFacts := wantFacts
	archivedFacts.Archived = buzzop.TriYes
	stateFilter, err := GroupStateFilter(relayKey.Hex(), []string{gateChannel})
	if err != nil {
		t.Fatal(err)
	}
	reactions, err := ReactionsFilter([]string{postID})
	if err != nil {
		t.Fatal(err)
	}
	// The e tag of an edit is 64 lowercase hex (gate run 2026-11-14: an id
	// written as id::<hex> was refused with "invalid: missing e tag for edit
	// target"); the builder refuses that form instead of signing it.
	_, idStringErr := Edit(gateChannel, "id::"+postID, "x", at)

	rows := []struct {
		name      string
		got, want any
	}{
		{"create channel", built(CreateChannel(gateChannel, "amq-w0-gate-e4788682", "amq-w0-gate", at)),
			shape{KindCreateChannel, nostr.Tags{{"h", gateChannel}, {"name", "amq-w0-gate-e4788682"}, {"visibility", "private"}, {"channel_type", "stream"}, {"about", "amq-w0-gate"}}, "", ts}},
		{"add member", built(AddMember(gateChannel, gateOwner, "admin", at)),
			shape{KindAddMember, nostr.Tags{{"h", gateChannel}, {"p", gateOwner}, {"role", "admin"}}, "", ts}},
		{"archive", built(SetArchived(gateChannel, true, at)),
			shape{KindEditMetadata, nostr.Tags{{"h", gateChannel}, {"archived", "true"}}, "", ts}},
		{"unarchive", built(SetArchived(gateChannel, false, at)),
			shape{KindEditMetadata, nostr.Tags{{"h", gateChannel}, {"archived", "false"}}, "", ts}},
		{"message reply with mention", built(Message(gateChannel, "hi", gateOwner, postID, at)),
			shape{KindMessage, nostr.Tags{{"h", gateChannel}, {"e", postID, "", "reply"}, {"p", gateOwner}}, "hi", ts}},
		{"edit", built(Edit(gateChannel, postID, "new", at)),
			shape{KindEdit, nostr.Tags{{"h", gateChannel}, {"e", postID}}, "new", ts}},
		{"edit refuses id::<hex>", idStringErr != nil, true},
		{"group state filter", stateFilter, nostr.Filter{
			Kinds:   []nostr.Kind{KindGroupMetadata, KindGroupAdmins, KindGroupMembers},
			Authors: []nostr.PubKey{relayKey}, Tags: nostr.TagMap{"d": {gateChannel}}}},
		{"reactions filter", reactions, nostr.Filter{Kinds: []nostr.Kind{KindReaction}, Tags: nostr.TagMap{"e": {postID}}}},
		{"facts", facts(meta), wantFacts},
		{"facts archived", facts(append(meta.Clone(), nostr.Tag{"archived", "true"})), archivedFacts},
	}
	for _, r := range rows {
		if !reflect.DeepEqual(r.got, r.want) {
			t.Errorf("%s:\n got %#v\nwant %#v", r.name, r.got, r.want)
		}
	}
}
