package linkio

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/jcs"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Errors Publish returns. Both leave the revision owed: core retries it on
// its next sweep and never marks it published.
var (
	// ErrPublishPending: the revision is offered on the live connection and
	// the server has not acknowledged its commit yet.
	ErrPublishPending = errors.New("link: revision offered, waiting for the server's acknowledgement")
	// ErrUnavailable: the link is down or its budget for unacknowledged
	// revisions is full.
	ErrUnavailable = errors.New("link: not connected or budget full")
	// ErrRevoked: the server revoked this device (close 4010).
	ErrRevoked = errors.New("link: the server revoked this device")
)

// Close codes of amq.remote.link/1.
const (
	CloseRevoked       websocket.StatusCode = 4010 // permanent: retire the sink
	closeServerRestart websocket.StatusCode = 1012 // server restart: reconnect after 0-60 s
)

// Defaults from the design (§6, §14).
const (
	DefaultOfferTTL     = 30 * time.Second
	DefaultPingTimeout  = 45 * time.Second
	DefaultRestartDelay = 60 * time.Second
	DefaultMinBackoff   = time.Second
	DefaultMaxBackoff   = 60 * time.Second
	MaxFrameBytes       = 4 << 20
	maxOffers           = 64
	maxOfferBytes       = 8 << 20
	maxAcked            = 4096
	minBusyRetry        = time.Second
	codeConflict        = "conflict"
	revokeQueue         = 64
	handshakeTimeout    = 15 * time.Second
	writeTimeout        = 15 * time.Second
	controlQueue        = 256
	dataQueue           = 64
)

// Config is one link's carrier configuration.
type Config struct {
	Name    string
	Pin     Pin
	StoreID string
	Key     DeviceKey
	Version string
	// Bindings returns the bindings shared with this link right now.
	Bindings func() []Binding
	// ConsentKeyRevoked drops a consent key the server says was removed.
	ConsentKeyRevoked func(credentialID string)
	// Revoked retires the sink after a permanent revoke (close 4010).
	Revoked func()
	// Handle runs an admitted command (core.Endpoint.Handle).
	Handle Handler
	// ConsentKeys returns the consent keys this machine trusts now.
	ConsentKeys func() []ConsentKey
	// LocalKey returns this link's local confirmation passkey, if any.
	LocalKey func() (ConsentKey, bool)
	Logf        func(format string, args ...any)

	Now          func() time.Time
	OfferTTL     time.Duration
	PingTimeout  time.Duration
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
	RestartDelay time.Duration
}

// Status is a link's state for `amq-remote link status`.
type Status struct {
	State    string    `json:"state"` // connecting, online, offline, revoked
	Gen      int64     `json:"gen,omitempty"`
	LastPing time.Time `json:"last_ping,omitzero"`
	// Owed counts the requests whose latest offered revision the server has
	// not acknowledged in this process. It is a view of what this carrier
	// offered, not of the store: a record core stopped owing (compacted)
	// stays counted until the process restarts.
	Owed int `json:"owed"`
	// Conflicts counts revisions the server holds with another digest. They
	// are terminal: never sent again.
	Conflicts int    `json:"conflicts,omitempty"`
	Error     string `json:"error,omitempty"`
}

type offer struct {
	ref     string
	rev     int64
	digest  string
	gen     int64
	expires time.Time
	size    int
	frameID string
}

// Carrier is one link: a connection that it dials, keeps and redials, and
// the acknowledgement cache that decides when a revision is published.
type Carrier struct {
	cfg  Config
	host string

	mu         sync.Mutex
	sess       *session
	acked      map[string]int64 // request_ref -> highest revision the server committed
	ackOrder   []string         // FIFO for bounded eviction
	offers     map[string]map[int64]*offer
	byFrame    map[string]*offer
	offerBytes int
	owed       map[string]int64
	status     Status
	sentBind   []byte                          // JCS of the last bindings the server was told
	bindFrame  string                          // id of the bindings frame awaiting a possible refusal
	conflicts  map[string]int64                // request_ref -> a revision the server holds with another digest
	frameLimit int                             // the smaller of MaxFrameBytes and the server's limit
	oversize   map[string]int64                // request_ref -> a revision already logged as over the frame limit
	revokes    chan string                     // consent keys the server removed, for the revoke worker
	liveKeys   atomic.Pointer[map[string]bool] // consent credential ids accepted now
	revoked    atomic.Pointer[map[string]bool] // consent credential ids the server removed in this process
	held       held                            // signed tasks waiting for the local confirmation
}

