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
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/sharestate"
)

// errMembershipUnverified keeps the DM surface closed until the channel's
// relay-authoritative membership is proven to be exactly owner and body
// (relay design §4). It fails closed: no command is admitted and no private
// result is published on an unverified channel.
var errMembershipUnverified = errors.New("DM channel membership is not verified")

// dmShare is one share's owner-DM edge.
type dmShare struct {
	share   manifest.Share
	binding buzzio.Binding
	carrier *buzzio.Carrier
}

// dmEdges holds the DM carriers of every commands share, keyed by body.
type dmEdges struct {
	mu     sync.Mutex
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
		ds := &dmShare{share: sh, binding: b}
		ds.carrier = buzzio.NewCarrier(ledger, b, creds.Body.Secret(), d.handleLate)
		d.byBody[b.Body] = ds
		d.state[sh.Session] = "configured"
	}
	return d
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
// body. It is the one piece of the edge that depends on the deployment's
// Buzz membership contract; until that read is implemented it refuses.
var verifyMembership = func(ctx context.Context, conn *relay.Conn, b buzzio.Binding) error {
	return errMembershipUnverified
}

// runDM is one authenticated connection's DM session: verify membership,
// subscribe to the owner's messages in the bound channel, ingest them, and
// flush owed output. It returns when the connection or ctx ends.
func (ds *dmShare) runDM(ctx context.Context, conn *relay.Conn, edges *dmEdges, warn io.Writer) {
	session := ds.share.Session
	if err := verifyMembership(ctx, conn, ds.binding); err != nil {
		edges.setState(session, "closed: "+err.Error())
		<-ctx.Done()
		return
	}
	owner, err := nostr.PubKeyFromHex(ds.binding.Owner)
	if err != nil {
		edges.setState(session, "closed: owner pubkey: "+err.Error())
		return
	}
	sub, err := conn.Subscribe(ctx, "dm-"+session, nostr.Filter{
		Kinds:   []nostr.Kind{buzzio.KindDM},
		Authors: []nostr.PubKey{owner},
		Tags:    nostr.TagMap{"h": {ds.binding.Channel}},
		Since:   nostr.Timestamp(time.Now().Add(-dmOverlap).Unix()),
	})
	if err != nil {
		edges.setState(session, "closed: subscribe: "+err.Error())
		return
	}
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
		return
	}
	edges.setState(session, "subscription_active")
	flush := time.NewTicker(time.Second)
	defer flush.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			edges.setState(session, "closed: "+fmt.Sprint(sub.Err()))
			return
		case evt := <-sub.Events:
			if err := ds.carrier.Ingest(evt); err != nil {
				say(warn, "relay share %s: ingest %s: %v", session, evt.ID.Hex(), err)
			}
		case <-reactions.Done():
			edges.setState(session, "closed: "+fmt.Sprint(reactions.Err()))
			return
		case evt := <-reactions.Events:
			if err := ds.carrier.IngestReaction(evt); err != nil {
				say(warn, "relay share %s: reaction %s: %v", session, evt.ID.Hex(), err)
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
