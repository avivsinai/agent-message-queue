// Package relay is AMQ's Nostr relay transport (bead 611.15, relay design
// slice 1): one bounded WebSocket per relay and body identity, NIP-42 AUTH
// that presents exactly one enrolled NIP-OA tag, and publish that succeeds
// only on the relay's matching OK. It executes no requests, mints no
// credentials and knows nothing about bridge envelopes; callers own policy
// and ledgers.
package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/coder/websocket"
)

// KindAuth is the NIP-42 client authentication event kind.
const KindAuth = 22242

// Defaults from the relay design (§3).
const (
	DefaultMaxInbound  = 512 << 10
	DefaultMaxOutbound = 64 << 10
	DefaultDialTimeout = 15 * time.Second
	DefaultAuthTimeout = 15 * time.Second
	maxPending         = 64
)

// ErrAuthRefused is a negative OK for this connection's AUTH event.
var ErrAuthRefused = errors.New("relay refused authentication")

// ErrUnknownDelivery is a publish whose OK never arrived: the event may or
// may not have been accepted. It is never reported as non-delivery.
var ErrUnknownDelivery = errors.New("relay publish result unknown")

// ErrGrantExpired means the enrolled grant this connection authenticated
// with has passed its signed not-after: the connection is unusable from that
// instant (codex slice 1 review r2 #3).
var ErrGrantExpired = errors.New("enrolled grant expired")

// ErrRejected is an explicit negative OK for a published event.
var ErrRejected = errors.New("relay rejected event")

// RemoteError carries a relay's refusal. Kind is the stable category;
// Reason is relay-controlled text, kept for local debugging and never
// written to status files or shown as AMQ's own words (codex slice 1 review
// #9). Error() returns only the category.
type RemoteError struct {
	Kind   error
	Reason string
}

func (e *RemoteError) Error() string { return e.Kind.Error() }
func (e *RemoteError) Unwrap() error { return e.Kind }

// Config is one connection's identity and bounds.
type Config struct {
	// URL is the relay endpoint: wss://, or ws:// to a loopback host only
	// (in-process tests). No userinfo.
	URL string
	// Secret is the body key's secret scalar; it signs the AUTH event.
	Secret [32]byte
	// AuthTag is the one enrolled NIP-OA tag ["auth", owner, conditions,
	// sig] presented on AUTH.
	AuthTag []string
	// NotAfter is the enrolled grant's signed not-after (created_at must be
	// before it). From that instant the connection is closed and unusable,
	// and an AUTH that completes at or after it is refused. Zero means none.
	NotAfter time.Time
	// Bounds; zero means the default.
	MaxInbound  int64
	MaxOutbound int
	DialTimeout time.Duration
	AuthTimeout time.Duration
	// Now is the clock for AUTH created_at; nil means time.Now.
	Now func() time.Time
}

// ValidateURL enforces the transport policy: wss with normal TLS, or ws to
// a loopback address, and never credentials in the URL.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("relay url: %w", err)
	}
	if u.User != nil {
		return errors.New("relay url must not carry userinfo")
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		host := u.Hostname()
		if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || host == "localhost" {
			return nil
		}
		return errors.New("relay url: ws:// is allowed only to a loopback host; use wss://")
	default:
		return fmt.Errorf("relay url scheme %q: want wss://", u.Scheme)
	}
}

type okResult struct {
	ok     bool
	reason string
}

// Conn is one authenticated relay connection. A Conn never crosses a
// reconnect: its challenge, OK waiters and authenticated flag die with it.
type Conn struct {
	cfg Config
	ws  *websocket.Conn
	// writeSem serializes frame writes; a waiter honors its own context, so
	// a queued publish keeps its deadline behind a stalled writer (codex
	// slice 1 review #6).
	writeSem chan struct{}

	mu        sync.Mutex
	waiters   map[nostr.ID]chan okResult
	challenge chan string
	done      chan struct{}
	readErr   error
	expired   bool
	// expiry closes the connection at cfg.NotAfter; readPump stops it.
	expiry *time.Timer
}

