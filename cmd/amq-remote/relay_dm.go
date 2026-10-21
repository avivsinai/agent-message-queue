package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
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
	// presence holds each presence share by session.
	presence map[string]*presenceShare
	// activity holds each activity share by session, and attachment is the
	// endpoint's attachment accessor it reads, bound late.
	activity   map[string]*activityShare
	attachment func(target string) (core.Attachment, bool)
	state      map[string]string // session -> commands surface state, for status
	// handle is bound once the endpoint exists; carriers only call it from
	// Ingest, which starts after startup.
	handle buzzio.Handler
	// identity is the endpoint's in-process native session accessor, bound
	// with handle.
	identity func(target string) string
	// respondThread is the endpoint's thread approval answer path, bound
	// with handle.
	respondThread buzzio.ThreadResponder
	// pins are the DM-approval pins this process wrote (pinApprovals,
	// pinNative).
	pins []approvalPin
	// shared names every target a relay share serves.
	shared map[string]bool
	// dupBodies are the bodies startup found enrolled for two shares.
	dupBodies map[string]bool
	// setupRefused names each share session whose setup refused a surface.
	// A refusal a surface reports later at run time is not one: only a
	// setup refusal is retried on a manifest change.
	setupRefused map[string]bool
}

// buildDMEdges opens a carrier per commands share before startup
// reconciliation, so a Buzz record owed from before a restart can publish
// during reconcile. A share whose enrolled credentials cannot load keeps its
// commands surface closed and is reported, never silently attached.
func buildDMEdges(root, stateDir string, r *manifest.Relay, warn io.Writer) *dmEdges {
	d := &dmEdges{byBody: map[string]*dmShare{}, state: map[string]string{}, presence: map[string]*presenceShare{}, activity: map[string]*activityShare{}, shared: map[string]bool{}, dupBodies: map[string]bool{}, setupRefused: map[string]bool{}}
	if r == nil {
		return d
	}
	d.relay, d.self = r.URL, r.Self
	for _, sh := range r.Shares {
		d.shared[sh.Target] = true
	}
	for _, sh := range r.Shares {
		d.addShare(root, stateDir, sh, true, warn)
	}
	return d
}

// setRelay records the relay that a serve started without one adopts with
// its first share.
func (d *dmEdges) setRelay(url, self string) {
	d.mu.Lock()
	d.relay, d.self = url, self
	d.mu.Unlock()
}

// addShare builds one share's surfaces: activity, presence and the commands
// carrier. Startup and a share added to the manifest later take this one
// path. Only the reconcile goroutine adds or removes shares.
func (d *dmEdges) addShare(root, stateDir string, sh manifest.Share, startup bool, warn io.Writer) {
	d.mu.Lock()
	d.shared[sh.Target] = true
	url := d.relay
	d.mu.Unlock()
	if sh.Activity {
		as, err := newActivityShare(root, stateDir, sh)
		if err != nil {
			say(warn, "relay share %s: activity disabled: %v", sh.Session, err)
			as = &activityShare{share: sh, state: "refused: " + err.Error()}
			d.markRefused(sh.Session)
		}
		d.mu.Lock()
		d.activity[sh.Session] = as
		d.mu.Unlock()
	}
	if sh.Presence {
		d.addPresence(root, sh, warn)
	}
	if !sh.Commands {
		return
	}
	creds, err := sharestate.Load(root, sh.Session)
	if err != nil {
		d.setState(sh.Session, "refused: "+err.Error())
		d.markRefused(sh.Session)
		say(warn, "relay share %s: commands disabled: %v", sh.Session, err)
		return
	}
	ledger, err := buzzio.OpenLedger(filepath.Join(stateDir, "shares", sh.Session))
	if err != nil {
		d.setState(sh.Session, "refused: "+err.Error())
		d.markRefused(sh.Session)
		say(warn, "relay share %s: commands disabled: %v", sh.Session, err)
		return
	}
	b := buzzio.Binding{Owner: sh.OwnerPubKey, Body: creds.Body.PublicKeyHex(), Channel: sh.DMChannelID, Target: sh.Target, RelayHost: url, NativeSession: sh.NativeSessionID, MinEvidence: sh.MinEvidence}
	// One body serves one share: two sessions whose key directories hold
	// the same body are refused, never merged. At startup
	// both are refused; a share added later is refused alone, and the
	// running one keeps its surface.
	const dupRefusal = "refused: its body key is also enrolled for another share"
	d.mu.Lock()
	if prev, dup := d.byBody[b.Body]; dup || d.dupBodies[b.Body] {
		if prev != nil && startup {
			d.state[prev.share.Session] = dupRefusal
			d.setupRefused[prev.share.Session] = true
			delete(d.byBody, b.Body)
			d.dupBodies[b.Body] = true
		}
		d.state[sh.Session] = dupRefusal
		d.setupRefused[sh.Session] = true
		d.mu.Unlock()
		say(warn, "relay share %s: commands disabled: body key shared with another session", sh.Session)
		return
	}
	d.mu.Unlock()
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
		d.setState(sh.Session, fmt.Sprintf("refused: buzz-dm is not enrolled (run amq-remote share --session %s --enable buzz-dm): %v", sh.Session, err))
		d.markRefused(sh.Session)
		say(warn, "relay share %s: commands disabled: buzz-dm is not enrolled", sh.Session)
		return
	}
	ds.carrier = buzzio.NewCarrier(ledger, b, creds.Body.Secret(), grant, d.identityLate, d.handleLate)
	ds.carrier.SetThreadResponder(d.respondThreadLate)
	d.mu.Lock()
	d.byBody[b.Body] = ds
	d.state[sh.Session] = "configured"
	d.mu.Unlock()
}