// New builds a carrier. It does not dial until Run.
func New(cfg Config) (*Carrier, error) {
	if err := manifest.ValidSocketURL(cfg.Pin.URL); err != nil {
		return nil, err
	}
	if cfg.Pin.ServerID == "" || cfg.StoreID == "" || cfg.Key.Private == nil {
		return nil, errors.New("link: server id, store id and device key are required")
	}
	if cfg.Bindings == nil {
		cfg.Bindings = func() []Binding { return nil }
	}
	if cfg.ConsentKeys == nil {
		cfg.ConsentKeys = func() []ConsentKey { return nil }
	}
	if cfg.LocalKey == nil {
		cfg.LocalKey = func() (ConsentKey, bool) { return ConsentKey{}, false }
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.OfferTTL <= 0 {
		cfg.OfferTTL = DefaultOfferTTL
	}
	if cfg.PingTimeout <= 0 {
		cfg.PingTimeout = DefaultPingTimeout
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = DefaultMinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.RestartDelay <= 0 {
		cfg.RestartDelay = DefaultRestartDelay
	}
	c := &Carrier{
		cfg:       cfg,
		host:      cfg.Key.Host(),
		acked:     map[string]int64{},
		offers:    map[string]map[int64]*offer{},
		byFrame:   map[string]*offer{},
		owed:      map[string]int64{},
		conflicts: map[string]int64{},
		oversize:  map[string]int64{},
		revokes:   make(chan string, revokeQueue),
		status:    Status{State: "offline"},
	}
	c.held.tasks = map[string]*HeldTask{}
	c.refreshKeys() // the consent snapshot exists before the first handoff check
	return c, nil
}

// Host is the link's creator host and sink.
func (c *Carrier) Host() string { return c.host }

// Status returns the link's current state.
func (c *Carrier) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Owed = len(c.owed)
	s.Conflicts = len(c.conflicts)
	if c.sess != nil {
		s.LastPing = time.Unix(0, c.sess.lastPing.Load())
	}
	return s
}

// Publish is the link's core.Publisher. It never waits on the network: it
// returns nil only when the server acknowledged this revision or a newer one
// on this sink. Otherwise it offers the snapshot on the live connection (at
// most once per offer lifetime) and returns ErrPublishPending, or
// ErrUnavailable when the link is down or its budget is full. Core then
// keeps the revision owed and calls again on its next sweep; a nil return
// lets core run its own marker.
func (c *Carrier) Publish(s protocol.Snapshot, _ map[string]string) error {
	ref := s.RequestRef
	c.mu.Lock()
	if c.acked[ref] >= s.Revision {
		delete(c.owed, ref)
		c.mu.Unlock()
		return nil
	}
	if c.owed[ref] < s.Revision {
		c.owed[ref] = s.Revision
	}
	if c.conflicts[ref] == s.Revision {
		c.mu.Unlock()
		return ErrPublishPending // terminal conflict: owed, never sent again
	}
	if c.inFlightLocked(ref, s.Revision) {
		c.mu.Unlock()
		return ErrPublishPending
	}
	if c.sess == nil {
		c.mu.Unlock()
		return ErrUnavailable
	}
	c.mu.Unlock()

	// Encode outside the lock: a snapshot can be several MiB.
	canon, err := jcs.Marshal(s)
	if err != nil {
		return fmt.Errorf("link: encode revision %s/%d: %w", ref, s.Revision, err)
	}
	sum := sha256.Sum256(canon)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	body, err := json.Marshal(revisionBody{
		Schema: SchemaRevision, StoreID: c.cfg.StoreID, RequestRef: ref,
		Revision: s.Revision, Digest: digest, Snapshot: s,
	})
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.acked[ref] >= s.Revision {
		delete(c.owed, ref)
		return nil
	}
	if c.inFlightLocked(ref, s.Revision) {
		return ErrPublishPending
	}
	sess := c.sess
	if sess == nil {
		return ErrUnavailable
	}
	now := c.cfg.Now()
	c.pruneLocked(now)
	if c.offerCountLocked() >= maxOffers || c.offerBytes+len(body) > maxOfferBytes {
		return ErrUnavailable
	}
	id := sess.nextID()
	frame, err := json.Marshal(Frame{Schema: SchemaFrame, ID: id, Gen: sess.gen, Body: body})
	if err != nil {
		return err
	}
	if c.frameLimit > 0 && len(frame) > c.frameLimit {
		// The server would close the socket on it, and the reconnect would
		// offer it again: one revision must not take the link down.
		if c.oversize[ref] != s.Revision {
			c.oversize[ref] = s.Revision
			c.cfg.Logf("link %s: revision %s/%d is %d bytes, over the server's %d-byte frame limit; it stays owed", c.cfg.Name, ref, s.Revision, len(frame), c.frameLimit)
		}
		return ErrUnavailable
	}
	if !sess.send(sess.data, frame) {
		return ErrUnavailable
	}
	o := &offer{ref: ref, rev: s.Revision, digest: digest, gen: sess.gen, expires: now.Add(c.cfg.OfferTTL), size: len(body), frameID: id}
	if c.offers[ref] == nil {
		c.offers[ref] = map[int64]*offer{}
	}
	if old := c.offers[ref][s.Revision]; old != nil {
		c.dropOfferLocked(old)
	}
	c.offers[ref][s.Revision] = o
	c.byFrame[id] = o
	c.offerBytes += o.size
	return ErrPublishPending
}

// inFlightLocked: this revision is offered on the current generation and the
// offer has not expired.
func (c *Carrier) inFlightLocked(ref string, rev int64) bool {
	o := c.offers[ref][rev]
	return o != nil && c.sess != nil && o.gen == c.sess.gen && c.cfg.Now().Before(o.expires)
}

func (c *Carrier) offerCountLocked() int { return len(c.byFrame) }

// pruneLocked drops expired offers and offers from older generations.
func (c *Carrier) pruneLocked(now time.Time) {
	for _, o := range c.byFrame {
		if c.sess == nil || o.gen != c.sess.gen || !now.Before(o.expires) {
			c.dropOfferLocked(o)
		}
	}
}

func (c *Carrier) dropOfferLocked(o *offer) {
	if c.byFrame[o.frameID] != o {
		return
	}
	delete(c.byFrame, o.frameID)
	c.offerBytes -= o.size
	if revs := c.offers[o.ref]; revs != nil && revs[o.rev] == o {
		delete(revs, o.rev)
		if len(revs) == 0 {
			delete(c.offers, o.ref)
		}
	}
}

// ackLocked records that the server committed rev for ref. An
// acknowledgement for any offered revision counts, and only the highest one
// is kept. The cache is bounded; evicting an entry only causes a resend.
func (c *Carrier) ackLocked(ref string, rev int64) {
	old, seen := c.acked[ref]
	if rev > old {
		c.acked[ref] = rev
	}
	if !seen {
		c.ackOrder = append(c.ackOrder, ref)
		for len(c.ackOrder) > maxAcked {
			delete(c.acked, c.ackOrder[0])
			c.ackOrder = c.ackOrder[1:]
		}
	}
	for r, o := range c.offers[ref] {
		if r <= c.acked[ref] {
			c.dropOfferLocked(o)
		}
	}
}

// Tick tells the server when the shared bindings changed. serve calls it on
// every sweep.
func (c *Carrier) Tick() {
	bindings := c.cfg.Bindings()
	canon, err := jcs.Marshal(bindings)
	if err != nil {
		c.cfg.Logf("link %s: encode bindings: %v", c.cfg.Name, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil || string(canon) == string(c.sentBind) {
		return
	}
	body, err := json.Marshal(bindingsBody{Schema: SchemaBindings, Bindings: nonNil(bindings)})
	if err != nil {
		return
	}
	id := c.sess.nextID()
	frame, err := json.Marshal(Frame{Schema: SchemaFrame, ID: id, Gen: c.sess.gen, Body: body})
	if err == nil && c.sess.send(c.sess.data, frame) {
		c.sentBind, c.bindFrame = canon, id
	}
}

func nonNil(b []Binding) []Binding {
	if b == nil {
		return []Binding{}
	}
	return b
}

// Run dials the server and keeps the link up until ctx ends or the server
// revokes the device. It returns nil on cancellation and ErrRevoked after a
// permanent revoke, which it has already passed to Config.Revoked.
func (c *Carrier) Run(ctx context.Context) error {
	go c.revokeWorker(ctx)
	backoff := c.cfg.MinBackoff
	for {
		start := c.cfg.Now()
		err := c.serveOnce(ctx)
		if ctx.Err() != nil {
			c.setState("offline", nil)
			return nil
		}
		code := websocket.CloseStatus(err)
		if code == CloseRevoked {
			c.setState("revoked", err)
			if c.cfg.Revoked != nil {
				c.cfg.Revoked()
			}
			return ErrRevoked
		}
		c.setState("offline", err)
		c.cfg.Logf("link %s: %v", c.cfg.Name, err)
		if c.cfg.Now().Sub(start) > c.cfg.MaxBackoff {
			backoff = c.cfg.MinBackoff
		}
		// Jitter around the backoff, applied after the cap, so it is never
		// clipped away at the cap: a fleet that lost one server stays spread.
		wait := backoff/2 + time.Duration(mrand.Int64N(int64(backoff)+1))
		if code == closeServerRestart {
			wait = time.Duration(mrand.Int64N(int64(c.cfg.RestartDelay) + 1))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff *= 2; backoff > c.cfg.MaxBackoff {
			backoff = c.cfg.MaxBackoff
		}
	}
}

func (c *Carrier) setState(state string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.State = state
	c.status.Error = ""
	if err != nil {
		c.status.Error = err.Error()
	}
}

// serveOnce runs one connection from dial to close.
func (c *Carrier) serveOnce(ctx context.Context) error {
	c.setState("connecting", nil)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("link server redirected; refusing")
	}}
	var lastPing atomic.Int64
	lastPing.Store(c.cfg.Now().UnixNano())
	dctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	ws, _, err := websocket.Dial(dctx, c.cfg.Pin.URL, &websocket.DialOptions{
		HTTPClient: noRedirect,
		OnPingReceived: func(context.Context, []byte) bool {
			lastPing.Store(c.cfg.Now().UnixNano())
			return true
		},
	})
	cancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = ws.CloseNow() }()
	ws.SetReadLimit(MaxFrameBytes)

	welcome, sentBind, err := c.handshake(ctx, ws)
	if err != nil {
		return err
	}
	gen := welcome.ConnectionGeneration
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()
	sess := &session{
		ws: ws, gen: gen, prefix: newPrefix(),
		control: make(chan []byte, controlQueue), data: make(chan []byte, dataQueue),
		dispatch: newDispatcher(connCtx),
	}
	sess.lastPing.Store(lastPing.Load())
	c.mu.Lock()
	c.sess = sess
	// A new connection starts with an empty cache: the next sweep offers
	// every revision still owed, and the server's insert is idempotent.
	c.acked = map[string]int64{}
	c.ackOrder = nil
	c.offers = map[string]map[int64]*offer{}
	c.byFrame = map[string]*offer{}
	c.offerBytes = 0
	c.sentBind, c.bindFrame = sentBind, ""
	c.frameLimit = MaxFrameBytes
	if l := int(welcome.Limits.FrameBytes); l > 0 && l < c.frameLimit {
		c.frameLimit = l
	}
	c.status = Status{State: "online", Gen: gen}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.sess == sess {
			c.sess = nil
		}
		c.mu.Unlock()
	}()
	go sess.writeLoop(connCtx, connCancel)
	go c.watchdog(connCtx, connCancel, sess, &lastPing)
	return c.readLoop(connCtx, sess)
}

