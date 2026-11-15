// Package buzzdirect is the Buzz operations of a machine agent over the
// relay itself: it builds each event with buzzwire, signs it with the agent
// key, keeps it in the outbox, and publishes it on an authenticated relay
// connection. It has the method set of the buzz CLI port. It does not retry,
// hold subscriptions open, or decide what a channel's state means.
package buzzdirect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzwire"
	"github.com/avivsinai/agent-message-queue/internal/remote/outbox"
)

// Conn is the part of a relay connection the port uses. *relay.Conn
// satisfies it.
type Conn interface {
	Publish(ctx context.Context, evt nostr.Event) error
	Subscribe(ctx context.Context, id string, filters ...nostr.Filter) (*relay.Sub, error)
}

// Config is what one port needs.
type Config struct {
	// Identity: Agent and RelaySelf are used.
	Identity buzzop.Identity
	// Secret is the agent key's secret. It signs every event and is never
	// logged or put in an error.
	Secret [32]byte
	// Conn returns the live authenticated connection, or nil when there is
	// none.
	Conn   func() Conn
	Outbox *outbox.Outbox
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// ReadTimeout bounds one read to EOSE; zero means defaultReadTimeout.
	ReadTimeout time.Duration
}

const (
	defaultReadTimeout = 10 * time.Second
	// retryGap is how long after an unknown or rejected attempt the next
	// one is due. The port only records it; the caller decides when to
	// replay.
	retryGap = 30 * time.Second
)

// subSeq numbers this process's subscriptions, so the ids stay distinct on
// a connection other users share.
var subSeq atomic.Uint64

// Port is the Buzz operations over one relay connection and one outbox.
type Port struct {
	id          buzzop.Identity
	secret      [32]byte
	conn        func() Conn
	out         *outbox.Outbox
	now         func() time.Time
	readTimeout time.Duration
}

// New returns a port. It refuses a config without the agent's secret, an
// agent public key that is the secret's own, a canonical relay self key, a
// connection getter or an outbox.
func New(cfg Config) (*Port, error) {
	switch {
	case cfg.Secret == [32]byte{}:
		return nil, errors.New("buzzdirect: the agent key's secret is required")
	case cfg.Identity.Agent == "" || nostr.GetPublicKey(cfg.Secret).Hex() != cfg.Identity.Agent:
		return nil, errors.New("buzzdirect: the agent public key must be the public key of the agent secret")
	case cfg.Conn == nil:
		return nil, errors.New("buzzdirect: a relay connection getter is required")
	case cfg.Outbox == nil:
		return nil, errors.New("buzzdirect: an outbox is required")
	}
	if self, err := nostr.PubKeyFromHex(cfg.Identity.RelaySelf); err != nil || self.Hex() != cfg.Identity.RelaySelf {
		return nil, errors.New("buzzdirect: the relay self key must be 64 lowercase hex")
	}
	p := &Port{id: cfg.Identity, secret: cfg.Secret, conn: cfg.Conn, out: cfg.Outbox, now: cfg.Now, readTimeout: cfg.ReadTimeout}
	if p.now == nil {
		p.now = time.Now
	}
	if p.readTimeout <= 0 {
		p.readTimeout = defaultReadTimeout
	}
	return p, nil
}

// CreateChannel creates the private stream channel spec.ID. The caller
// chooses and stores the id before the call, so an empty ID is NotSent.
// ChannelID is spec.ID when the create is Accepted.
func (p *Port) CreateChannel(ctx context.Context, spec buzzop.ChannelSpec) buzzop.Created {
	if spec.ID == "" {
		return buzzop.Created{Result: notSent("create channel: a chosen channel id is required")}
	}
	r := p.write(ctx, spec.Key, func(at time.Time) (nostr.Event, error) {
		return buzzwire.CreateChannel(spec.ID, spec.Name, spec.About, at)
	})
	c := buzzop.Created{Result: r}
	if r.Outcome == buzzop.Accepted {
		c.ChannelID = spec.ID
	}
	return c
}

// AddMember adds pubkey to channel with role.
func (p *Port) AddMember(ctx context.Context, key buzzop.Key, channel, pubkey, role string) buzzop.Result {
	return p.write(ctx, key, func(at time.Time) (nostr.Event, error) {
		return buzzwire.AddMember(channel, pubkey, role, at)
	})
}

// SetArchived archives channel, or unarchives it when archived is false.
func (p *Port) SetArchived(ctx context.Context, key buzzop.Key, channel string, archived bool) buzzop.Result {
	return p.write(ctx, key, func(at time.Time) (nostr.Event, error) {
		return buzzwire.SetArchived(channel, archived, at)
	})
}

// Send posts m.Content in m.Channel, mentioning m.Mention and replying to
// m.ReplyTo when they are set.
func (p *Port) Send(ctx context.Context, m buzzop.Message) buzzop.Result {
	return p.write(ctx, m.Key, func(at time.Time) (nostr.Event, error) {
		return buzzwire.Message(m.Channel, m.Content, m.Mention, m.ReplyTo, at)
	})
}