// removeShare drops a share removed from the manifest: its surfaces, its
// body's startup duplicate mark (so the other share of the pair can recover
// on its retry), and its approval pins, which it returns for the caller to
// unpin once any replacement pin is written. Its owed Buzz
// records stay owed, as after a restart without it. A target serves at most
// one running share, so its shared mark goes with it.
func (d *dmEdges) removeShare(sh manifest.Share, body string) []approvalPin {
	d.mu.Lock()
	for b, ds := range d.byBody {
		if ds.share.Session == sh.Session {
			delete(d.byBody, b)
		}
	}
	if body != "" {
		delete(d.dupBodies, body)
	}
	delete(d.presence, sh.Session)
	delete(d.activity, sh.Session)
	delete(d.state, sh.Session)
	delete(d.setupRefused, sh.Session)
	delete(d.shared, sh.Target)
	var gone, kept []approvalPin
	for _, p := range d.pins {
		if p.share == sh.Session {
			gone = append(gone, p)
		} else {
			kept = append(kept, p)
		}
	}
	d.pins = kept
	d.mu.Unlock()
	return gone
}

// unpin removes approval pins that removeShare returned.
func unpin(pins []approvalPin) {
	for _, p := range pins {
		claude.UnpinApprovals(p.home, p.session, p.token)
	}
}

// markRefused records that the share's setup refused a surface.
func (d *dmEdges) markRefused(session string) {
	d.mu.Lock()
	d.setupRefused[session] = true
	d.mu.Unlock()
}

// refused reports whether a surface of the share was refused at setup.
func (d *dmEdges) refused(session string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.setupRefused[session]
}

// approvalPin is one written DM-approval pin, removed at teardown.
type approvalPin struct {
	home, session, token string
	// share is the relay share session that wrote the pin, "" for a
	// native pin; target is the adapter's target.
	share, target string
}

// pinApprovals pins, for each configured commands share whose target is a
// Claude adapter with approve, the share's native session, so the
// PermissionRequest hook of that session may answer from the DM. A pin
// lives while this process does; the hook ignores a pin whose pid is dead.
func (d *dmEdges) pinApprovals(adapters []manifest.Adapter, warn io.Writer) {
	d.mu.Lock()
	shares := make([]manifest.Share, 0, len(d.byBody))
	for _, ds := range d.byBody {
		shares = append(shares, ds.share)
	}
	d.mu.Unlock()
	for _, sh := range shares {
		d.pinShare(sh, adapters, warn)
	}
}

// pinShare writes the share's approval pin when it is a configured commands
// share of a Claude adapter, then removes the target's native pin, which a
// shared target does not get at startup. The share pin comes first, so the
// session is never without a pin.
func (d *dmEdges) pinShare(sh manifest.Share, adapters []manifest.Adapter, warn io.Writer) {
	d.mu.Lock()
	ds := d.forBodyLocked(sh.Session)
	d.mu.Unlock()
	for _, ad := range adapters {
		if ds == nil || ad.Target != sh.Target || ad.Kind != "claude" || sh.NativeSessionID == "" {
			continue
		}
		if err := d.pin(ad, sh.NativeSessionID, sh.Session, sh.OwnerPubKey); err != nil {
			say(warn, "relay share %s: DM approvals disabled: %v", sh.Session, err)
		}
	}
	d.mu.Lock()
	var gone, kept []approvalPin
	for _, p := range d.pins {
		if p.share == "" && p.target == sh.Target {
			gone = append(gone, p)
		} else {
			kept = append(kept, p)
		}
	}
	d.pins = kept
	d.mu.Unlock()
	for _, p := range gone {
		claude.UnpinApprovals(p.home, p.session, p.token)
	}
}

