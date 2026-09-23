package relay

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// State is a relay surface's reported condition. Each is distinct: socket
// open is not authenticated, and authenticated is not proof that the relay
// materialized ownership or that any viewer is ready (relay design §2).
type State string

const (
	StateConfigured    State = "configured"
	StateAuthPending   State = "auth_pending"
	StateAuthenticated State = "authenticated"
	StateUnavailable   State = "unavailable"
)

// Status is a point-in-time view of a Client.
type Status struct {
	State State
	// Err is the last connection or authentication failure, if any.
	Err string
	// Since is when State was entered.
	Since time.Time
}

// ConfigFunc produces a fresh connection config for each attempt, so a
// renewed enrollment is picked up on reconnect and an expired one stops
// authentication rather than riding an old socket.
type ConfigFunc func() (Config, error)

// Client keeps one authenticated connection alive: connect, AUTH, and on
// loss reconnect with capped jittered backoff (1–30 s). No challenge, OK
// waiter or authenticated flag crosses a reconnect.
type Client struct {
	config ConfigFunc
	// OnConnect, if set, runs for each authenticated connection in its own
	// goroutine; its context ends when that connection does. Subscriptions
	// and outbox flushing live here, so nothing crosses a reconnect.
	OnConnect func(ctx context.Context, conn *Conn)

	mu     sync.Mutex
	status Status
	conn   *Conn

	// for tests
	minBackoff, maxBackoff time.Duration
}

// NewClient returns a client in state configured.
func NewClient(config ConfigFunc) *Client {
	return &Client{
		config:     config,
		status:     Status{State: StateConfigured, Since: time.Now()},
		minBackoff: time.Second,
		maxBackoff: 30 * time.Second,
	}
}

// Status returns the current status.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Conn returns the current authenticated connection, or nil.
func (c *Client) Conn() *Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

func (c *Client) set(state State, err error, conn *Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = Status{State: state, Since: time.Now()}
	if err != nil {
		c.status.Err = Category(err)
	}
	c.conn = conn
}

// identityCheck is how often a live connection's enrolled identity is
// re-validated for removal or renewal (codex slice 1 review #2). Expiry is
// not polled: the connection itself closes at the grant's not-after.
var identityCheck = time.Minute

// errIdentityChanged ends a connection whose enrolled grant expired, was
// removed, or changed; the reconnect re-authenticates under the current
// one, or fails.
var errIdentityChanged = errors.New("enrolled identity changed")

// hold keeps an authenticated connection until it ends, ctx ends, or the
// enrolled identity stops matching the one it authenticated with. It
// reports whether ctx ended.
func (c *Client) hold(ctx context.Context, conn *Conn, authed Config) bool {
	t := time.NewTicker(identityCheck)
	defer t.Stop()
	for {
		select {
		case <-conn.Done():
			c.set(StateUnavailable, conn.Err(), nil)
			return false
		case <-ctx.Done():
			conn.Close()
			return true
		case <-t.C:
			cur, err := c.config()
			if err != nil || !sameIdentity(authed, cur) {
				conn.Close()
				if err == nil {
					err = errIdentityChanged
				}
				c.set(StateUnavailable, err, nil)
				return false
			}
		}
	}
}

func sameIdentity(a, b Config) bool {
	if a.URL != b.URL || a.Secret != b.Secret || !a.NotAfter.Equal(b.NotAfter) || len(a.AuthTag) != len(b.AuthTag) {
		return false
	}
	for i := range a.AuthTag {
		if a.AuthTag[i] != b.AuthTag[i] {
			return false
		}
	}
	return true
}

// Category maps an error to a fixed, AMQ-worded category for status files:
// relay-controlled text (OK reasons, close reasons) never reaches them
// (codex slice 1 review #9). Local configuration errors keep their text.
func Category(err error) string {
	var remote *RemoteError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &remote):
		return remote.Kind.Error()
	case errors.Is(err, ErrGrantExpired):
		return ErrGrantExpired.Error()
	case errors.Is(err, errIdentityChanged):
		return "enrolled identity changed; re-authenticating"
	case errors.Is(err, ErrUnknownDelivery):
		return ErrUnknownDelivery.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "relay timed out"
	case strings.HasPrefix(err.Error(), "relay dial"):
		return "relay unreachable"
	case strings.HasPrefix(err.Error(), "share "), strings.HasPrefix(err.Error(), "relay: an enrolled"):
		return err.Error() // local configuration text, not relay-controlled
	default:
		return "relay connection lost"
	}
}

// Run connects and stays connected until ctx ends. It returns nil on
// cancellation; it never returns on transport failure, only backs off.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.minBackoff
	for {
		c.set(StateAuthPending, nil, nil)
		start := time.Now()
		cfg, err := c.config()
		var conn *Conn
		if err == nil {
			conn, err = Connect(ctx, cfg)
		}
		if err != nil {
			if ctx.Err() != nil {
				c.set(StateUnavailable, nil, nil)
				return nil
			}
			c.set(StateUnavailable, err, nil)
		} else {
			c.set(StateAuthenticated, nil, conn)
			connCtx, connCancel := context.WithCancel(ctx)
			hookDone := make(chan struct{})
			if c.OnConnect != nil {
				go func() {
					defer close(hookDone)
					c.OnConnect(connCtx, conn)
				}()
			} else {
				close(hookDone)
			}
			ended := c.hold(ctx, conn, cfg)
			connCancel()
			<-hookDone
			if ended {
				c.set(StateUnavailable, nil, nil)
				return nil
			}
			// A connection that stayed up for a while resets the backoff.
			if time.Since(start) > c.maxBackoff {
				backoff = c.minBackoff
			}
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
		if wait > c.maxBackoff {
			wait = c.maxBackoff
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		if backoff *= 2; backoff > c.maxBackoff {
			backoff = c.maxBackoff
		}
	}
}
