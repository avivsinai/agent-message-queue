package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/sharestate"
)

// dmShare is one share's owner-DM edge.
type dmShare struct {
	share   manifest.Share
	binding buzzio.Binding
	carrier *buzzio.Carrier
}

// dmEdges holds the DM carriers of every commands share, keyed by body.
type dmEdges struct {
	mu     sync.Mutex
	relay  string // relay URL, for the NIP-11 self lookup
	self   string // pinned relay self key; empty reads NIP-11
	byBody map[string]*dmShare
	state  map[string]string // session -> commands surface state, for status
	// handle is bound once the endpoint exists; carriers only call it from
	// Ingest, which starts after startup.
	handle buzzio.Handler
}

// buildDMEdges opens a carrier per commands share before startup
// reconciliation, so a Buzz record owed from before a restart can publish
// during reconcile. A share whose enrolled credentials cannot load keeps its
// commands surface closed and is reported, never silently attached.
func buildDMEdges(root, stateDir string, r *manifest.Relay, warn io.Writer) *dmEdges {
	d := &dmEdges{byBody: map[string]*dmShare{}, state: map[string]string{}}
	if r == nil {
		return d
	}
	d.relay, d.self = r.URL, r.Self
	for _, sh := range r.Shares {
		if !sh.Commands {
			continue
		}
		creds, err := sharestate.Load(root, sh.Session)
		if err != nil {
			d.state[sh.Session] = "refused: " + err.Error()
			say(warn, "relay share %s: commands disabled: %v", sh.Session, err)
			continue
		}
		ledger, err := buzzio.OpenLedger(filepath.Join(stateDir, "shares", sh.Session))
		if err != nil {
			d.state[sh.Session] = "refused: " + err.Error()
			say(warn, "relay share %s: commands disabled: %v", sh.Session, err)
			continue
		}
		b := buzzio.Binding{Owner: sh.OwnerPubKey, Body: creds.Body.PublicKeyHex(), Channel: sh.DMChannelID, Target: sh.Target, RelayHost: r.URL}
		if len(sh.MentionChannels) > 0 {
			b.Mentions = map[string]bool{}
			for _, ch := range sh.MentionChannels {
				b.Mentions[ch] = true
			}
		}
		ds := &dmShare{share: sh, binding: b}
		ds.carrier = buzzio.NewCarrier(ledger, b, creds.Body.Secret(), enrolledGrant(root, sh.Session, b), d.handleLate)
		d.byBody[b.Body] = ds
		d.state[sh.Session] = "configured"
	}
	return d
}

// enrolledGrant reads the session's current enrolled generation for each
// signed event, so a renewed generation is used without a restart and an
// expired or re-bound one stops signing.
func enrolledGrant(root, session string, b buzzio.Binding) buzzio.Grant {
	return func(kind uint16, at time.Time) (bodykey.AuthTag, error) {
		creds, err := sharestate.Load(root, session)
		if err != nil {
			return bodykey.AuthTag{}, err
		}
		if creds.Body.PublicKeyHex() != b.Body || creds.Owner != b.Owner {
			return bodykey.AuthTag{}, errors.New("enrolled body or owner changed")
		}
		return creds.TagFor(kind, at)
	}
}

func (d *dmEdges) bind(h buzzio.Handler) {
	d.mu.Lock()
	d.handle = h
	d.mu.Unlock()
}

func (d *dmEdges) handleLate(cmd *protocol.Command, src core.Source) (any, error) {
	d.mu.Lock()
	h := d.handle
	d.mu.Unlock()
	if h == nil {
		return nil, errors.New("endpoint not started")
	}
	return h(cmd, src)
}

func (d *dmEdges) setState(session, state string) {
	d.mu.Lock()
	d.state[session] = state
	d.mu.Unlock()
}

func (d *dmEdges) stateOf(session string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state[session]
}

// publish is the buzz entry of the publish router: a record goes to the
// carrier of the body that created it, or stays owed.
func (d *dmEdges) publish(s protocol.Snapshot, origin map[string]string) error {
	d.mu.Lock()
	ds := d.byBody[origin["body"]]
	d.mu.Unlock()
	if ds == nil {
		return fmt.Errorf("%w: no Buzz carrier for body %s", errCarrierUnavailable, origin["body"])
	}
	return ds.carrier.Publish(s, origin)
}

// forBody returns the DM share of a body, if it has one.
func (d *dmEdges) forBody(body string) *dmShare {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.byBody[body]
}

// dmOverlap re-reads recent owner messages after a reconnect; the ingress
// claims make the overlap idempotent.
const dmOverlap = 5 * time.Minute

// verifyMembership proves the DM channel's members are exactly owner and
// body, from snapshots signed by the relay's self key (slice 4 contract).
var verifyMembership = buzzio.VerifyDMMembership

// membershipRecheck is how often an open DM surface re-reads membership.
// The snapshots are cached relay-side, so a change is seen within about this
// interval plus the relay's cache, never instantly.
var membershipRecheck = time.Minute

// errMembershipChanged ends one open DM session when a re-read no longer
// proves owner-and-body membership.
var errMembershipChanged = errors.New("DM channel membership changed")

// relaySelf returns the pinned relay self key, or reads it from the relay's
// NIP-11 document.
func (d *dmEdges) relaySelf(ctx context.Context) (string, error) {
	if d.self != "" {
		return d.self, nil
	}
	return buzzio.RelaySelf(ctx, d.relay)
}