// forBodyLocked returns the configured DM share of a share session. d.mu is
// held.
func (d *dmEdges) forBodyLocked(session string) *dmShare {
	for _, ds := range d.byBody {
		if ds.share.Session == session {
			return ds
		}
	}
	return nil
}

// pinNative pins, for a Claude adapter with approve that no relay share
// serves, its native session with no owner, so its PermissionRequest hook
// can take a deny from the Buzz Desktop agent (amq-acp) and never an allow.
// An adapter with only observe_approvals gets an observe-only pin instead:
// its approvals are
// shown and never answered. Like a share pin it lives while this process
// does.
func (d *dmEdges) pinNative(ad manifest.Adapter, native string, warn io.Writer) {
	d.mu.Lock()
	shared := d.shared[ad.Target]
	d.mu.Unlock()
	if ad.Kind != "claude" || native == "" || shared {
		return
	}
	if err := d.pin(ad, native, "", ""); err != nil {
		say(warn, "%s: Buzz Desktop approvals disabled: %v", ad.Target, err)
	}
}

// pin writes one approval pin for a Claude adapter with approve, or an
// observe-only pin for one with only observe_approvals, and records it for
// unpinApprovals. An adapter with neither pins nothing.
func (d *dmEdges) pin(ad manifest.Adapter, native, share, owner string) error {
	var cfg claudeApprovals
	if len(ad.Config) > 0 && json.Unmarshal(ad.Config, &cfg) != nil || !cfg.Approve && !cfg.Observe {
		return nil
	}
	home := cfg.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = h
	}
	write := func() (string, error) { return claude.PinApprovals(home, native, share, owner, os.Getpid()) }
	if !cfg.Approve {
		write = func() (string, error) { return claude.ObserveApprovals(home, native, os.Getpid()) }
	}
	token, err := write()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.pins = append(d.pins, approvalPin{home: home, session: native, token: token, share: share, target: ad.Target})
	d.mu.Unlock()
	return nil
}

// claudeApprovals is the approval part of a Claude adapter config.
type claudeApprovals struct {
	Home    string `json:"home"`
	Approve bool   `json:"approve"`
	Observe bool   `json:"observe_approvals"`
}

// observeOnlyTargets names the Claude targets of the root's manifest whose
// approvals are shown and never answered: observe_approvals without approve.
func observeOnlyTargets(stateDir string) map[string]bool {
	mf, err := manifest.Load(manifest.DefaultPath(stateDir))
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, ad := range mf.Adapters {
		var cfg claudeApprovals
		if ad.Kind == "claude" && json.Unmarshal(ad.Config, &cfg) == nil && cfg.Observe && !cfg.Approve {
			out[ad.Target] = true
		}
	}
	return out
}

// unpinApprovals removes every pin this process wrote.
func (d *dmEdges) unpinApprovals() {
	d.mu.Lock()
	pins := d.pins
	d.pins = nil
	d.mu.Unlock()
	for _, p := range pins {
		claude.UnpinApprovals(p.home, p.session, p.token)
	}
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
		d.markRefused(sh.Session)
		say(warn, "relay share %s: presence disabled: %v", sh.Session, err)
	} else {
		b := buzzio.Binding{Owner: sh.OwnerPubKey, Body: creds.Body.PublicKeyHex()}
		ps.p = buzzio.NewPresence(buzzio.NewSigner(creds.Body.Secret(), enrolledGrant(root, sh.Session, b)), sh.Name)
		ps.state = "configured"
	}
	d.mu.Lock()
	d.presence[sh.Session] = ps
	d.mu.Unlock()
}

// bindAttachments attaches the endpoint's attachment accessor.
func (d *dmEdges) bindAttachments(fn func(target string) (core.Attachment, bool)) {
	d.mu.Lock()
	d.attachment = fn
	d.mu.Unlock()
}

// attachmentLate is the target's attachment, or false before bind.
func (d *dmEdges) attachmentLate(target string) (core.Attachment, bool) {
	d.mu.Lock()
	fn := d.attachment
	d.mu.Unlock()
	if fn == nil {
		return nil, false
	}
	return fn(target)
}

// activityFor returns a session's activity share when it can export.
func (d *dmEdges) activityFor(session string) *activityShare {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	as := d.activity[session]
	d.mu.Unlock()
	if as == nil || as.stateDir == "" {
		return nil
	}
	return as
}

// activityView is an activity share's state for status.
func (d *dmEdges) activityView(session string) string {
	if d == nil {
		return ""
	}
	d.mu.Lock()
	as := d.activity[session]
	d.mu.Unlock()
	if as == nil {
		return ""
	}
	return as.view()
}

