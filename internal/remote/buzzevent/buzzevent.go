// Package buzzevent verifies one signed Buzz channel message offline: its
// id is the hash of its content, its author signed it, and it is the
// plaintext message of the channel that was asked for. A caller that holds
// such an event can carry it as proof of who wrote it; any consumer can
// verify it again. DropMentions gives the text of a message without its
// leading mentions.
package buzzevent

import (
	"errors"
	"strings"
	"unicode"

	"fiatjaf.com/nostr"
)

// KindChannelMessage is the kind of a plaintext Buzz channel message.
const KindChannelMessage nostr.Kind = 9

// channelTag names the tag that carries a channel message's channel id.
const channelTag = "h"

var (
	// ErrWrongID means the event's id is not the hash of its content.
	ErrWrongID = errors.New("buzz event id differs from the requested id")
	// ErrWrongKind means the event is not a plaintext channel message.
	ErrWrongKind = errors.New("buzz event is not a plaintext channel message")
	// ErrWrongChannel means the event does not carry exactly one channel
	// tag equal to the requested channel.
	ErrWrongChannel = errors.New("buzz event is not in the requested channel")
	// ErrBadSignature means the event's signature is not its author's.
	ErrBadSignature = errors.New("buzz event signature does not verify")
)

// Verify accepts evt when its id is the hash of its content, its author
// signed it, and it is the plaintext message in channel.
func Verify(evt nostr.Event, channel string) error {
	if !evt.CheckID() {
		return ErrWrongID
	}
	if !evt.VerifySignature() {
		return ErrBadSignature
	}
	if evt.Kind != KindChannelMessage {
		return ErrWrongKind
	}
	var channels []string
	for _, tag := range evt.Tags {
		if len(tag) >= 2 && tag[0] == channelTag {
			channels = append(channels, tag[1])
		}
	}
	if len(channels) != 1 || channels[0] != channel {
		return ErrWrongChannel
	}
	return nil
}

// DropMentions trims a message's text and drops its leading mentions. Line
// breaks stay: one line can answer each question of a dialog.
func DropMentions(text string) string {
	text = strings.TrimSpace(text)
	for {
		end := strings.IndexFunc(text, unicode.IsSpace)
		if end < 0 || (!strings.HasPrefix(text, "@") && !strings.HasPrefix(text, "nostr:")) {
			return text
		}
		text = strings.TrimSpace(text[end:])
	}
}