// Edit replaces the content of eventID in channel.
func (p *Port) Edit(ctx context.Context, key buzzop.Key, channel, eventID, content string) buzzop.Result {
	return p.write(ctx, key, func(at time.Time) (nostr.Event, error) {
		return buzzwire.Edit(channel, eventID, content, at)
	})
}

// Replay sends the stored event of key again, with the same event id. A
// key with no record is NotSent.
func (p *Port) Replay(ctx context.Context, key buzzop.Key) buzzop.Result {
	return p.write(ctx, key, nil)
}

// write makes one attempt of the write named by key. build makes the
// unsigned event when key has no record yet; nil means the key must have
// one. The record's reg is the second element of the key ("<what>/<reg>/..."),
// or "" for a key with no second element.
//
// Invariants:
//   - A write without a key is NotSent: it cannot be made idempotent.
//   - Input that cannot be built into an event (content over the limit, a
//     malformed id) is Rejected and nothing is prepared: it is never
//     accepted as it is, so it must not be sent again.
//   - The port signs at most one event per key. When key has a record, its
//     stored event is the one sent and build is not called.
//   - Only a kind buzzwire.Publishable is prepared or sent.
//   - A record already accepted is Accepted with its event id, and nothing
//     is sent.
//   - The outcome is unknown, durably, before the bytes leave; only the
//     returned error of the publish settles it.
//   - Accepted means the relay's positive OK for this event id, or a stored
//     accepted record.
//   - An attempt whose bytes never left (no connection, a failed
//     BeginAttempt, a publish refused before it wrote) proves nothing new:
//     the result and the stored outcome are what the earlier attempts left
//     (not_sent for a record never sent, else unknown or rejected).
//   - A record that cannot be read is Unknown: nothing says what an earlier
//     attempt did. A storage failure after the publish began is Unknown
//     too; a failure to make a new record is NotSent. The storage error is
//     the Reason.
//   - The port never retries; a not_sent record is due now, an unknown or
//     rejected one after retryGap.
func (p *Port) write(ctx context.Context, key buzzop.Key, build func(time.Time) (nostr.Event, error)) buzzop.Result {
	if key == "" {
		return notSent("a write needs a key")
	}
	now := p.now()
	rec, ok, err := p.out.Get(string(key))
	if err != nil {
		return buzzop.Result{Outcome: buzzop.Unknown, Reason: "outbox: " + err.Error()}
	}
	if !ok {
		if build == nil {
			return notSent(fmt.Sprintf("no outbox record for key %q", key))
		}
		evt, err := build(now)
		if err != nil {
			return buzzop.Result{Outcome: buzzop.Rejected, Reason: err.Error()}
		}
		if !buzzwire.Publishable(evt.Kind) {
			return notSent(fmt.Sprintf("kind %d is not published", evt.Kind))
		}
		if err := relay.Sign(&evt, p.secret); err != nil {
			return notSent("sign: " + err.Error())
		}
		if rec, err = p.out.Prepare(string(key), reg(key), evt, now); err != nil {
			return notSent("outbox: " + err.Error())
		}
	}
	if rec.Outcome == buzzop.Accepted {
		return buzzop.Result{Outcome: buzzop.Accepted, EventID: rec.EventID}
	}
	// held is what an attempt that never sends reports: the outcome the
	// earlier attempts left.
	held := buzzop.Result{Outcome: rec.Outcome, EventID: rec.EventID}
	evt, err := rec.SignedEvent()
	if err != nil {
		held.Reason = err.Error()
		return held
	}
	if !buzzwire.Publishable(evt.Kind) {
		held.Reason = fmt.Sprintf("kind %d is not published", evt.Kind)
		return held
	}
	conn := p.live()
	if conn == nil {
		held.Reason = buzzop.ReasonNoConnection
		return held
	}
	if _, err := p.out.BeginAttempt(string(key), now); err != nil {
		held.Reason = "outbox: " + err.Error()
		return held
	}
	pubErr := conn.Publish(ctx, evt)
	r := buzzop.Result{Outcome: outbox.OutcomeOf(pubErr), EventID: rec.EventID, Reason: reason(pubErr)}
	if r.Outcome == buzzop.NotSent {
		r.Outcome = held.Outcome
	}
	settledAt := p.now()
	next := settledAt
	if r.Outcome != buzzop.NotSent {
		next = settledAt.Add(retryGap)
	}
	if _, err := p.out.Settle(string(key), r.Outcome, r.Reason, settledAt, next); err != nil {
		return buzzop.Result{Outcome: buzzop.Unknown, EventID: rec.EventID,
			Reason: fmt.Sprintf("outbox: %v (the relay's result was %s)", err, r.Outcome)}
	}
	return r
}

// live is the current connection, or nil; a nil *relay.Conn inside the
// interface counts as none.
func (p *Port) live() Conn {
	c := p.conn()
	if rc, ok := c.(*relay.Conn); ok && rc == nil {
		return nil
	}
	return c
}