// watchdog closes a connection that saw no ping for PingTimeout: the server
// pings every 20 s, so silence means the network or the machine slept.
func (c *Carrier) watchdog(ctx context.Context, cancel context.CancelFunc, sess *session, lastPing *atomic.Int64) {
	t := time.NewTicker(c.cfg.PingTimeout / 9)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			seen := lastPing.Load()
			sess.lastPing.Store(seen)
			if c.cfg.Now().Sub(time.Unix(0, seen)) > c.cfg.PingTimeout {
				_ = sess.ws.Close(websocket.StatusGoingAway, "no ping")
				cancel()
				return
			}
		}
	}
}

// handshake answers the challenge and returns the welcome and the JCS form of
// the bindings hello carried: what the server knows now.
func (c *Carrier) handshake(ctx context.Context, ws *websocket.Conn) (welcomeBody, []byte, error) {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	ch, err := readFrame(hctx, ws)
	if err != nil {
		return welcomeBody{}, nil, fmt.Errorf("challenge: %w", err)
	}
	var body challengeBody
	if err := decodeStrict(ch.Body, SchemaChallenge, &body); err != nil || ch.ID == "" || ch.Gen != 0 {
		return welcomeBody{}, nil, fmt.Errorf("challenge: malformed: %v", err)
	}
	if body.ServerID != c.cfg.Pin.ServerID {
		_ = ws.Close(websocket.StatusPolicyViolation, "unexpected server")
		return welcomeBody{}, nil, fmt.Errorf("challenge: the server says it is %q, this link is pinned to %q", body.ServerID, c.cfg.Pin.ServerID)
	}
	if body.Nonce == "" {
		return welcomeBody{}, nil, errors.New("challenge: empty nonce")
	}
	helloID := "h_" + newPrefix()
	bindings := nonNil(c.cfg.Bindings())
	sentBind, err := jcs.Marshal(bindings)
	if err != nil {
		return welcomeBody{}, nil, err
	}
	hello := helloBody{
		Schema: SchemaHello, DeviceKey: b64.EncodeToString(c.cfg.Key.SPKI), StoreID: c.cfg.StoreID,
		AMQVersion: c.cfg.Version, Bindings: bindings, PendingLocal: c.held.digests(c.cfg.Now()),
		Signature: b64.EncodeToString(c.cfg.Key.Sign(HelloMessage(body.ServerID, body.Nonce, c.cfg.StoreID))),
	}
	raw, err := json.Marshal(hello)
	if err != nil {
		return welcomeBody{}, nil, err
	}
	frame, err := json.Marshal(Frame{Schema: SchemaFrame, ID: helloID, Re: ch.ID, Body: raw})
	if err != nil {
		return welcomeBody{}, nil, err
	}
	if err := ws.Write(hctx, websocket.MessageText, frame); err != nil {
		return welcomeBody{}, nil, fmt.Errorf("hello: %w", err)
	}
	wf, err := readFrame(hctx, ws)
	if err != nil {
		return welcomeBody{}, nil, fmt.Errorf("welcome: %w", err)
	}
	var welcome welcomeBody
	if err := decodeStrict(wf.Body, SchemaWelcome, &welcome); err != nil {
		return welcomeBody{}, nil, fmt.Errorf("welcome: malformed: %w", err)
	}
	if wf.Re != helloID || welcome.ConnectionGeneration < 1 || wf.Gen != welcome.ConnectionGeneration {
		return welcomeBody{}, nil, errors.New("welcome: does not answer this hello or has no generation")
	}
	return welcome, sentBind, nil
}