// verify resolves the relay self key and checks membership once.
func (ds *dmShare) verify(ctx context.Context, conn *relay.Conn, edges *dmEdges) error {
	self, err := edges.relaySelf(ctx)
	if err != nil {
		return err
	}
	return verifyMembership(ctx, conn, self, ds.binding)
}

// runDM is one authenticated connection's DM edge. The surface opens only
// while membership verifies; a failed check or a changed membership closes
// it (no command admitted, no result published) and it re-checks on the
// recheck interval. It returns when the connection or ctx ends.
func (ds *dmShare) runDM(ctx context.Context, conn *relay.Conn, edges *dmEdges, warn io.Writer) {
	session := ds.share.Session
	for {
		err := ds.verify(ctx, conn, edges)
		if err == nil {
			err = ds.serveDM(ctx, conn, edges, warn)
			if !errors.Is(err, errMembershipChanged) {
				return
			}
		}
		edges.setState(session, "closed: "+err.Error())
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		case <-time.After(membershipRecheck):
		}
	}
}

// serveDM subscribes to the owner's messages and reactions in the bound
// channel, ingests them, and flushes owed output, re-verifying membership
// on the recheck interval. It returns errMembershipChanged when a re-read
// fails, and nil or a subscription error when the session ends otherwise.
func (ds *dmShare) serveDM(ctx context.Context, conn *relay.Conn, edges *dmEdges, warn io.Writer) error {
	session := ds.share.Session
	owner, err := nostr.PubKeyFromHex(ds.binding.Owner)
	if err != nil {
		edges.setState(session, "closed: owner pubkey: "+err.Error())
		return err
	}
	sub, err := conn.Subscribe(ctx, "dm-"+session, nostr.Filter{
		Kinds:   []nostr.Kind{buzzio.KindDM},
		Authors: []nostr.PubKey{owner},
		Tags:    nostr.TagMap{"h": {ds.binding.Channel}},
		Since:   nostr.Timestamp(time.Now().Add(-dmOverlap).Unix()),
	})
	if err != nil {
		edges.setState(session, "closed: subscribe: "+err.Error())
		return err
	}
	defer sub.Close()
	// Reactions need their own owner-authored subscription with no h filter:
	// the phone's reaction carries the target row, not the channel. Each is
	// validated against this edge's persisted rows (IngestReaction).
	reactions, err := conn.Subscribe(ctx, "dm-react-"+session, nostr.Filter{
		Kinds:   []nostr.Kind{buzzio.KindReaction},
		Authors: []nostr.PubKey{owner},
		Since:   nostr.Timestamp(time.Now().Add(-dmOverlap).Unix()),
	})
	if err != nil {
		edges.setState(session, "closed: subscribe reactions: "+err.Error())
		return err
	}
	defer reactions.Close()
	// Owner messages that mention the body in an opted-in channel (slice 5).
	// Without mention channels this subscription stays nil and never fires.
	var mentions *relay.Sub
	var mentionEvents <-chan nostr.Event
	var mentionsDone <-chan struct{}
	if len(ds.share.MentionChannels) > 0 {
		mentions, err = conn.Subscribe(ctx, "dm-mention-"+session, nostr.Filter{
			Kinds:   []nostr.Kind{buzzio.KindDM},
			Authors: []nostr.PubKey{owner},
			Tags:    nostr.TagMap{"h": ds.share.MentionChannels, "p": {ds.binding.Body}},
			Since:   nostr.Timestamp(time.Now().Add(-dmOverlap).Unix()),
		})
		if err != nil {
			edges.setState(session, "closed: subscribe mentions: "+err.Error())
			return err
		}
		defer mentions.Close()
		mentionEvents, mentionsDone = mentions.Events, mentions.Done()
	}
	edges.setState(session, "subscription_active")
	flush := time.NewTicker(time.Second)
	defer flush.Stop()
	recheck := time.NewTicker(membershipRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sub.Done():
			edges.setState(session, "closed: "+fmt.Sprint(sub.Err()))
			return sub.Err()
		case evt := <-sub.Events:
			if err := ds.carrier.Ingest(evt); err != nil {
				say(warn, "relay share %s: ingest %s: %v", session, evt.ID.Hex(), err)
			}
		case <-reactions.Done():
			edges.setState(session, "closed: "+fmt.Sprint(reactions.Err()))
			return reactions.Err()
		case evt := <-reactions.Events:
			if err := ds.carrier.IngestReaction(evt); err != nil {
				say(warn, "relay share %s: reaction %s: %v", session, evt.ID.Hex(), err)
			}
		case <-mentionsDone:
			edges.setState(session, "closed: "+fmt.Sprint(mentions.Err()))
			return mentions.Err()
		case evt := <-mentionEvents:
			if err := ds.carrier.IngestMention(evt); err != nil {
				say(warn, "relay share %s: mention %s: %v", session, evt.ID.Hex(), err)
			}
		case <-recheck.C:
			if err := ds.verify(ctx, conn, edges); err != nil {
				return fmt.Errorf("%w: %v", errMembershipChanged, err)
			}
		case <-flush.C:
			if err := ds.carrier.Flush(ctx, conn.Publish); err != nil {
				edges.setState(session, "publish_pending: "+err.Error())
			} else if edges.stateOf(session) != "subscription_active" {
				edges.setState(session, "subscription_active")
			}
		}
	}
}
