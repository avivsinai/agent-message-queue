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
	discovery string // policy_present, policy_missing, or an error
	published string // the last status published on this connection
}

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

func (ps *presenceShare) view() (string, string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.state, ps.discovery
}

// status maps the target's attachment to a presence status: online only
// when the endpoint has it attached, away otherwise.
func (ps *presenceShare) status(edges *dmEdges) string {
	out, err := edges.handleLate(&protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionInspect, TargetID: ps.share.Target}, sourceForPresence())
	if err != nil {
		return buzzio.StatusAway
	}
	if s, ok := out.(protocol.Session); ok && s.Attachment != "" && s.Attachment != "offline" {
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
	ps.published = ""
	ps.mu.Unlock()
	if evt, err := ps.p.Profile(time.Now()); err != nil {
		if errors.Is(err, buzzio.ErrNoGrant) {
			say(warn, "relay share %s: profile not enrolled: run amq-remote share --session %s --enable buzz-profile", session, session)
		}
		ps.set("refused: profile: "+err.Error(), "")
	} else if err := conn.Publish(ctx, evt); err != nil {
		ps.set("publish_pending: profile: "+relay.Category(err), "")
	}
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
			ps.publishStatus(ctx, conn, ps.status(edges))
		case <-recheck.C:
			ps.checkDiscovery(ctx, conn)
		}
	}
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
	if err := conn.Publish(ctx, evt); err != nil {
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
	found := false
	for {
		select {
		case evt := <-sub.Events:
			if evt.PubKey == ps.owner && buzzio.PolicyFor(evt, ps.body.Hex()) {
				found = true
			}
		case <-sub.EOSE:
			if found {
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