// HelloMessage is the byte string the device key signs in hello.
func HelloMessage(serverID, nonce, storeID string) []byte {
	return []byte("amq.remote.link/1\x00hello\x00" + serverID + "\x00" + nonce + "\x00" + storeID)
}

func readFrame(ctx context.Context, ws *websocket.Conn) (Frame, error) {
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return Frame{}, err
	}
	if typ != websocket.MessageText {
		return Frame{}, errors.New("binary frame")
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil || f.Schema != SchemaFrame || len(f.Body) == 0 {
		return Frame{}, fmt.Errorf("not an %s frame", SchemaFrame)
	}
	return f, nil
}

// readLoop is the one reader. It never waits on the store, a command or a
// tool call: every frame is handled inline in bounded time or answered from
// the control lane.
func (c *Carrier) readLoop(ctx context.Context, sess *session) error {
	for {
		f, err := readFrame(ctx, sess.ws)
		if err != nil {
			return err
		}
		if f.Gen != sess.gen {
			continue // a frame from an older generation
		}
		switch {
		case f.Re != "":
			c.handleReply(f)
		case f.ID != "":
			c.handleRequest(sess, f)
		}
	}
}

// handleReply reads the server's answer to a revision: its acknowledgement
// with the committed digest, or busy.
func (c *Carrier) handleReply(f Frame) {
	var r reply
	if err := json.Unmarshal(f.Body, &r); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.Re == c.bindFrame && f.Re != "" {
		if r.Error != nil {
			c.sentBind = nil // refused (busy): the next sweep pushes the bindings again
		}
		c.bindFrame = ""
		return
	}
	o := c.byFrame[f.Re]
	if o == nil {
		return // an answer to an expired or older offer
	}
	switch {
	case r.OK != "":
		if r.OK != o.digest {
			c.cfg.Logf("link %s: acknowledgement for %s/%d names digest %s, offered %s", c.cfg.Name, o.ref, o.rev, r.OK, o.digest)
			return
		}
		c.ackLocked(o.ref, o.rev)
	case r.Error != nil && r.Error.Code == string(protocol.CodeBusy):
		// The server keeps nothing; offer again after it asked to wait, and
		// never sooner than a second.
		o.expires = c.cfg.Now().Add(max(time.Duration(r.Error.RetryAfterMS)*time.Millisecond, minBusyRetry))
	case r.Error != nil && r.Error.Code == codeConflict:
		// The server holds this revision with another digest and never
		// overwrites it: terminal for this revision, no resend.
		c.conflicts[o.ref] = o.rev
		c.dropOfferLocked(o)
		c.cfg.Logf("link %s: the server holds %s/%d with another digest; it will not be sent again", c.cfg.Name, o.ref, o.rev)
	case r.Error != nil:
		c.cfg.Logf("link %s: server refused %s/%d: %s %s", c.cfg.Name, o.ref, o.rev, r.Error.Code, r.Error.Message)
	}
}

