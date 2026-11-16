// Package buzzwire builds the Buzz channel events a machine agent publishes
// and reads the relay-signed group state of a channel. The builders return
// unsigned events with the tag shapes of the Buzz SDK builders
// (build_create_channel, build_add_member, build_archive, build_unarchive,
// build_message, build_edit); the caller signs. Every event id and public
// key in a tag is bare lowercase hex. The package does no I/O.
package buzzwire

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

// Event kinds a machine agent publishes or reads.
const (
	KindMessage       nostr.Kind = 9
	KindAddMember     nostr.Kind = 9000
	KindEditMetadata  nostr.Kind = 9002
	KindCreateChannel nostr.Kind = 9007
	KindEdit          nostr.Kind = 40003
	KindReaction      nostr.Kind = 7
	KindGroupMetadata nostr.Kind = 39000
	KindGroupAdmins   nostr.Kind = 39001
	KindGroupMembers  nostr.Kind = 39002
)

// Publishable reports whether kind is in the set a machine agent publishes
// through its outbox: 9, 9000, 9002, 9007 and 40003. The only other kind it
// sends is the typing pulse (Typing), which has no outbox record.
func Publishable(kind nostr.Kind) bool {
	switch kind {
	case KindMessage, KindAddMember, KindEditMetadata, KindCreateChannel, KindEdit:
		return true
	}
	return false
}

// Bounds the relay enforces on what these builders produce.
const (
	maxContentBytes     = 64 * 1024
	maxChannelNameChars = 255
)

// roles are the member roles a 9000 may carry.
var roles = map[string]bool{"owner": true, "admin": true, "member": true, "guest": true, "bot": true}

// CreateChannel returns the kind 9007 event that creates the private stream
// channel id: tags h, name, visibility=private, channel_type=stream, and
// about when about is not empty. The name is trimmed as Buzz stores it.
func CreateChannel(id, name, about string, at time.Time) (nostr.Event, error) {
	if err := checkChannel(id); err != nil {
		return nostr.Event{}, err
	}
	name = strings.TrimRightFunc(strings.TrimLeftFunc(name, func(r rune) bool { return r == '#' || unicode.IsSpace(r) }), unicode.IsSpace)
	if name == "" {
		return nostr.Event{}, errors.New("channel name is required")
	}
	if utf8.RuneCountInString(name) > maxChannelNameChars {
		return nostr.Event{}, fmt.Errorf("channel name is longer than %d characters", maxChannelNameChars)
	}
	tags := nostr.Tags{{"h", id}, {"name", name}, {"visibility", "private"}, {"channel_type", "stream"}}
	if about != "" {
		tags = append(tags, nostr.Tag{"about", about})
	}
	return event(KindCreateChannel, tags, "", at), nil
}

// AddMember returns the kind 9000 event that adds pubkey to channel: tags
// h, p, and role when role is not empty.
func AddMember(channel, pubkey, role string, at time.Time) (nostr.Event, error) {
	if err := checkChannel(channel); err != nil {
		return nostr.Event{}, err
	}
	pk, err := hex64(pubkey, "member pubkey")
	if err != nil {
		return nostr.Event{}, err
	}
	tags := nostr.Tags{{"h", channel}, {"p", pk}}
	if role != "" {
		if !roles[role] {
			return nostr.Event{}, fmt.Errorf("member role %q is not owner, admin, member, guest or bot", role)
		}
		tags = append(tags, nostr.Tag{"role", role})
	}
	return event(KindAddMember, tags, "", at), nil
}

// SetArchived returns the kind 9002 event that archives (archived true) or
// unarchives (archived false) channel: tags h and archived.
func SetArchived(channel string, archived bool, at time.Time) (nostr.Event, error) {
	if err := checkChannel(channel); err != nil {
		return nostr.Event{}, err
	}
	value := "false"
	if archived {
		value = "true"
	}
	return event(KindEditMetadata, nostr.Tags{{"h", channel}, {"archived", value}}, "", at), nil
}