// ChannelFacts reads the relay-signed state of channel up to EOSE. No state
// event before EOSE leaves the facts Unknown and is not an error. No
// connection, a timeout before EOSE, or a closed subscription is an error,
// which proves nothing.
func (p *Port) ChannelFacts(ctx context.Context, channel string) (buzzop.ChannelFacts, error) {
	filter, err := buzzwire.GroupStateFilter(p.id.RelaySelf, []string{channel})
	if err != nil {
		return buzzop.ChannelFacts{}, err
	}
	events, err := p.read(ctx, filter)
	if err != nil {
		return buzzop.ChannelFacts{}, err
	}
	newest := map[nostr.Kind]nostr.Event{}
	for _, evt := range events {
		if cur, ok := newest[evt.Kind]; !ok || newer(evt, cur) {
			newest[evt.Kind] = evt
		}
	}
	kept := make([]nostr.Event, 0, len(newest))
	for _, kind := range []nostr.Kind{buzzwire.KindGroupMetadata, buzzwire.KindGroupAdmins, buzzwire.KindGroupMembers} {
		if evt, ok := newest[kind]; ok {
			kept = append(kept, evt)
		}
	}
	return buzzwire.Facts(channel, p.id.RelaySelf, kept...)
}

// newer is the replaceable-event order: the later created_at, then the
// lower id.
func newer(a, b nostr.Event) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID.Hex() < b.ID.Hex()
}

// Reactions reads the reactions on eventID up to EOSE: one Reaction per
// emoji (the event content) with its distinct authors, in first-seen order.
// A reaction targets its last e tag, so one that only names eventID in an
// earlier e tag is not a reaction on it. The error rule is ChannelFacts's.
func (p *Port) Reactions(ctx context.Context, eventID string) ([]buzzop.Reaction, error) {
	filter, err := buzzwire.ReactionsFilter([]string{eventID})
	if err != nil {
		return nil, err
	}
	events, err := p.read(ctx, filter)
	if err != nil {
		return nil, err
	}
	var out []buzzop.Reaction
	index := map[string]int{}
	for _, evt := range events {
		if !strings.EqualFold(reactionTarget(evt), eventID) {
			continue
		}
		i, ok := index[evt.Content]
		if !ok {
			i = len(out)
			index[evt.Content] = i
			out = append(out, buzzop.Reaction{Emoji: evt.Content})
		}
		if pk := evt.PubKey.Hex(); !slices.Contains(out[i].PubKeys, pk) {
			out[i].PubKeys = append(out[i].PubKeys, pk)
		}
	}
	return out, nil
}

// reactionTarget is the event a reaction targets: the value of its last e
// tag, or "".
func reactionTarget(evt nostr.Event) string {
	target := ""
	for _, t := range evt.Tags {
		if len(t) >= 2 && t[0] == "e" {
			target = t[1]
		}
	}
	return target
}

// Lookup reports whether the relay stores the event with this id. An error
// proves nothing.
func (p *Port) Lookup(ctx context.Context, eventID string) (bool, error) {
	id, err := nostr.IDFromHex(eventID)
	if err != nil {
		return false, fmt.Errorf("event id: %w", err)
	}
	events, err := p.read(ctx, nostr.Filter{IDs: []nostr.ID{id}, Limit: 1})
	if err != nil {
		return false, err
	}
	for _, evt := range events {
		if evt.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// read returns the stored events that match filter, up to EOSE. The relay
// client delivers only events whose id and signature verify and that match
// the filter.
func (p *Port) read(ctx context.Context, filter nostr.Filter) ([]nostr.Event, error) {
	conn := p.live()
	if conn == nil {
		return nil, errors.New(buzzop.ReasonNoConnection)
	}
	ctx, cancel := context.WithTimeout(ctx, p.readTimeout)
	defer cancel()
	sub, err := conn.Subscribe(ctx, fmt.Sprintf("buzzdirect-%d", subSeq.Add(1)), filter)
	if err != nil {
		return nil, err
	}
	defer sub.Close()
	var out []nostr.Event
	for {
		select {
		case evt := <-sub.Events:
			out = append(out, evt)
		case <-sub.EOSE:
			// Stored events are queued before EOSE; take them first.
			for {
				select {
				case evt := <-sub.Events:
					out = append(out, evt)
				default:
					return out, nil
				}
			}
		case <-sub.Done():
			return nil, fmt.Errorf("the relay read ended before EOSE: %v", sub.Err())
		case <-ctx.Done():
			return nil, errors.New("the relay read timed out before EOSE")
		}
	}
}

// reason is the relay's words for a publish error: the refusal text of a
// *relay.RemoteError, else the error's text.
func reason(err error) string {
	if err == nil {
		return ""
	}
	var remote *relay.RemoteError
	if errors.As(err, &remote) && remote.Reason != "" {
		return remote.Reason
	}
	return err.Error()
}

// reg is the second element of key, or "".
func reg(key buzzop.Key) string {
	parts := strings.SplitN(string(key), "/", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func notSent(reason string) buzzop.Result {
	return buzzop.Result{Outcome: buzzop.NotSent, Reason: reason}
}
