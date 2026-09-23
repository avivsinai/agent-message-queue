package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// presenceShare is one share's Buzz presence (611.17 slice 2): the body's
// kind 0 profile and kind 10100 status, plus a read of the owner's kind
// 30177 policy that Desktop needs to list the body as owned. The relay only
// accepts events its authenticated key authored, so the owner's 30177 is
// published by the owner's own client; AMQ reads and reports it.
type presenceShare struct {
	share manifest.Share
	owner nostr.PubKey
	body  nostr.PubKey
	p     *buzzio.Presence

	mu        sync.Mutex
	state     string // online, away, offline, or an error
	profile   string // "published", or why the kind 0 profile is not
	discovery string // policy_present, policy_missing, or an error
	published string // the last status published on this connection
}

// presencePublishTimeout bounds one presence publication's wait for its OK,
// so one missing OK never stalls status or policy refresh (codex #867 r1).
const presencePublishTimeout = 10 * time.Second

// presenceTick is how often a live connection re-derives the status from
// the target's attachment and republishes it on change.
var presenceTick = 10 * time.Second

// discoveryRecheck is how often the owner's 30177 policy is read again.
var discoveryRecheck = 5 * time.Minute

func (ps *presenceShare) set(state, discovery string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if state != "" {
		ps.state = state
	}
	if discovery != "" {
		ps.discovery = discovery
	}
}

// view is the presence state and discovery for status. Until the profile
// is published the state is the profile's problem, never a status that
// would read ready without an ownership profile (codex #867 r1).
func (ps *presenceShare) view() (string, string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.profile != "" && ps.profile != "published" {
		return ps.profile, ps.discovery
	}
	return ps.state, ps.discovery
}

// status maps the target's attachment to a presence status: online only
// when the endpoint has it attached and, for a share that pins
// native_session_id, the attached session is the approved one; away
// otherwise.
func (ps *presenceShare) status(edges *dmEdges) string {
	out, err := edges.handleLate(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionInspect, TargetID: ps.share.Target}, sourceForPresence())
	if err != nil {
		return buzzio.StatusAway
	}
	s, ok := out.(protocol.Session)
	if ok && s.Attachment != "" && s.Attachment != "offline" &&
		(ps.share.NativeSessionID == "" || edges.identityLate(ps.share.Target) == ps.share.NativeSessionID) {
		return buzzio.StatusOnline
	}
	return buzzio.StatusAway
}

// runPresence is one authenticated connection's presence session: publish
// the profile once, the status now and on every change, and read the
// owner's policy. It returns when the connection or ctx ends.
func (ps *presenceShare) runPresence(ctx context.Context, conn *relay.Conn, edges *dmEdges, warn io.Writer) {
	session := ps.share.Session
	ps.mu.Lock()
	ps.published, ps.profile = "", ""
	ps.mu.Unlock()
	ps.publishProfile(ctx, conn, session, warn)
	ps.publishStatus(ctx, conn, ps.status(edges))
	ps.checkDiscovery(ctx, conn)
	tick := time.NewTicker(presenceTick)
	defer tick.Stop()
	recheck := time.NewTicker(discoveryRecheck)
	defer recheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		case <-tick.C:
			ps.publishProfile(ctx, conn, session, warn) // retried until published
			ps.publishStatus(ctx, conn, ps.status(edges))
		case <-recheck.C:
			ps.checkDiscovery(ctx, conn)
		}
	}
}

