package relay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fiatjaf.com/nostr"
)

// Subscription bounds (relay design §3).
const (
	maxSubscriptions = 32
	subBuffer        = 256
	// closeWriteTimeout bounds the best-effort CLOSE frame.
	closeWriteTimeout = 5 * time.Second
)

// ErrSubscriptionClosed is a CLOSED from the relay for a subscription.
var ErrSubscriptionClosed = errors.New("relay closed the subscription")

// ErrOverflow closes a subscription whose consumer fell behind. Events are
// never dropped silently: the consumer re-subscribes from its own cursor.
var ErrOverflow = errors.New("relay subscription overflowed; resubscribe from your cursor")

// Sub is one REQ on one connection. Events carries only events whose id
// and signature verify and that match the subscription's filters. EOSE is
// closed when the relay reports end of stored events, which is an
// observation, never proof of complete history.
type Sub struct {
	// ID is the subscription id on the wire: the caller's label and a
	// number the connection uses once.
	ID      string
	Events  <-chan nostr.Event
	EOSE    <-chan struct{}
	conn    *Conn
	filters []nostr.Filter
	events  chan nostr.Event
	eose    chan struct{}
	eoseSet bool
	done    chan struct{}
	err     error
}

// Done is closed when the subscription ends: CLOSED from the relay,
// overflow, Close, or the connection ending.
func (s *Sub) Done() <-chan struct{} { return s.done }

// Err is why the subscription ended, once Done is closed.
func (s *Sub) Err() error {
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()
	return s.err
}

// Subscribe sends REQ with the given filters. label names the subscription
// for a reader. Its id on the wire is never used again on this connection: a
// relay may answer CLOSE with CLOSED, and that frame must not end a later
// subscription of the same label.
func (c *Conn) Subscribe(ctx context.Context, label string, filters ...nostr.Filter) (*Sub, error) {
	if label == "" || len(filters) == 0 {
		return nil, errors.New("relay subscribe: a label and at least one filter are required")
	}
	s := &Sub{
		conn: c, filters: filters,
		events: make(chan nostr.Event, subBuffer),
		eose:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	s.Events, s.EOSE = s.events, s.eose
	if c.cfg.expiredAt(c.cfg.Now()) {
		return nil, ErrGrantExpired
	}
	c.mu.Lock()
	if c.readErr != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("relay connection closed: %w", c.readErr)
	}
	if c.subs == nil {
		c.subs = map[string]*Sub{}
	}
	if len(c.subs) >= maxSubscriptions {
		c.mu.Unlock()
		return nil, errors.New("relay subscribe: too many open subscriptions")
	}
	c.subSeq++
	s.ID = fmt.Sprintf("%s-%d", label, c.subSeq)
	c.subs[s.ID] = s
	c.mu.Unlock()
	frame, err := nostr.ReqEnvelope{SubscriptionID: s.ID, Filters: filters}.MarshalJSON()
	if err == nil && len(frame) > c.cfg.MaxOutbound {
		err = fmt.Errorf("REQ is %d bytes, over the %d-byte bound", len(frame), c.cfg.MaxOutbound)
	}
	if err == nil {
		err = c.write(ctx, frame)
	}
	if err != nil {
		c.endSub(s, err)
		return nil, fmt.Errorf("relay subscribe: %w", err)
	}
	return s, nil
}

// Close sends CLOSE and ends the subscription.
func (s *Sub) Close() {
	frame, _ := nostr.CloseEnvelope(s.ID).MarshalJSON()
	ctx, cancel := context.WithTimeout(context.Background(), closeWriteTimeout)
	_ = s.conn.write(ctx, frame)
	cancel()
	s.conn.endSub(s, nil)
}

// endSub removes and closes a subscription once.
func (c *Conn) endSub(s *Sub, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subs[s.ID] != s {
		return
	}
	delete(c.subs, s.ID)
	s.err = err
	close(s.done)
}

// routeEvent delivers a verified, filter-matching event to its
// subscription without blocking the read pump; a full queue ends the
// subscription with ErrOverflow.
func (c *Conn) routeEvent(subID string, evt nostr.Event) {
	c.mu.Lock()
	s := c.subs[subID]
	c.mu.Unlock()
	if s == nil || !evt.CheckID() || !evt.VerifySignature() || !s.matches(evt) {
		return
	}
	select {
	case s.events <- evt:
	default:
		c.endSub(s, ErrOverflow)
	}
}

func (s *Sub) matches(evt nostr.Event) bool {
	for _, f := range s.filters {
		if f.Matches(evt) {
			return true
		}
	}
	return false
}

// routeEOSE marks end of stored events for a subscription.
func (c *Conn) routeEOSE(subID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.subs[subID]; s != nil && !s.eoseSet {
		s.eoseSet = true
		close(s.eose)
	}
}

// routeClosed ends a subscription the relay closed.
func (c *Conn) routeClosed(subID, reason string) {
	c.mu.Lock()
	s := c.subs[subID]
	c.mu.Unlock()
	if s != nil {
		// The reason is relay-controlled text: it rides in RemoteError, whose
		// Error() is only the category, so it never reaches a status file.
		c.endSub(s, &RemoteError{Kind: ErrSubscriptionClosed, Reason: reason})
	}
}

// endAllSubs ends every subscription when the connection ends.
func (c *Conn) endAllSubs(err error) {
	c.mu.Lock()
	subs := make([]*Sub, 0, len(c.subs))
	for _, s := range c.subs {
		subs = append(subs, s)
	}
	c.mu.Unlock()
	for _, s := range subs {
		c.endSub(s, err)
	}
}