// KindTyping is the ephemeral typing pulse. A relay does not store it, and
// a Buzz client shows the author as working in the channel for a few
// seconds after each one.
const KindTyping nostr.Kind = 20002

// Typing returns the typing pulse of channel: empty content and the h tag
// alone, which a client shows in the channel's own timeline.
func Typing(channel string, at time.Time) (nostr.Event, error) {
	if err := checkChannel(channel); err != nil {
		return nostr.Event{}, err
	}
	return event(KindTyping, nostr.Tags{{"h", channel}}, "", at), nil
}

// Message returns the kind 9 message in channel. A non-empty replyTo adds
// the direct-reply tag ["e", replyTo, "", "reply"]; a non-empty mention then
// adds ["p", mention].
func Message(channel, content, mention, replyTo string, at time.Time) (nostr.Event, error) {
	if err := checkChannel(channel); err != nil {
		return nostr.Event{}, err
	}
	if err := checkContent(content); err != nil {
		return nostr.Event{}, err
	}
	tags := nostr.Tags{{"h", channel}}
	if replyTo != "" {
		id, err := hex64(replyTo, "reply event id")
		if err != nil {
			return nostr.Event{}, err
		}
		tags = append(tags, nostr.Tag{"e", id, "", "reply"})
	}
	if mention != "" {
		pk, err := hex64(mention, "mention pubkey")
		if err != nil {
			return nostr.Event{}, err
		}
		tags = append(tags, nostr.Tag{"p", pk})
	}
	return event(KindMessage, tags, content, at), nil
}

// Edit returns the kind 40003 event that replaces the content of eventID in
// channel: tags h and e.
func Edit(channel, eventID, content string, at time.Time) (nostr.Event, error) {
	if err := checkChannel(channel); err != nil {
		return nostr.Event{}, err
	}
	if err := checkContent(content); err != nil {
		return nostr.Event{}, err
	}
	id, err := hex64(eventID, "edit target event id")
	if err != nil {
		return nostr.Event{}, err
	}
	return event(KindEdit, nostr.Tags{{"h", channel}, {"e", id}}, content, at), nil
}

// GroupStateFilter returns the filter for the relay-signed state (39000,
// 39001, 39002) of channels, authored by the relay's own key relaySelf.
// The caller keeps each filter within the relay's request bounds.
func GroupStateFilter(relaySelf string, channels []string) (nostr.Filter, error) {
	self, err := nostr.PubKeyFromHex(relaySelf)
	if err != nil {
		return nostr.Filter{}, fmt.Errorf("relay self key: %w", err)
	}
	for _, c := range channels {
		if err := checkChannel(c); err != nil {
			return nostr.Filter{}, err
		}
	}
	return nostr.Filter{
		Kinds:   []nostr.Kind{KindGroupMetadata, KindGroupAdmins, KindGroupMembers},
		Authors: []nostr.PubKey{self},
		Tags:    nostr.TagMap{"d": channels},
	}, nil
}

// ReactionsFilter returns the filter for reactions (kind 7) on eventIDs.
func ReactionsFilter(eventIDs []string) (nostr.Filter, error) {
	ids := make([]string, len(eventIDs))
	for i, id := range eventIDs {
		h, err := hex64(id, "reaction target event id")
		if err != nil {
			return nostr.Filter{}, err
		}
		ids[i] = h
	}
	return nostr.Filter{Kinds: []nostr.Kind{KindReaction}, Tags: nostr.TagMap{"e": ids}}, nil
}