// revokeWorker drops the consent keys the server removed, one at a time, off
// the reader.
func (c *Carrier) revokeWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-c.revokes:
			if c.cfg.ConsentKeyRevoked != nil {
				c.cfg.ConsentKeyRevoked(id)
			}
		}
	}
}

// handleRequest answers a frame the server sends. key_revoked stops the key
// at once and drops it on disk off the reader. A signed submit and a command
// (get, cancel, session list or inspect) wait in one ordered queue per
// request id on bounded workers (ruling r), so a cancel runs after the submit
// it follows. Only the reader's own refusals (undecodable, busy) are answered
// inline.
func (c *Carrier) handleRequest(sess *session, f Frame) {
	switch bodySchema(f.Body) {
	case SchemaKeyRevoked:
		var b keyRevokedBody
		if err := decodeStrict(f.Body, SchemaKeyRevoked, &b); err == nil && c.cfg.ConsentKeyRevoked != nil {
			// The key stops counting now, before any later frame is read; the
			// disk write follows on the revoke worker.
			c.markRevoked(b.CredentialID)
			select {
			case c.revokes <- b.CredentialID: // the revoke worker does the disk work
			default:
				c.cfg.Logf("link %s: too many key removals at once; dropped %s until the next one", c.cfg.Name, b.CredentialID)
			}
		}
		return
	case SchemaSignedSubmit:
		var m SignedSubmit
		if err := decodeStrict(f.Body, SchemaSignedSubmit, &m); err != nil {
			c.refuse(sess, f.ID, ErrorBody{Code: CodeConsentInvalid, Message: err.Error()})
			return
		}
		// The queue key is the request id, read before verification. Only the
		// order of work depends on it, never what runs: a forged id changes
		// where the frame waits, and verification still decides. A document
		// whose request id does not decode is refused here, so a submit and
		// the cancel that follows it always share one key.
		var d struct {
			Command struct {
				RequestID string `json:"request_id"`
			} `json:"command"`
		}
		doc, err := b64.DecodeString(m.DocumentB64)
		if err != nil || json.Unmarshal(doc, &d) != nil || d.Command.RequestID == "" {
			c.refuse(sess, f.ID, ErrorBody{Code: CodeConsentInvalid, Message: "the signed document names no request"})
			return
		}
		c.enqueue(sess, f, d.Command.RequestID, func() (any, error) { return c.admitSigned(m) })
		return
	case protocol.SchemaCommand:
		cmd, err := protocol.DecodeCommand(f.Body)
		if err != nil {
			c.refuse(sess, f.ID, ErrorBody{Code: string(protocol.CodeInvalid), Message: err.Error()})
			return
		}
		if cmd.Op == protocol.OpRequestSubmit {
			c.refuse(sess, f.ID, ErrorBody{Code: string(protocol.CodeUnsupported), Message: "a link submits only signed tasks (signed_submit)"})
			return
		}
		key := cmd.RequestID
		if cmd.RequestRef != "" {
			// get and cancel queue behind the submit of the same request id.
			if _, _, id, err := protocol.DecodeRef(cmd.RequestRef); err == nil {
				key = id
			}
		}
		if key == "" {
			key = "session:" + cmd.TargetID
		}
		c.enqueue(sess, f, key, func() (any, error) { return c.runCommand(cmd) })
		return
	}
	c.refuse(sess, f.ID, ErrorBody{Code: string(protocol.CodeUnsupported), Message: "this endpoint does not accept that request on a link"})
}

