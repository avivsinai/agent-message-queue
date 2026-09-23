package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	// presence holds each presence share by session (611.17 slice 2).
	presence map[string]*presenceShare
	state    map[string]string // session -> commands surface state, for status
	// handle is bound once the endpoint exists; carriers only call it from
	// Ingest, which starts after startup.
	handle buzzio.Handler
	// identity is the endpoint's in-process native session accessor, bound
	// with handle.
	identity func(target string) string
}

// buildDMEdges opens a carrier per commands share before startup
// reconciliation, so a Buzz record owed from before a restart can publish
// during reconcile. A share whose enrolled credentials cannot load keeps its
// commands surface closed and is reported, never silently attached.
func buildDMEdges(root, stateDir string, r *manifest.Relay, warn io.Writer) *dmEdges {
	d := &dmEdges{byBody: map[string]*dmShare{}, state: map[string]string{}, presence: map[string]*presenceShare{}}
	if r == nil {
		return d
	}
	d.relay, d.self = r.URL, r.Self
	dupBodies := map[string]bool{}
	for _, sh := range r.Shares {
		if sh.Presence {
			d.addPresence(root, sh, warn)
		}
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
		b := buzzio.Binding{Owner: sh.OwnerPubKey, Body: creds.Body.PublicKeyHex(), Channel: sh.DMChannelID, Target: sh.Target, RelayHost: r.URL, NativeSession: sh.NativeSessionID}
		// One body serves one share: two sessions whose key directories hold
		// the same body are both refused, never merged (codex #866 r1 #2).
		if prev, dup := d.byBody[b.Body]; dup || dupBodies[b.Body] {
			if prev != nil {
				d.state[prev.share.Session] = "refused: its body key is also enrolled for another share"
				delete(d.byBody, b.Body)
			}
			dupBodies[b.Body] = true
			d.state[sh.Session] = "refused: its body key is also enrolled for another share"
			say(warn, "relay share %s: commands disabled: body key shared with another session", sh.Session)
			continue
		}
		if len(sh.MentionChannels) > 0 {
			b.Mentions = map[string]bool{}
			for _, ch := range sh.MentionChannels {
				b.Mentions[ch] = true
			}
		}
		ds := &dmShare{share: sh, binding: b}
		grant := enrolledGrant(root, sh.Session, b)
		for _, kind := range []uint16{buzzio.KindDM, buzzio.KindEdit} {
			if _, gerr := grant(kind, time.Now()); gerr != nil && err == nil {
				err = gerr
			}
		}
		if err != nil {
			d.state[sh.Session] = fmt.Sprintf("refused: buzz-dm is not enrolled (run amq-remote share --session %s --enable buzz-dm): %v", sh.Session, err)
			say(warn, "relay share %s: commands disabled: buzz-dm is not enrolled", sh.Session)
			continue
		}
		ds.carrier = buzzio.NewCarrier(ledger, b, creds.Body.Secret(), grant, d.identityLate, d.handleLate)
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

// addPresence builds a share's presence surface from its enrolled body. A
// share whose body cannot load is reported and publishes nothing.
func (d *dmEdges) addPresence(root string, sh manifest.Share, warn io.Writer) {
	ps := &presenceShare{share: sh}
	creds, err := sharestate.Load(root, sh.Session)
	if err == nil {
		ps.owner, err = nostr.PubKeyFromHex(sh.OwnerPubKey)
	}
	if err == nil {
		ps.body, err = nostr.PubKeyFromHex(creds.Body.PublicKeyHex())
	}
	if err != nil {
		ps.state = "refused: " + err.Error()
		say(warn, "relay share %s: presence disabled: %v", sh.Session, err)
		d.presence[sh.Session] = ps
		return
	}
	b := buzzio.Binding{Owner: sh.OwnerPubKey, Body: creds.Body.PublicKeyHex()}
	ps.p = buzzio.NewPresence(buzzio.NewSigner(creds.Body.Secret(), enrolledGrant(root, sh.Session, b)), sh.Name)
	ps.state = "configured"
	d.presence[sh.Session] = ps
}

// presenceFor returns a session's presence share when it can publish.
func (d *dmEdges) presenceFor(session string) *presenceShare {
	if d == nil {
		return nil
	}
	ps := d.presence[session]
	if ps == nil || ps.p == nil {
		return nil
	}
	return ps
}

// presenceView returns a presence share's state and discovery for status.
func (d *dmEdges) presenceView(session string) (string, string) {
	if d == nil || d.presence[session] == nil {
		return "", ""
	}
	return d.presence[session].view()
}

// bind attaches the endpoint: its command handler and its native session
// accessor.
func (d *dmEdges) bind(h buzzio.Handler, identity func(target string) string) {
	d.mu.Lock()
	d.handle, d.identity = h, identity
	d.mu.Unlock()
}

// identityLate is the native session of target, "" before bind.
func (d *dmEdges) identityLate(target string) string {
	d.mu.Lock()
	id := d.identity
	d.mu.Unlock()
	if id == nil {
		return ""
	}
	return id(target)
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
//
// Freshness policy (lead ruling 2026-09-23, codex #866 r1): Buzz offers no
// read of current membership; the relay-signed 39002/39000 snapshots are
// stored events behind a relay-side cache. v1 accepts them as the gate: the
// surface opens on a verified snapshot and closes on the first re-read that
// no longer verifies. No bound on how late a change shows is claimed.
var membershipRecheck = time.Minute

// errMembershipChanged ends one open DM session when a re-read no longer
// proves owner-and-body membership or the approved native session.
var errMembershipChanged = errors.New("DM channel membership or shared session changed")

// relaySelf returns the pinned relay self key, or reads it from the relay's
// NIP-11 document.
func (d *dmEdges) relaySelf(ctx context.Context) (string, error) {
	if d.self != "" {
		return d.self, nil
	}
	return buzzio.RelaySelf(ctx, d.relay)
}

// verify checks, once, that the channel membership is exactly owner and body
// and that the target's attached session is the approved native session.
func (ds *dmShare) verify(ctx context.Context, conn *relay.Conn, edges *dmEdges) error {
	self, err := edges.relaySelf(ctx)
	if err != nil {
		return err
	}
	if err := verifyMembership(ctx, conn, self, ds.binding); err != nil {
		return err
	}
	return ds.carrier.Shared()
}

// runDM is one authenticated connection's DM edge. The surface opens only
// while verify passes; a failed check or a change closes it (no command
// admitted, no output exported) and it re-checks on the recheck interval. A
// subscription that ends on a healthy socket (CLOSED, overflow) closes the
// connection, so the client's reconnect restores every subscription and
// re-reads the overlap window (codex #866 r1 #9). It returns when the
// connection or ctx ends.
func (ds *dmShare) runDM(ctx context.Context, conn *relay.Conn, edges *dmEdges, warn io.Writer) {
	session := ds.share.Session
	for {
		err := ds.verify(ctx, conn, edges)
		if err == nil {
			err = ds.serveDM(ctx, conn, edges, warn)
			if !errors.Is(err, errMembershipChanged) {
				if ctx.Err() == nil {
					select {
					case <-conn.Done():
					default:
						edges.setState(session, "closed: "+fmt.Sprint(err)+"; reconnecting")
						conn.Close()
					}
				}
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

// serveDM subscribes to the owner's messages, reactions and mentions,
// ingests them, and runs two bounded loops beside the ingest loop: the
// membership re-check and the outbox flush. Each command admission and each
// export checks the verified flag, so a stalled publication never delays a
// membership change or command handling (codex #866 r1 #8). It returns
// errMembershipChanged when a re-check fails, and nil or a subscription
// error when the session ends otherwise.
func (ds *dmShare) serveDM(ctx context.Context, conn *relay.Conn, edges *dmEdges, warn io.Writer) error {
	session := ds.share.Session
	owner, err := nostr.PubKeyFromHex(ds.binding.Owner)
	if err != nil {
		edges.setState(session, "closed: owner pubkey: "+err.Error())
		return err
	}
	since := nostr.Timestamp(time.Now().Add(-dmOverlap).Unix())
	sub, err := conn.Subscribe(ctx, "dm-"+session, nostr.Filter{
		Kinds: []nostr.Kind{buzzio.KindDM}, Authors: []nostr.PubKey{owner},
		Tags: nostr.TagMap{"h": {ds.binding.Channel}}, Since: since,
	})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Close()
	// Reactions need their own owner-authored subscription with no h filter:
	// the phone's reaction carries the target row, not the channel. Each is
	// validated against this edge's persisted rows (IngestReaction).
	reactions, err := conn.Subscribe(ctx, "dm-react-"+session, nostr.Filter{
		Kinds: []nostr.Kind{buzzio.KindReaction}, Authors: []nostr.PubKey{owner}, Since: since,
	})
	if err != nil {
		return fmt.Errorf("subscribe reactions: %w", err)
	}
	defer reactions.Close()
	// Owner messages that mention the body in an opted-in channel (slice 5).
	// Without mention channels these channels stay nil and never fire.
	var mentions *relay.Sub
	var mentionEvents <-chan nostr.Event
	var mentionsDone <-chan struct{}
	if len(ds.share.MentionChannels) > 0 {
		mentions, err = conn.Subscribe(ctx, "dm-mention-"+session, nostr.Filter{
			Kinds: []nostr.Kind{buzzio.KindDM}, Authors: []nostr.PubKey{owner},
			Tags: nostr.TagMap{"h": ds.share.MentionChannels, "p": {ds.binding.Body}}, Since: since,
		})
		if err != nil {
			return fmt.Errorf("subscribe mentions: %w", err)
		}
		defer mentions.Close()
		mentionEvents, mentionsDone = mentions.Events, mentions.Done()
	}
	edges.setState(session, "subscription_active")

	var open atomic.Bool
	open.Store(true)
	gate := func() error {
		if !open.Load() {
			return errMembershipChanged
		}
		return nil
	}
	changed := make(chan error, 1)
	var loops sync.WaitGroup
	defer loops.Wait()
	lctx, stop := context.WithCancel(ctx)
	defer stop() // runs before loops.Wait
	loops.Add(2)
	go func() {
		defer loops.Done()
		t := time.NewTicker(membershipRecheck)
		defer t.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-t.C:
				if err := ds.verify(lctx, conn, edges); err != nil && lctx.Err() == nil {
					open.Store(false)
					changed <- err
					return
				}
			}
		}
	}()
	go func() {
		defer loops.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-t.C:
				if err := ds.carrier.Flush(lctx, conn.Publish, gate); err != nil {
					if open.Load() {
						edges.setState(session, "publish_pending: "+err.Error())
					}
				} else if open.Load() && edges.stateOf(session) != "subscription_active" {
					edges.setState(session, "subscription_active")
				}
			}
		}
	}()
	ingest := func(kind string, evt nostr.Event, fn func(nostr.Event) error) {
		if !open.Load() {
			return // re-read from the overlap window once the surface reopens
		}
		if err := fn(evt); err != nil {
			say(warn, "relay share %s: %s %s: %v", session, kind, evt.ID.Hex(), err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-changed:
			return fmt.Errorf("%w: %v", errMembershipChanged, err)
		case <-sub.Done():
			return fmt.Errorf("dm subscription ended: %v", sub.Err())
		case evt := <-sub.Events:
			ingest("ingest", evt, ds.carrier.Ingest)
		case <-reactions.Done():
			return fmt.Errorf("reaction subscription ended: %v", reactions.Err())
		case evt := <-reactions.Events:
			ingest("reaction", evt, ds.carrier.IngestReaction)
		case <-mentionsDone:
			return fmt.Errorf("mention subscription ended: %v", mentions.Err())
		case evt := <-mentionEvents:
			ingest("mention", evt, ds.carrier.IngestMention)
		}
	}
}
