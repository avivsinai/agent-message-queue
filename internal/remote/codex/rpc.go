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

// event carries one reader callback to the ordered worker. done is closed
// after run returns, so Close can wait for the in-flight event.
type event struct {
	run  func()
	done chan struct{}
}

// callbackBacklog bounds the ordered worker's queue. It is comfortably
// above the notification rate the app-server produces; blocking the read
// pump is not (a stalled pump starves every pending Call on the
// connection).
const callbackBacklog = 64

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
	// worker goroutine (B14b/B9). Start and stop have SEPARATE lifecycles:
	// sharing one sync.Once between newClient (start) and Close (stop) meant
	// close(events) never ran and the worker leaked on every teardown.
	events        chan event
	workerStart   sync.Once
	workerStop    sync.Once
	workerStarted atomic.Bool
	workerDone    chan struct{} // closed by the worker on exit (testable teardown)
	// eventsInFlight counts callbacks currently executing in the worker, so
	// waitCallbacks can await the in-flight event — len(events) alone
	// excludes it.
	eventsInFlight atomic.Int64

	// reqQ is the reply-required server-request queue (B14b/B8). Server
	// requests (approvals, user input) must never be silently dropped: the
	// app-server waits for a response. The queue is bounded by the liveness
	// invariant — at most one in-flight approval per live run — and dispatch
	// to it NEVER blocks the read pump. On the should-be-impossible overflow
	// the request is failed explicitly (Respond with an error) so the server
	// never waits silently.
	reqQ          chan ServerRequest
	reqQCapacity  int
	reqWorkerDone chan struct{} // closed by the req worker on exit

	OnNotification  func(Notification)
	OnServerRequest func(ServerRequest)
}

// reqQSlack is the slack above max live runs for the reply-required queue.
const reqQSlack = 8

// maxLiveRuns bounds the number of concurrent native runs the attachment
// tracks; approvals are per-run, so this bounds reply-required in-flight.
const maxLiveRuns = 16

// Dial connects to the app-server unix socket and starts the read loop.
func Dial(socketPath string) (*Client, error) {
	ws, err := dialUnixWS(socketPath, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial app-server %s: %w", socketPath, err)
	}
	return newClient(ws), nil
}

func newClient(ws *wsConn) *Client {
	c := &Client{
		ws:            ws,
		pending:       map[int64]chan rpcMessage{},
		closed:        make(chan struct{}),
		events:        make(chan event, callbackBacklog),
		workerDone:    make(chan struct{}),
		reqQ:          make(chan ServerRequest, maxLiveRuns+reqQSlack),
		reqQCapacity:  maxLiveRuns + reqQSlack,
		reqWorkerDone: make(chan struct{}),
	}
	go c.readLoop()
	go c.reqWorker()
	c.workerStart.Do(func() {
		c.workerStarted.Store(true)
		go c.startWorker()
	})
	return c
}

// startWorker runs the single ordered callback worker: events apply strictly
// in arrival order — Question then QuestionResolved cannot invert — and the
// read pump never blocks on handler latency. The worker recovers its own
// panic so a callback bug cannot kill the read pump and every pending call
// with it. It exits when the events channel is closed, then closes
// workerDone so tests can observe the exit deterministically.
func (c *Client) startWorker() {
	defer close(c.workerDone)
	for ev := range c.events {
		c.eventsInFlight.Add(1)
		func() {
			defer func() { _ = recover() }()
			defer close(ev.done)
			ev.run()
		}()
		c.eventsInFlight.Add(-1)
	}
}

// dispatch hands one reader callback to the ordered worker with a bounded,
// non-blocking enqueue. Overflow drops the event rather than wedging the
// pump (notifications may drop; reply-required frames never reach this path
// — they go through dispatchServerRequest).
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

// dispatchServerRequest enqueues a reply-required server request without
// ever blocking the read pump (B14b/B8). Overflow must not happen — the
// app-server has at most one in-flight approval per live run — but if it
// ever does, the request is FAILED EXPLICITLY (Respond with an error) so
// the app-server never waits for an answer that never comes.
func (c *Client) dispatchServerRequest(sr ServerRequest) {
	select {
	case c.reqQ <- sr:
		return
	default:
	}
	// Overflow: explicit failure, never a silent drop, never a blocked pump.
	_ = c.respondError(sr.ID, "server request queue overflow: "+sr.Method)
}

// respondError answers a server request with an error result (JSON-RPC 2.0:
// error responses carry error, never result — sending both would make the
// server-side parser pick one arbitrarily).
func (c *Client) respondError(id json.RawMessage, msg string) error {
	errObj := rpcError{Code: -32000, Message: msg}
	msg2 := rpcMessage{JSONRPC: "2.0", ID: &id, Error: &errObj}
	data, err := json.Marshal(msg2)
	if err != nil {
		return err
	}
	return c.ws.writeText(data)
}

// reqWorker drains the reply-required queue. It exits when reqQ is closed.
func (c *Client) reqWorker() {
	defer close(c.reqWorkerDone)
	for sr := range c.reqQ {
		func() {
			defer func() { _ = recover() }()
			if c.OnServerRequest != nil {
				c.OnServerRequest(sr)
			}
		}()
	}
}

// waitCallbacks drains both queues and waits for the in-flight callbacks,
// so Close does not return while callbacks still touch app state. Bounded:
// the deadline keeps Close responsive if a handler is wedged.
func (c *Client) waitCallbacks() {
	deadline := time.After(2 * time.Second)
	for {
		c.mu.Lock()
		depth := len(c.events) + len(c.reqQ)
		c.mu.Unlock()
		if depth == 0 && c.eventsInFlight.Load() == 0 {
			return
		}
		select {
		case <-deadline:
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
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
				c.dispatchServerRequest(ServerRequest{ID: *msg.ID, Method: msg.Method, Params: msg.Params})
			}
		case msg.Method != "":
			if c.OnNotification != nil {
				c.OnNotification(Notification{Method: msg.Method, Params: msg.Params})
			}
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
	// Bound BOTH phases of the write by the context (B14b/B6): WAITING for
	// the writer slot is acquired ctx-aware (the old wmu sync.Mutex waited
	// unboundedly — ctx only bounded the write after acquisition), and the
	// write itself carries the deadline under the slot.
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

// Close tears the connection down and stops the callback workers (B14b/B9):
// the events channel closes (worker exits after the in-flight event; Close
// waits, bounded), then the conn closes — aborting any blocked read or
// write immediately. The stop lifecycle is a separate sync.Once from the
// start Once, so it actually runs: the previous single-once design leaked
// the worker on every teardown. Safe to call twice.
func (c *Client) Close() error {
	c.workerStop.Do(func() {
		if c.workerStarted.Load() {
			close(c.events)
		}
		close(c.reqQ)
	})
	c.waitCallbacks()
	return c.ws.close()
}