// enqueue queues one server request behind earlier ones with the same key,
// or answers busy when the queue is full.
func (c *Carrier) enqueue(sess *session, f Frame, key string, run func() (any, error)) {
	if !sess.dispatch.enqueue(&job{sess: sess, f: f, key: key, run: run}) {
		c.refuse(sess, f.ID, ErrorBody{Code: string(protocol.CodeBusy), Message: "too many requests in progress", RetryAfterMS: busyRetryAfterMS})
	}
}

// runCommand runs a get, cancel, session list or inspect from the server as
// this link's source. Core applies the link's ownership; session answers are
// the link's own bindings for the targets core lets it see.
func (c *Carrier) runCommand(cmd *protocol.Command) (any, error) {
	if c.cfg.Handle == nil {
		return nil, refuse(string(protocol.CodeUnsupported), "this endpoint runs no commands")
	}
	view := c.view()
	out, err := c.cfg.Handle(cmd, c.source(view, "", ""))
	if err != nil {
		return nil, err
	}
	switch v := out.(type) {
	case []protocol.Session:
		targets := map[string]bool{}
		for _, s := range v {
			targets[s.TargetID] = true
		}
		return bindingsReply{Bindings: bindingsFor(view, targets)}, nil
	case protocol.Session:
		return bindingsReply{Bindings: bindingsFor(view, map[string]bool{v.TargetID: true})}, nil
	}
	return out, nil
}