// Facts reads channel's facts from relay-signed state events: at most one
// 39000, one 39001 and one 39002 (the caller keeps the newest of each).
// Each event fills only the fields of its kind. Every event must be
// authored by relaySelf and carry the d tag channel; otherwise Facts returns
// an error and no facts. The caller passes events whose id and signature
// were verified on receipt.
func Facts(channel, relaySelf string, events ...nostr.Event) (buzzop.ChannelFacts, error) {
	f := buzzop.ChannelFacts{Channel: channel}
	seen := map[nostr.Kind]bool{}
	for _, evt := range events {
		if evt.PubKey.Hex() != relaySelf {
			return buzzop.ChannelFacts{}, fmt.Errorf("group state event %s is not authored by the relay key", evt.ID.Hex())
		}
		if d := evt.Tags.Find("d"); d == nil || d[1] != channel {
			return buzzop.ChannelFacts{}, fmt.Errorf("group state event %s is not for channel %s", evt.ID.Hex(), channel)
		}
		if seen[evt.Kind] {
			return buzzop.ChannelFacts{}, fmt.Errorf("more than one kind %d event for channel %s", evt.Kind, channel)
		}
		seen[evt.Kind] = true
		switch evt.Kind {
		case KindGroupMetadata:
			readMetadata(&f, evt)
		case KindGroupAdmins:
			f.AdminsEvent = evt.ID.Hex()
			f.Roles = map[string]string{}
			for _, t := range evt.Tags {
				if len(t) >= 3 && t[0] == "p" {
					f.Roles[t[1]] = t[2]
				}
			}
		case KindGroupMembers:
			f.MembersEvent = evt.ID.Hex()
			f.Members = []string{}
			for _, t := range evt.Tags {
				if len(t) >= 2 && t[0] == "p" {
					f.Members = append(f.Members, t[1])
				}
			}
		default:
			return buzzop.ChannelFacts{}, fmt.Errorf("event kind %d is not group state", evt.Kind)
		}
	}
	return f, nil
}

// readMetadata fills f from a 39000: private or public, archived only while
// the archived tag is true, the creator from P, and name and about.
func readMetadata(f *buzzop.ChannelFacts, evt nostr.Event) {
	f.MetaEvent = evt.ID.Hex()
	f.Archived = buzzop.TriNo
	for _, t := range evt.Tags {
		switch {
		case len(t) >= 1 && t[0] == "private":
			f.Private = buzzop.TriYes
		case len(t) >= 1 && t[0] == "public" && f.Private != buzzop.TriYes:
			f.Private = buzzop.TriNo
		case len(t) >= 2 && t[0] == "archived" && t[1] == "true":
			f.Archived = buzzop.TriYes
		case len(t) >= 2 && t[0] == "P":
			f.Creator = t[1]
		case len(t) >= 2 && t[0] == "name":
			f.Name = t[1]
		case len(t) >= 2 && t[0] == "about":
			f.About = t[1]
		}
	}
}

func event(kind nostr.Kind, tags nostr.Tags, content string, at time.Time) nostr.Event {
	return nostr.Event{Kind: kind, Tags: tags, Content: content, CreatedAt: nostr.Timestamp(at.Unix())}
}

func checkContent(content string) error {
	if len(content) > maxContentBytes {
		return fmt.Errorf("content is %d bytes, over %d", len(content), maxContentBytes)
	}
	return nil
}

// checkChannel accepts a channel id in the canonical lowercase UUID form
// Buzz writes in an h tag.
func checkChannel(id string) error {
	if len(id) != 36 {
		return fmt.Errorf("channel id %q is not a lowercase UUID", id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return fmt.Errorf("channel id %q is not a lowercase UUID", id)
			}
		default:
			if !isLowerHex(c) {
				return fmt.Errorf("channel id %q is not a lowercase UUID", id)
			}
		}
	}
	return nil
}

// hex64 returns s lowercased if it is 64 hex characters, the form of an
// event id or a public key in a tag.
func hex64(s, field string) (string, error) {
	s = strings.ToLower(s)
	if len(s) != 64 {
		return "", fmt.Errorf("%s must be 64 hex characters", field)
	}
	for i := 0; i < len(s); i++ {
		if !isLowerHex(s[i]) {
			return "", fmt.Errorf("%s must be 64 hex characters", field)
		}
	}
	return s, nil
}

func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}