// presenceFor returns a session's presence share when it can publish.
func (d *dmEdges) presenceFor(session string) *presenceShare {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	ps := d.presence[session]
	d.mu.Unlock()
	if ps == nil || ps.p == nil {
		return nil
	}
	return ps
}

// presenceView returns a presence share's state and discovery for status.
func (d *dmEdges) presenceView(session string) (string, string) {
	if d == nil {
		return "", ""
	}
	d.mu.Lock()
	ps := d.presence[session]
	d.mu.Unlock()
	if ps == nil {
		return "", ""
	}
	return ps.view()
}

// bind attaches the endpoint: its command handler and its native session
// accessor.
func (d *dmEdges) bind(h buzzio.Handler, identity func(target string) string) {
	d.mu.Lock()
	d.handle, d.identity = h, identity
	d.mu.Unlock()
}

// bindThreadApprovals attaches the endpoint's thread approval answer path
// and returns where the endpoint shows thread approvals: the commands share
// of the target whose approved native session is the approval's.
func (d *dmEdges) bindThreadApprovals(respond buzzio.ThreadResponder) core.ThreadApprovalSink {
	d.mu.Lock()
	d.respondThread = respond
	d.mu.Unlock()
	return core.ThreadApprovalSink{Owner: d.threadOwner, Publish: d.publishThread}
}

// threadShare is the commands share of target pinned to native, or nil.
func (d *dmEdges) threadShare(target, native string) *dmShare {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ds := range d.byBody {
		if ds.binding.Target == target && native != "" && ds.binding.NativeSession == native {
			return ds
		}
	}
	return nil
}

// threadOwner is the source of the share a thread approval belongs to.
func (d *dmEdges) threadOwner(target, native string) (core.Source, bool) {
	ds := d.threadShare(target, native)
	if ds == nil {
		return core.Source{}, false
	}
	return ds.carrier.ShareSource(), true
}

// publishThread shows a thread approval on its share, or leaves it owed.
func (d *dmEdges) publishThread(ta core.ThreadApproval) error {
	ds := d.threadShare(ta.TargetID, ta.NativeSession)
	if ds == nil {
		return fmt.Errorf("%w: no Buzz carrier for thread approval %s", errCarrierUnavailable, ta.ID)
	}
	return ds.carrier.PublishThreadApproval(ta)
}

// respondThreadLate is the endpoint's thread approval answer path.
func (d *dmEdges) respondThreadLate(src core.Source, id, option string) error {
	d.mu.Lock()
	fn := d.respondThread
	d.mu.Unlock()
	if fn == nil {
		return errors.New("endpoint not started")
	}
	return fn(src, id, option)
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
// body, from snapshots signed by the relay's self key.
var verifyMembership = buzzio.VerifyDMMembership

// membershipRecheck is how often an open DM surface re-reads membership.
//
// Freshness policy: Buzz offers no read of current membership; the
// relay-signed 39002/39000 snapshots are
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
	d.mu.Lock()
	url, self := d.relay, d.self
	d.mu.Unlock()
	if self != "" {
		return self, nil
	}
	return buzzio.RelaySelf(ctx, url)
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
// re-reads the overlap window. It returns when the connection or ctx ends.
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
						edges.setState(session, "closed: "+relay.StatusText(err)+"; reconnecting")
						conn.Close()
					}
				}
				return
			}
		}
		edges.setState(session, "closed: "+relay.StatusText(err))
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
// membership change or command handling. It returns errMembershipChanged when
// a re-check fails, and nil or a subscription
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
	// Owner messages that mention the body in an opted-in channel.
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
				if err := ds.carrier.Flush(lctx, conn.Publish, buzzio.StoredEventLookup(conn), gate); err != nil {
					if open.Load() {
						edges.setState(session, "publish_pending: "+relay.StatusText(err))
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
			return fmt.Errorf("%w: %w", errMembershipChanged, err)
		case <-sub.Done():
			return fmt.Errorf("dm subscription ended: %w", sub.Err())
		case evt := <-sub.Events:
			ingest("ingest", evt, ds.carrier.Ingest)
		case <-reactions.Done():
			return fmt.Errorf("reaction subscription ended: %w", reactions.Err())
		case evt := <-reactions.Events:
			ingest("reaction", evt, ds.carrier.IngestReaction)
		case <-mentionsDone:
			return fmt.Errorf("mention subscription ended: %w", mentions.Err())
		case evt := <-mentionEvents:
			ingest("mention", evt, ds.carrier.IngestMention)
		}
	}
}
