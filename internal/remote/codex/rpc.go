package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// rpcMessage is one JSON-RPC 2.0 message in either direction. The app-server
// omits "jsonrpc" on some notifications, so it is optional on decode.
type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc,omitempty"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("app-server error %d: %s", e.Code, e.Message) }

// ServerRequest is a request the app-server sends to us (approvals, user
// input). The handler answers through Respond.
type ServerRequest struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// Notification is a server-initiated notification.
type Notification struct {
	Method string
	Params json.RawMessage
}

// Client multiplexes one WebSocket connection: requests get correlated
// responses, notifications and server requests fan out to the handlers set
// before Run starts reading.
type Client struct {
	ws      *wsConn
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcMessage
	closed  chan struct{}
	once    sync.Once
	readErr error

	// events is the ordered, bounded callback queue drained by the single
	// worker goroutine (Pro B14 recut #4/#6). Closed by Close.
	events     chan event
	workerOnce sync.Once

	OnNotification  func(Notification)
	OnServerRequest func(ServerRequest)
}

// Dial connects to the app-server unix socket and starts the read loop.
func Dial(socketPath string) (*Client, error) {
	ws, err := dialUnixWS(socketPath, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial app-server %s: %w", socketPath, err)
	}
	return newClient(ws), nil
}

func newClient(ws *wsConn) *Client {
	c := &Client{ws: ws, pending: map[int64]chan rpcMessage{}, closed: make(chan struct{}), events: make(chan event, callbackBacklog)}
	go c.readLoop()
	c.workerOnce.Do(func() { go c.startWorker() })
	return c
}

func (c *Client) readLoop() {
	for {
		payload, err := c.ws.readText()
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			c.once.Do(func() { close(c.closed) })
			return
		}
		var msg rpcMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		switch {
		case msg.ID != nil && msg.Method == "":
			var id int64
			if err := json.Unmarshal(*msg.ID, &id); err != nil {
				continue
			}
			c.mu.Lock()
			ch, ok := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ok {
				ch <- msg
			}
		case msg.ID != nil:
			if c.OnServerRequest != nil {
				sr := ServerRequest{ID: *msg.ID, Method: msg.Method, Params: msg.Params}
				c.dispatch(func() { c.OnServerRequest(sr) })
			}
		case msg.Method != "":
			if c.OnNotification != nil {
				n := Notification{Method: msg.Method, Params: msg.Params}
				c.dispatch(func() { c.OnNotification(n) })
			}
		}
	}
}

// event carries one reader callback to the ordered worker. done is closed
// after run returns, so Close can wait for the in-flight event.
type event struct {
	run  func()
	done chan struct{}
}

// callbackBacklog bounds the ordered worker's queue. It is comfortably
// larger than any real app-server's burst: the reader enqueues only while a
// callback is mid-flight, and a healthy handler drains far faster than
// frames arrive. Overflow policy: the read pump DROPS the event — dropping
// one notification is recoverable (the endpoint's Reconcile/Tick re-derives
// state from the durable record); blocking the read pump is not (a stalled
// pump starves every pending Call on the connection).
const callbackBacklog = 64

// startWorker runs the single ordered callback worker (Pro B14 recut
// #4/#6): events apply strictly in arrival order — Question then
// QuestionResolved cannot invert — and the read pump never blocks on
// handler latency. The worker recovers its own panic so a callback bug
// cannot kill the read pump and every pending call with it. It exits when
// the events channel is closed.
func (c *Client) startWorker() {
	for ev := range c.events {
		func() {
			defer func() { _ = recover() }()
			defer close(ev.done)
			ev.run()
		}()
	}
}

// dispatch hands one reader callback to the ordered worker with a bounded,
// non-blocking enqueue. Overflow drops the event rather than wedging the
// pump.
func (c *Client) dispatch(run func()) {
	ev := event{run: run, done: make(chan struct{})}
	select {
	case c.events <- ev:
	default:
		// Backlog full: drop. The endpoint re-derives dropped state via
		// Reconcile; blocking here would stall the read pump.
		close(ev.done)
	}
}

// waitCallbacks drains the ordered worker's backlog and waits for the
// in-flight event, so Close does not return while callbacks still touch
// app state. Bounded by one callback duration plus backlog drain time; the
// deadline below keeps Close responsive if a handler is wedged.
func (c *Client) waitCallbacks() {
	deadline := time.After(2 * time.Second)
	for {
		c.mu.Lock()
		depth := len(c.events)
		c.mu.Unlock()
		if depth == 0 {
			return
		}
		select {
		case <-deadline:
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Done closes when the connection is gone.
func (c *Client) Done() <-chan struct{} { return c.closed }

// Call sends a request and waits for its response or ctx.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	idRaw := json.RawMessage(fmt.Sprintf("%d", id))
	msg := rpcMessage{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: raw}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ch := make(chan rpcMessage, 1)
	c.mu.Lock()
	if c.readErr != nil {
		c.mu.Unlock()
		return fmt.Errorf("app-server connection closed: %w", c.readErr)
	}
	c.pending[id] = ch
	c.mu.Unlock()
	// Bound the write itself by the context: a blocked socket write must
	// abort at the deadline, not hang until the connection is closed (Pro
	// B14). writeTextCtx installs+clears the deadline under the writer
	// mutex so concurrent writers cannot clobber each other's deadline
	// (recut #5).
	if err := c.ws.writeTextCtx(ctx, data); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return errors.New("app-server connection closed before reply")
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// Respond answers a server request.
func (c *Client) Respond(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	msg := rpcMessage{JSONRPC: "2.0", ID: &id, Result: raw}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.ws.writeText(data)
}

// Close closes the connection.
// Close tears the connection down and stops the callback worker: the
// events channel closes (worker exits after the in-flight event; Close
// waits, bounded), then the conn closes — aborting any blocked read or
// write immediately.
func (c *Client) Close() error {
	c.workerOnce.Do(func() { close(c.events) })
	c.waitCallbacks()
	err := c.ws.close()
	// Safe double-close of the events channel if Close runs again: the
	// workerOnce guarantees it happens at most once.
	return err
}
