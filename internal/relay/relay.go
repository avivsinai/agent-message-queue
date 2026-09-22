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

// ErrRejected is an explicit negative OK for a published event.
var ErrRejected = errors.New("relay rejected event")

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
	cfg     Config
	ws      *websocket.Conn
	writeMu sync.Mutex

	mu        sync.Mutex
	waiters   map[nostr.ID]chan okResult
	subs      map[string]*Sub
	challenge chan string
	done      chan struct{}
	readErr   error
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
	dctx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, cfg.URL, &websocket.DialOptions{})
	if err != nil {
		return nil, fmt.Errorf("relay dial: %w", err)
	}
	ws.SetReadLimit(cfg.MaxInbound)
	c := &Conn{
		cfg:       cfg,
		ws:        ws,
		waiters:   map[nostr.ID]chan okResult{},
		challenge: make(chan string, 1),
		done:      make(chan struct{}),
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
		return nil, fmt.Errorf("%w: %s", ErrAuthRefused, res.reason)
	}
	return c, nil
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
		return fmt.Errorf("%w: %s", ErrRejected, res.reason)
	}
	return nil
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended, once Done is closed.
func (c *Conn) Err() error { return c.err() }

// Close ends the connection.
func (c *Conn) Close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }

func (c *Conn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// roundTrip registers a waiter for evt's OK, writes the envelope, and waits.
func (c *Conn) roundTrip(ctx context.Context, evt nostr.Event, env interface{ MarshalJSON() ([]byte, error) }) (okResult, error) {
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
	c.writeMu.Lock()
	err = c.ws.Write(ctx, websocket.MessageText, frame)
	c.writeMu.Unlock()
	if err != nil {
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

// readPump is the connection's only reader. It routes the AUTH challenge,
// OK results, and subscription EVENT/EOSE/CLOSED frames. It never calls
// into callers or blocks on them.
func (c *Conn) readPump() {
	defer close(c.done)
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			c.endAllSubs(err)
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
		case *nostr.EventEnvelope:
			if e.SubscriptionID != nil {
				c.routeEvent(*e.SubscriptionID, e.Event)
			}
		case *nostr.EOSEEnvelope:
			c.routeEOSE(e.SubscriptionID)
		case *nostr.ClosedEnvelope:
			c.routeClosed(e.SubscriptionID, e.Reason)
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