// publishProfile publishes the kind 0 profile once per connection, and
// retries on each tick until the relay accepts it.
func (ps *presenceShare) publishProfile(ctx context.Context, conn *relay.Conn, session string, warn io.Writer) {
	ps.mu.Lock()
	done := ps.profile == "published"
	first := ps.profile == ""
	ps.mu.Unlock()
	if done {
		return
	}
	evt, err := ps.p.Profile(time.Now())
	if err != nil {
		if first && errors.Is(err, buzzio.ErrNoGrant) {
			say(warn, "relay share %s: profile not enrolled: run amq-remote share --session %s --enable buzz-profile", session, session)
		}
		ps.setProfile("refused: profile: " + err.Error())
		return
	}
	pctx, cancel := context.WithTimeout(ctx, presencePublishTimeout)
	defer cancel()
	if err := conn.Publish(pctx, evt); err != nil {
		ps.setProfile("publish_pending: profile: " + relay.Category(err))
		return
	}
	ps.setProfile("published")
}

func (ps *presenceShare) setProfile(v string) {
	ps.mu.Lock()
	ps.profile = v
	ps.mu.Unlock()
}

// publishStatus publishes status when it differs from the last one this
// connection published.
func (ps *presenceShare) publishStatus(ctx context.Context, conn *relay.Conn, status string) {
	ps.mu.Lock()
	same := ps.published == status
	ps.mu.Unlock()
	if same {
		return
	}
	evt, err := ps.p.Status(status, time.Now())
	if err != nil {
		ps.set("refused: status: "+err.Error(), "")
		return
	}
	pctx, cancel := context.WithTimeout(ctx, presencePublishTimeout)
	defer cancel()
	if err := conn.Publish(pctx, evt); err != nil {
		ps.set("publish_pending: status: "+relay.Category(err), "")
		return
	}
	ps.mu.Lock()
	ps.published = status
	ps.mu.Unlock()
	ps.set(status, "")
}

// beforeClose publishes offline at a graceful shutdown. A crash or lost
// connection cannot, so offline is never a liveness claim.
func (ps *presenceShare) beforeClose(ctx context.Context, conn *relay.Conn) {
	ps.mu.Lock()
	ps.published = ""
	ps.mu.Unlock()
	ps.publishStatus(ctx, conn, buzzio.StatusOffline)
}

// checkDiscovery reads the owner's 30177 coordinate for this body
// ({authors:[owner], kinds:[30177], #d:[body]}), the exact query Desktop
// makes, and records whether it exists.
func (ps *presenceShare) checkDiscovery(ctx context.Context, conn *relay.Conn) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sub, err := conn.Subscribe(qctx, fmt.Sprintf("policy-%s-%d", ps.share.Session, time.Now().UnixNano()), nostr.Filter{
		Kinds: []nostr.Kind{buzzio.KindManagedAgent}, Authors: []nostr.PubKey{ps.owner},
		Tags: nostr.TagMap{"d": {ps.body.Hex()}}, Limit: 1,
	})
	if err != nil {
		ps.set("", "unknown: "+relay.Category(err))
		return
	}
	defer sub.Close()
	// The latest coordinate is the policy; it is validated after selection,
	// as Desktop does, so an older valid one never hides a newer invalid one.
	var latest *nostr.Event
	take := func(evt nostr.Event) {
		if evt.PubKey == ps.owner && (latest == nil || evt.CreatedAt > latest.CreatedAt) {
			e := evt
			latest = &e
		}
	}
	for {
		select {
		case evt := <-sub.Events:
			take(evt)
		case <-sub.EOSE:
			// Stored events are queued before EOSE; drain them before deciding.
			for drained := false; !drained; {
				select {
				case evt := <-sub.Events:
					take(evt)
				default:
					drained = true
				}
			}
			if latest != nil && buzzio.PolicyFor(*latest, ps.body.Hex()) {
				ps.set("", "policy_present")
			} else {
				ps.set("", "policy_missing")
			}
			return
		case <-sub.Done():
			ps.set("", "unknown: "+relay.Category(sub.Err()))
			return
		case <-qctx.Done():
			ps.set("", "unknown: policy read timed out")
			return
		}
	}
}

// sourceForPresence is the endpoint source for presence's own inspect
// reads: no carrier origin, since presence never creates requests.
func sourceForPresence() core.Source { return core.Source{Host: "buzz-presence"} }
