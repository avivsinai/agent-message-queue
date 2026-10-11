package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/sharestate"
)

// relayStatusFile is serve's diagnostic view of each shared session's relay
// connection; doctor reads it. It is a status snapshot, not authority.
const relayStatusFile = "relay-status.json"

// authKind is the enrolled credential kind presented on connection AUTH
// (relay design §3: one tag, chosen deterministically; 1059 is the body's
// enrolled message kind).
const authKind = 1059

type relayShareStatus struct {
	Session string `json:"session"`
	Target  string `json:"target"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Since   string `json:"since"`
	// Commands is the owner-DM surface's state for a commands share:
	// configured, closed (membership unverified), subscription_active,
	// publish_pending, or refused.
	Commands string `json:"commands,omitempty"`
	// Presence is the body's published Buzz status (online, away, offline)
	// or why it is not published; Discovery says whether the owner's kind
	// 30177 policy that Desktop needs exists.
	Presence  string `json:"presence,omitempty"`
	Discovery string `json:"discovery,omitempty"`
	// Activity is the share's activity export state: exporting, or why not.
	Activity string `json:"activity,omitempty"`
}

type relayStatusDoc struct {
	URL    string             `json:"url"`
	Shares []relayShareStatus `json:"shares"`
}

// relayConfigFor re-reads the session's enrolled generation on every
// connection attempt: a renewal is picked up on reconnect, and an expired or
// mismatched credential stops authentication instead of riding an old
// socket. serve never mints, renews or enrolls.
//
// The first body this serve loads is pinned for its lifetime
// (codex slice 1 review r2 #1): a renewal for that body is adopted, but a
// different body under the same owner is refused until the operator
// restarts serve.
func relayConfigFor(root, url string, sh manifest.Share) relay.ConfigFunc {
	var mu sync.Mutex
	pinned := ""
	return func() (relay.Config, error) {
		creds, err := sharestate.Load(root, sh.Session)
		if err != nil {
			return relay.Config{}, err
		}
		if creds.Owner != sh.OwnerPubKey {
			return relay.Config{}, fmt.Errorf("share %s: enrolled owner %s differs from the manifest owner %s", sh.Session, creds.Owner, sh.OwnerPubKey)
		}
		body := creds.Body.PublicKeyHex()
		mu.Lock()
		if pinned == "" {
			pinned = body
		}
		same := pinned == body
		mu.Unlock()
		if !same {
			return relay.Config{}, fmt.Errorf("share %s: enrolled body changed from %s to %s; restart serve to adopt it", sh.Session, pinned, body)
		}
		tag, err := creds.TagFor(authKind, time.Now())
		if err != nil {
			return relay.Config{}, err
		}
		return relay.Config{
			URL:      url,
			Secret:   creds.Body.Secret(),
			AuthTag:  []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()},
			NotAfter: creds.NotAfter,
		}, nil
	}
}

// relaySet runs one authenticated relay client per manifest share and
// follows the manifest while serve runs (#1024): a share added starts on the
// startup path, a share removed stops, and any other change waits for a
// restart. A relay that is down or refusing leaves local IPC and AMQ
// operation untouched.
type relaySet struct {
	root, stateDir, manifestFile string
	edges                        *dmEdges
	stderr                       io.Writer

	// One goroutine runs start and then every tick, and only it changes the
	// set; mu guards what the status writer reads.
	mu        sync.Mutex
	ctx       context.Context
	url, self string // the adopted relay, fixed once set
	running   map[string]*relayRun
	wg        sync.WaitGroup

	raw     []byte            // the manifest bytes last reconciled
	started *manifest.Relay   // the relay block serve started from
	said    map[string]string // what each report key last said
}

// relayRun is one share's running client.
type relayRun struct {
	share  manifest.Share
	body   string // the enrolled body key, "" when it did not load
	client *relay.Client
	cancel context.CancelFunc
	done   chan struct{}
}

// relayStopWait bounds how long a removed share's client may take to close;
// its BeforeClose has its own 2 s bound.
const relayStopWait = 5 * time.Second

func newRelaySet(root, stateDir, manifestFile string, edges *dmEdges, stderr io.Writer) *relaySet {
	return &relaySet{root: root, stateDir: stateDir, manifestFile: manifestFile, edges: edges, stderr: stderr, running: map[string]*relayRun{}, said: map[string]string{}}
}

// start runs the startup shares, whose surfaces buildDMEdges built, and
// the status writer. The first tick reconciles against the file as it is
// then, so an edit made during startup is not missed.
func (rs *relaySet) start(ctx context.Context, r *manifest.Relay) {
	rs.ctx = ctx
	rs.started = r
	if r != nil {
		rs.url, rs.self = r.URL, r.Self
		for _, sh := range r.Shares {
			rs.startShare(sh)
		}
	}
	rs.wg.Add(1)
	go func() {
		defer rs.wg.Done()
		rs.writeStatus(ctx)
	}()
}

// wait returns once every client and the status writer ended.
func (rs *relaySet) wait() { rs.wg.Wait() }

// tick reconciles the relay shares when the manifest bytes changed. The
// bytes, not the mtime, decide: a same-second edit or a rename that keeps
// the mtime still counts. A manifest that does not load keeps every running
// share and is reported once (docs/configuration.md#hot-reload).
func (rs *relaySet) tick() {
	raw, err := os.ReadFile(rs.manifestFile)
	if err != nil && !os.IsNotExist(err) {
		rs.sayOnce("manifest", fmt.Sprintf("relay: reload manifest: %v", err))
		return
	}
	if rs.raw != nil && bytes.Equal(raw, rs.raw) {
		return
	}
	first := rs.raw == nil
	rs.raw = append([]byte{}, raw...)
	mf, err := manifest.Load(rs.manifestFile)
	if err != nil {
		rs.sayOnce("manifest", fmt.Sprintf("relay: the manifest is refused, so the shares stay as they are: %v", err))
		return
	}
	delete(rs.said, "manifest")
	if first && reflect.DeepEqual(mf.Relay, rs.started) {
		// The file serve started from is not a manifest change, so a
		// share refused at startup is not retried (review #1048 r1).
		return
	}
	rs.reconcile(mf)
}

// reconcile makes the running shares match the manifest, keyed by share
// session, with the effect a restart would have and nothing more.
func (rs *relaySet) reconcile(mf manifest.File) {
	want := map[string]manifest.Share{}
	if r := mf.Relay; r != nil {
		if rs.url == "" || len(rs.running) == 0 {
			// A relay with no running share holds nothing to keep: adopt
			// the manifest's, as a restart would.
			rs.mu.Lock()
			rs.url, rs.self = r.URL, r.Self
			rs.mu.Unlock()
			rs.edges.setRelay(r.URL, r.Self)
		}
		if r.URL != rs.url || r.Self != rs.self {
			rs.sayOnce("relay", "relay: the manifest relay url or relay_self changed; restart serve to adopt it")
			return
		}
		delete(rs.said, "relay")
		for _, sh := range r.Shares {
			want[sh.Session] = sh
		}
	}
	// Plan first, then act: removals free a target or a body before a
	// retry or an add may take it (review #1048 r1).
	var removed, retried []*relayRun
	for session, run := range rs.running {
		sh, ok := want[session]
		switch {
		case !ok:
			removed = append(removed, run)
		case !reflect.DeepEqual(sh, run.share):
			entry, _ := json.Marshal(sh)
			rs.sayOnce("share:"+session, fmt.Sprintf("relay share %s: its manifest entry changed; restart serve, or remove and re-add the share, to adopt it", session), string(entry))
		case rs.edges.refused(session):
			// A setup refusal is retried on a manifest change, never on
			// every tick (#1024 ruling 3).
			retried = append(retried, run)
		}
	}
	for _, run := range removed {
		rs.stopShare(run, mf.Adapters)
		say(rs.stderr, "relay share %s: removed from the manifest; stopped", run.share.Session)
	}
	for _, run := range retried {
		// A share that still collides with another running share stays
		// as a restart leaves it (a startup duplicate pair stays refused).
		if rs.collision(run.share, run.body, run.share.Session) != "" {
			continue
		}
		rs.stopShare(run, mf.Adapters)
		rs.addShare(want[run.share.Session], mf.Adapters)
	}
	var added []manifest.Share
	for session, sh := range want {
		if rs.running[session] == nil {
			added = append(added, sh)
		}
	}
	for _, sh := range added {
		if rs.addShare(sh, mf.Adapters) {
			say(rs.stderr, "relay share %s: added from the manifest", sh.Session)
		}
	}
}

// addShare sets up a share added to the manifest: the startup surfaces and
// pins, then its client. A share whose target or enrolled body a running
// share already holds is refused whole: no surface, pin or client, so it
// never takes over the running share's target, profile or presence
// (#1024 ruling 1, review #1048 r1). It reports false then.
func (rs *relaySet) addShare(sh manifest.Share, adapters []manifest.Adapter) bool {
	body := ""
	if creds, err := sharestate.Load(rs.root, sh.Session); err == nil {
		body = creds.Body.PublicKeyHex()
	}
	if why := rs.collision(sh, body, ""); why != "" {
		rs.sayOnce("share:"+sh.Session, fmt.Sprintf("relay share %s: refused: %s", sh.Session, why))
		return false
	}
	delete(rs.said, "share:"+sh.Session)
	rs.edges.addShare(rs.root, rs.stateDir, sh, false, rs.stderr)
	rs.edges.pinShare(sh, adapters, rs.stderr)
	rs.startShare(sh)
	return true
}

// collision names the running share, other than self, that holds sh's
// target or enrolled body, or returns "".
func (rs *relaySet) collision(sh manifest.Share, body, self string) string {
	for session, run := range rs.running {
		switch {
		case session == self:
		case run.share.Target == sh.Target:
			return fmt.Sprintf("running share %s still serves target %s; restart serve, or remove and re-add the share, to adopt it", session, sh.Target)
		case body != "" && run.body == body:
			return fmt.Sprintf("its body key is also enrolled for running share %s", session)
		}
	}
	return ""
}

// stopShare closes a share's client, drops its surfaces and pins, and
// gives an attached Claude target the owner-less native pin a restart would
// (#1024 ruling 2a).
func (rs *relaySet) stopShare(run *relayRun, adapters []manifest.Adapter) {
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(relayStopWait):
		say(rs.stderr, "relay share %s: its client did not close within %s", run.share.Session, relayStopWait)
	}
	rs.mu.Lock()
	delete(rs.running, run.share.Session)
	rs.mu.Unlock()
	delete(rs.said, "share:"+run.share.Session)
	gone := rs.edges.removeShare(run.share, run.body)
	// The native pin is written before the share's pins go, so the session
	// is never without a pin (#1024 ruling 2a).
	for _, ad := range adapters {
		if ad.Target == run.share.Target {
			rs.edges.pinNative(ad, rs.edges.identityLate(ad.Target), rs.stderr)
		}
	}
	unpin(gone)
}

// startShare starts one share's client with its surfaces' hooks.
func (rs *relaySet) startShare(sh manifest.Share) {
	edges, stderr := rs.edges, rs.stderr
	c := relay.NewClient(relayConfigFor(rs.root, rs.url, sh))
	var hooks []func(context.Context, *relay.Conn)
	if sh.Commands {
		if creds, err := sharestate.Load(rs.root, sh.Session); err == nil {
			if ds := edges.forBody(creds.Body.PublicKeyHex()); ds != nil {
				hooks = append(hooks, func(ctx context.Context, conn *relay.Conn) { ds.runDM(ctx, conn, edges, stderr) })
			}
		}
	}
	if as := edges.activityFor(sh.Session); as != nil {
		hooks = append(hooks, func(ctx context.Context, conn *relay.Conn) {
			as.runActivity(ctx, conn, edges.identityLate, edges.attachmentLate, stderr)
		})
	}
	if ps := edges.presenceFor(sh.Session); ps != nil {
		hooks = append(hooks, func(ctx context.Context, conn *relay.Conn) { ps.runPresence(ctx, conn, edges, stderr) })
		c.BeforeClose = ps.beforeClose
	}
	if len(hooks) > 0 {
		c.OnConnect = func(ctx context.Context, conn *relay.Conn) {
			var hw sync.WaitGroup
			for _, h := range hooks {
				hw.Add(1)
				go func() {
					defer hw.Done()
					h(ctx, conn)
				}()
			}
			hw.Wait()
		}
	}
	body := ""
	if creds, err := sharestate.Load(rs.root, sh.Session); err == nil {
		body = creds.Body.PublicKeyHex()
	}
	ctx, cancel := context.WithCancel(rs.ctx)
	run := &relayRun{share: sh, body: body, client: c, cancel: cancel, done: make(chan struct{})}
	rs.mu.Lock()
	rs.running[sh.Session] = run
	rs.mu.Unlock()
	rs.wg.Add(1)
	go func() {
		defer rs.wg.Done()
		defer close(run.done)
		_ = c.Run(ctx)
	}()
}

// sayOnce reports msg under key unless the key last reported the same
// thing; detail, when given, is what makes two reports distinct.
func (rs *relaySet) sayOnce(key, msg string, detail ...string) {
	id := msg
	if len(detail) > 0 {
		id = detail[0]
	}
	if rs.said[key] == id {
		return
	}
	rs.said[key] = id
	say(rs.stderr, "%s", msg)
}

// writeStatus keeps relay-status.json current for doctor until ctx ends.
func (rs *relaySet) writeStatus(ctx context.Context) {
	last := ""
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		rs.mu.Lock()
		doc := relayStatusDoc{URL: rs.url}
		runs := make([]*relayRun, 0, len(rs.running))
		for _, run := range rs.running {
			runs = append(runs, run)
		}
		rs.mu.Unlock()
		for _, run := range runs {
			st := run.client.Status()
			row := relayShareStatus{
				Session: run.share.Session, Target: run.share.Target, State: string(st.State),
				Error: st.Err, Since: st.Since.UTC().Format(time.RFC3339),
			}
			if run.share.Commands {
				row.Commands = rs.edges.stateOf(run.share.Session)
			}
			if run.share.Activity {
				row.Activity = rs.edges.activityView(run.share.Session)
			}
			if run.share.Presence {
				row.Presence, row.Discovery = rs.edges.presenceView(run.share.Session)
			}
			doc.Shares = append(doc.Shares, row)
		}
		sort.Slice(doc.Shares, func(i, j int) bool { return doc.Shares[i].Session < doc.Shares[j].Session })
		raw, _ := json.MarshalIndent(doc, "", "  ")
		if doc.URL != "" && string(raw) != last {
			if _, err := fsq.WriteFileAtomic(rs.stateDir, relayStatusFile, raw, 0o600); err != nil {
				say(rs.stderr, "relay status: %v", err)
			} else {
				last = string(raw)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// loadRelayStatus reads serve's relay status for doctor.
func loadRelayStatus(stateDir string) (*relayStatusDoc, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, relayStatusFile))
	if err != nil {
		return nil, err
	}
	var doc relayStatusDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", relayStatusFile, err)
	}
	return &doc, nil
}