// bindingsFor lists the view's bindings whose target is in targets.
func bindingsFor(view *ConsentView, targets map[string]bool) []Binding {
	out := []Binding{}
	for _, b := range view.Bindings {
		if targets[b.TargetID] {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b Binding) int { return strings.Compare(a.Binding, b.Binding) })
	return out
}

// admitSigned runs a signed submit: the command decoded from the signed
// bytes, on the native session it was signed for, or a refusal.
func (c *Carrier) admitSigned(m SignedSubmit) (any, error) {
	if c.cfg.Handle == nil {
		return nil, refuse(string(protocol.CodeUnsupported), "this endpoint runs no commands")
	}
	view := c.view()
	v, err := VerifySignedSubmit(view, m, c.cfg.Now())
	if err != nil {
		return nil, err
	}
	if v.Binding.Consent == "local" {
		return c.hold(v, m.CredentialID)
	}
	out, err := c.cfg.Handle(v.Cmd, c.source(view, v.Native, m.CredentialID))
	if err != nil {
		return nil, err
	}
	reply, ok := out.(protocol.Reply)
	if !ok {
		return nil, errors.New("the endpoint returned no submit reply")
	}
	return outcomeReply{Outcome: reply.Outcome}, nil
}

// view is what the machine trusts right now. It is rebuilt for each signed
// submit, so a removed key or a changed binding takes effect at once.
func (c *Carrier) view() *ConsentView {
	v := &ConsentView{ServerID: c.cfg.Pin.ServerID, StoreID: c.cfg.StoreID, Keys: map[string]ConsentKey{}, Bindings: map[string]Binding{}}
	v.Keys = c.refreshKeys()
	for _, b := range c.cfg.Bindings() {
		v.Bindings[b.Binding] = b
	}
	return v
}

// ConsentLive reports whether this link still accepts a consent key. Core
// asks it right before a handoff, under its own lock, so it reads only the
// snapshot the last signed submit or key removal stored.
func (c *Carrier) ConsentLive(credentialID string) bool {
	live := c.liveKeys.Load()
	return live != nil && (*live)[credentialID]
}

// RefreshConsentKeys re-reads the consent keys into the snapshot, after a
// key was removed.
func (c *Carrier) RefreshConsentKeys() { c.refreshKeys() }

// refreshKeys reads the consent keys this machine trusts and stores the
// snapshot ConsentLive reads.
func (c *Carrier) refreshKeys() map[string]ConsentKey {
	keys := map[string]ConsentKey{}
	live := map[string]bool{}
	revoked := c.revoked.Load()
	for _, k := range c.cfg.ConsentKeys() {
		if revoked != nil && (*revoked)[k.CredentialID] {
			continue // removed by the server; the disk may not say so yet
		}
		keys[k.CredentialID] = k
		live[k.CredentialID] = true
	}
	c.liveKeys.Store(&live)
	return keys
}

// markRevoked stops a consent key at once: it joins the revoked set and
// leaves the live snapshot, both copy-on-write, with no I/O.
func (c *Carrier) markRevoked(id string) {
	next := map[string]bool{id: true}
	if old := c.revoked.Load(); old != nil {
		for k := range *old {
			next[k] = true
		}
	}
	c.revoked.Store(&next)
	live := map[string]bool{}
	if old := c.liveKeys.Load(); old != nil {
		for k := range *old {
			if k != id {
				live[k] = true
			}
		}
	}
	c.liveKeys.Store(&live)
}

// source is the authenticated source of a signed submit: this link's sink,
// the native session and consent key it was signed with, and the targets
// shared with the link. The origin routes its revisions back to this sink.
func (c *Carrier) source(view *ConsentView, native, credential string) core.Source {
	shared := make([]string, 0, len(view.Bindings))
	for _, b := range view.Bindings {
		shared = append(shared, b.TargetID)
	}
	return core.Source{
		Host:          c.host,
		Origin:        map[string]string{"carrier": core.CarrierLink, core.OriginSink: c.host},
		Shared:        shared,
		NativeSession: native,
		Credential:    credential,
	}
}

