// Package buzzevent fetches one signed Buzz channel message from the relay
// by id and accepts it only when it is the plaintext message that was asked
// for, in the channel that was asked for. A caller that holds the result can
// carry the event itself as proof of who wrote it; any consumer can verify
// it again offline.
package buzzevent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
)

// KindChannelMessage is the kind of a plaintext Buzz channel message.
const KindChannelMessage nostr.Kind = 9

// channelTag names the tag that carries a channel message's channel id.
const channelTag = "h"

// Budget bounds one Fetch: connect, authenticate, request and wait.
const Budget = 2 * time.Second

var (
	// ErrNotFound means the relay reported the end of its stored events
	// without the requested event.
	ErrNotFound = errors.New("buzz event not found on the relay")
	// ErrWrongID means the relay returned an event other than the one
	// requested.
	ErrWrongID = errors.New("buzz event id differs from the requested id")
	// ErrWrongKind means the event is not a plaintext channel message.
	ErrWrongKind = errors.New("buzz event is not a plaintext channel message")
	// ErrWrongChannel means the event does not carry exactly one channel
	// tag equal to the requested channel.
	ErrWrongChannel = errors.New("buzz event is not in the requested channel")
	// ErrBadSignature means the event's signature is not its author's.
	ErrBadSignature = errors.New("buzz event signature does not verify")
)

// Fetch returns the event id from the relay cfg names, authenticated as cfg's
// identity. The event's id and signature are verified by the subscription;
// Fetch accepts it only if its id is id, its kind is KindChannelMessage, and
// it has exactly one channel tag, equal to channel. The whole call ends
// within Budget.
func Fetch(ctx context.Context, cfg relay.Config, id, channel string) (nostr.Event, error) {
	want, err := nostr.IDFromHex(id)
	if err != nil {
		return nostr.Event{}, fmt.Errorf("buzz event id: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	conn, err := relay.Connect(ctx, cfg)
	if err != nil {
		return nostr.Event{}, err
	}
	// The close handshake may outlast the budget on a stalled relay; the
	// result never waits for it.
	defer func() { go conn.Close() }()
	sub, err := conn.Subscribe(ctx, "amq-event", nostr.Filter{IDs: []nostr.ID{want}})
	if err != nil {
		return nostr.Event{}, err
	}
	select {
	case evt := <-sub.Events:
		return accept(evt, id, channel)
	case <-sub.EOSE:
		// An event the relay sent before its EOSE is already queued.
		select {
		case evt := <-sub.Events:
			return accept(evt, id, channel)
		default:
			return nostr.Event{}, ErrNotFound
		}
	case <-sub.Done():
		if err := sub.Err(); err != nil {
			return nostr.Event{}, err
		}
		return nostr.Event{}, ErrNotFound
	case <-ctx.Done():
		return nostr.Event{}, ctx.Err()
	}
}

// Verify accepts evt offline as Fetch accepts it from the relay: its id
// is the hash of its content, its author signed it, and it is the
// plaintext message in channel.
func Verify(evt nostr.Event, channel string) error {
	if !evt.CheckID() {
		return ErrWrongID
	}
	if !evt.VerifySignature() {
		return ErrBadSignature
	}
	_, err := accept(evt, evt.ID.Hex(), channel)
	return err
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

// accept returns evt only if it is the plaintext channel message id in
// channel.
func accept(evt nostr.Event, id, channel string) (nostr.Event, error) {
	if evt.ID.Hex() != id {
		return nostr.Event{}, ErrWrongID
	}
	if evt.Kind != KindChannelMessage {
		return nostr.Event{}, ErrWrongKind
	}
	var channels []string
	for _, tag := range evt.Tags {
		if len(tag) >= 2 && tag[0] == channelTag {
			channels = append(channels, tag[1])
		}
	}
	if len(channels) != 1 || channels[0] != channel {
		return nostr.Event{}, ErrWrongChannel
	}
	return evt, nil
}