// Connect dials the relay, awaits its AUTH challenge, answers it with a
// signed kind 22242 event carrying the relay and challenge tags plus the one
// NIP-OA tag, and returns only after the relay's positive OK for that event.
// Socket open is not authentication; a negative OK, malformed challenge,
// timeout or disconnect is an error and the connection is closed.
func Connect(ctx context.Context, cfg Config) (*Conn, error) {
	if err := ValidateURL(cfg.URL); err != nil {
		return nil, err
	}
	if len(cfg.AuthTag) != 4 || cfg.AuthTag[0] != "auth" {
		return nil, errors.New("relay: an enrolled NIP-OA auth tag is required")
	}
	if cfg.MaxInbound <= 0 {
		cfg.MaxInbound = DefaultMaxInbound
	}
	if cfg.MaxOutbound <= 0 {
		cfg.MaxOutbound = DefaultMaxOutbound
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultDialTimeout
	}
	if cfg.AuthTimeout <= 0 {
		cfg.AuthTimeout = DefaultAuthTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.expiredAt(cfg.Now()) {
		return nil, ErrGrantExpired
	}
	dctx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	// No redirect is followed (codex slice 1 review #1): the library would
	// otherwise follow one with scheme conversion, taking AUTH to another
	// origin or to cleartext.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("relay redirected; refusing")
	}}
	ws, _, err := websocket.Dial(dctx, cfg.URL, &websocket.DialOptions{HTTPClient: noRedirect})
	if err != nil {
		return nil, fmt.Errorf("relay dial: %w", err)
	}
	ws.SetReadLimit(cfg.MaxInbound)
	c := &Conn{
		cfg:       cfg,
		ws:        ws,
		writeSem:  make(chan struct{}, 1),
		waiters:   map[nostr.ID]chan okResult{},
		challenge: make(chan string, 1),
		done:      make(chan struct{}),
	}
	if !cfg.NotAfter.IsZero() {
		// At expiry access is revoked first, then the transport is aborted
		// without a close handshake: a peer that withholds its close reply
		// cannot keep the connection open (codex slice 1 review r3 #2).
		c.expiry = time.AfterFunc(cfg.NotAfter.Sub(cfg.Now()), func() {
			c.mu.Lock()
			c.expired = true
			c.mu.Unlock()
			_ = c.ws.CloseNow()
		})
	}
	go c.readPump()

	actx, acancel := context.WithTimeout(ctx, cfg.AuthTimeout)
	defer acancel()
	var ch string
	select {
	case ch = <-c.challenge:
	case <-c.done:
		return nil, fmt.Errorf("relay closed before its AUTH challenge: %w", c.err())
	case <-actx.Done():
		c.Close()
		return nil, errors.New("relay sent no AUTH challenge in time")
	}
	evt := nostr.Event{
		CreatedAt: nostr.Timestamp(cfg.Now().Unix()),
		Kind:      KindAuth,
		Tags:      nostr.Tags{{"relay", cfg.URL}, {"challenge", ch}, nostr.Tag(cfg.AuthTag)},
	}
	if err := evt.Sign(cfg.Secret); err != nil {
		c.Close()
		return nil, fmt.Errorf("sign AUTH: %w", err)
	}
	res, err := c.roundTrip(actx, evt, nostr.AuthEnvelope{Event: evt})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("relay AUTH: %w", err)
	}
	if !res.ok {
		c.Close()
		return nil, &RemoteError{Kind: ErrAuthRefused, Reason: res.reason}
	}
	if cfg.expiredAt(cfg.Now()) {
		// A stale completion: the relay accepted a grant that has since run
		// out, so the connection is never handed out.
		c.Close()
		return nil, ErrGrantExpired
	}
	return c, nil
}

// expiredAt reports whether the grant no longer admits an event at now:
// NIP-OA's created_at < not-after, on whole seconds.
func (cfg Config) expiredAt(now time.Time) bool {
	return !cfg.NotAfter.IsZero() && now.Unix() >= cfg.NotAfter.Unix()
}

// Publish sends one already signed event and returns nil only on the
// relay's matching positive OK. A negative OK is ErrRejected; a missing OK
// is ErrUnknownDelivery. It never re-signs or retries.
func (c *Conn) Publish(ctx context.Context, evt nostr.Event) error {
	if !evt.CheckID() || !evt.VerifySignature() {
		return errors.New("relay publish: event is not signed")
	}
	res, err := c.roundTrip(ctx, evt, nostr.EventEnvelope{Event: evt})
	if err != nil {
		return err
	}
	if !res.ok {
		return &RemoteError{Kind: ErrRejected, Reason: res.reason}
	}
	return nil
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended, once Done is closed.
func (c *Conn) Err() error { return c.err() }

// Close ends the connection.
func (c *Conn) Close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }

// Expired reports whether the connection's grant has passed its not-after.
func (c *Conn) Expired() bool {
	c.mu.Lock()
	expired := c.expired
	c.mu.Unlock()
	return expired || c.cfg.expiredAt(c.cfg.Now())
}

// grantCtx bounds ctx by the grant's not-after, so a queued or in-flight
// write or OK wait never outlives the grant.
func (c *Conn) grantCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.cfg.NotAfter.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, c.cfg.NotAfter)
}

func (c *Conn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expired {
		return ErrGrantExpired
	}
	return c.readErr
}

// roundTrip registers a waiter for evt's OK, writes the envelope, and waits.
func (c *Conn) roundTrip(ctx context.Context, evt nostr.Event, env interface{ MarshalJSON() ([]byte, error) }) (okResult, error) {
	if c.Expired() {
		return okResult{}, ErrGrantExpired
	}
	ctx, cancel := c.grantCtx(ctx)
	defer cancel()
	frame, err := env.MarshalJSON()
	if err != nil {
		return okResult{}, err
	}
	if len(frame) > c.cfg.MaxOutbound {
		return okResult{}, fmt.Errorf("relay frame is %d bytes, over the %d-byte bound", len(frame), c.cfg.MaxOutbound)
	}
	wait := make(chan okResult, 1)
	c.mu.Lock()
	if c.readErr != nil {
		c.mu.Unlock()
		return okResult{}, fmt.Errorf("relay connection closed: %w", c.readErr)
	}
	if _, dup := c.waiters[evt.ID]; dup {
		// One immutable event id has one OK; a second concurrent send of
		// it must not steal or delete the first waiter (codex slice 1
		// review #5).
		c.mu.Unlock()
		return okResult{}, fmt.Errorf("relay: event %s is already awaiting its OK", evt.ID.Hex())
	}
	if len(c.waiters) >= maxPending {
		c.mu.Unlock()
		return okResult{}, errors.New("relay: too many publications awaiting OK")
	}
	c.waiters[evt.ID] = wait
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiters, evt.ID)
		c.mu.Unlock()
	}()
	if err := c.write(ctx, frame); err != nil {
		return okResult{}, fmt.Errorf("%w: write: %v", ErrUnknownDelivery, err)
	}
	select {
	case res := <-wait:
		return res, nil
	case <-c.done:
		return okResult{}, fmt.Errorf("%w: connection closed before OK", ErrUnknownDelivery)
	case <-ctx.Done():
		return okResult{}, fmt.Errorf("%w: %v", ErrUnknownDelivery, ctx.Err())
	}
}

// write sends one frame, waiting for the write slot only as long as ctx
// and the grant's not-after allow.
func (c *Conn) write(ctx context.Context, frame []byte) error {
	if c.Expired() {
		return ErrGrantExpired
	}
	ctx, cancel := c.grantCtx(ctx)
	defer cancel()
	select {
	case c.writeSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("connection closed")
	}
	defer func() { <-c.writeSem }()
	return c.ws.Write(ctx, websocket.MessageText, frame)
}

// readPump is the connection's only reader. It routes the AUTH challenge
// and OK results; everything else is ignored in this slice. It never calls
// into callers or blocks on them.
func (c *Conn) readPump() {
	defer close(c.done)
	defer func() {
		if c.expiry != nil {
			c.expiry.Stop()
		}
	}()
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			return
		}
		env, err := nostr.ParseMessage(string(data))
		if err != nil {
			continue
		}
		switch e := env.(type) {
		case *nostr.AuthEnvelope:
			if e.Challenge != nil && strings.TrimSpace(*e.Challenge) != "" {
				select {
				case c.challenge <- *e.Challenge:
				default: // one challenge per connection is honored
				}
			}
		case *nostr.OKEnvelope:
			c.mu.Lock()
			w := c.waiters[e.EventID]
			c.mu.Unlock()
			if w != nil {
				select {
				case w <- okResult{ok: e.OK, reason: e.Reason}:
				default:
				}
			}
		}
	}
}