// refuse answers on the control lane. A full control lane drops the answer;
// the server's own timeout covers it.
func (c *Carrier) refuse(sess *session, re string, e ErrorBody) {
	body, err := json.Marshal(errorReply{Error: e})
	if err != nil {
		return
	}
	frame, err := json.Marshal(Frame{Schema: SchemaFrame, Re: re, Gen: sess.gen, Body: body})
	if err == nil {
		sess.send(sess.control, frame)
	}
}

// session is one live connection.
type session struct {
	ws       *websocket.Conn
	gen      int64
	prefix   string
	seq      atomic.Int64
	lastPing atomic.Int64
	control  chan []byte // acknowledgements and refusals: never waits behind data
	data     chan []byte
	dispatch *dispatcher
}

func (s *session) nextID() string {
	return fmt.Sprintf("m_%s_%d", s.prefix, s.seq.Add(1))
}

// send queues a frame without waiting. False means the lane is full.
func (s *session) send(lane chan []byte, frame []byte) bool {
	select {
	case lane <- frame:
		return true
	default:
		return false
	}
}

// writeLoop is the one writer. The control lane always goes first.
func (s *session) writeLoop(ctx context.Context, cancel context.CancelFunc) {
	for {
		var frame []byte
		select {
		case frame = <-s.control:
		default:
			select {
			case <-ctx.Done():
				return
			case frame = <-s.control:
			case frame = <-s.data:
			}
		}
		wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
		err := s.ws.Write(wctx, websocket.MessageText, frame)
		wcancel()
		if err != nil {
			cancel()
			return
		}
	}
}

func newPrefix() string {
	raw := make([]byte, 5)
	_, _ = rand.Read(raw) // crypto/rand.Read never fails on supported platforms
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
}

// hold keeps a verified task on a local binding until the user confirms it on
// this machine. The reply is AMQ's Outcome with code pending_local; nothing
// is stored and nothing runs until the confirmation.
func (c *Carrier) hold(v *Verified, credential string) (any, error) {
	if _, ok := c.cfg.LocalKey(); !ok {
		return nil, refuse(string(protocol.CodeUnsupported), "binding %q needs a local confirmation passkey; run amq-remote link local-key %s in a terminal", v.Binding.Binding, c.cfg.Name)
	}
	notAfter, err := time.Parse(time.RFC3339, v.Cmd.NotAfter)
	if err != nil {
		return nil, refuse(CodeConsentInvalid, "not_after is not RFC 3339")
	}
	t := &HeldTask{
		ID: heldID(v.Cmd.RequestID), Digest: v.Digest, Binding: v.Binding.Binding, Session: v.Binding.Labels.Session,
		Text: v.Text, NotAfter: notAfter, verified: v, credential: credential,
	}
	if err := c.held.add(t, c.cfg.Now()); err != nil {
		return nil, err
	}
	return outcomeReply{Outcome: protocol.Outcome{
		Op: protocol.OpRequestSubmit, Code: protocol.CodePendingLocal,
		Message: "confirm on your machine: amq-remote link confirm " + t.ID,
	}}, nil
}

// HeldTasks lists the tasks waiting for a local confirmation.
func (c *Carrier) HeldTasks() []HeldTask { return c.held.list(c.cfg.Now()) }

// ConfirmLocal runs one held task after the user's local passkey signed the
// confirm challenge of the digest that was shown. The task moves into the
// store once; the passkey assertion is checked here, by the endpoint, so a
// caller that only reaches the local socket cannot confirm without the user.
func (c *Carrier) ConfirmLocal(id, digest, authenticatorData, clientDataJSON, signature string) (any, error) {
	key, ok := c.cfg.LocalKey()
	if !ok {
		return nil, refuse(string(protocol.CodeUnsupported), "this link has no local confirmation passkey")
	}
	if err := VerifyLocalConfirm(key, digest, authenticatorData, clientDataJSON, signature); err != nil {
		return nil, refuse(CodeConsentInvalid, "the confirmation does not verify: %v", err)
	}
	t, err := c.held.take(id, digest, c.cfg.Now())
	if err != nil {
		return nil, err
	}
	if c.cfg.Handle == nil {
		return nil, refuse(string(protocol.CodeUnsupported), "this endpoint runs no commands")
	}
	return c.cfg.Handle(t.verified.Cmd, c.source(c.view(), t.verified.Native, t.credential))
}
