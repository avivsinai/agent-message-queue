package relay

import (
	"context"
	"math/rand/v2"
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
		c.status.Err = err.Error()
	}
	c.conn = conn
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
			select {
			case <-conn.Done():
				c.set(StateUnavailable, conn.Err(), nil)
			case <-ctx.Done():
				conn.Close()
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
