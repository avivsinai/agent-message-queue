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

	// reqQ is the reply-required server-request queue (B14b/B8). Server
	// requests (approvals, user input) must never be silently dropped: the
	// app-server waits for a response. The queue is bounded by the liveness
	// invariant — at most one in-flight approval per live run — and dispatch
	// to it NEVER blocks the read pump. On the should-be-impossible overflow
	// the request is failed explicitly (Respond with an error) so the server
	// never waits silently.
	reqQ          chan ServerRequest
	reqWorkerDone chan struct{} // closed by the req worker on exit
	// reqInFlight counts server-request handlers currently executing, so
	// Close's bounded drain awaits the executing handler too.
	reqInFlight atomic.Int64
	stopOnce    sync.Once

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
		reqQ:          make(chan ServerRequest, maxLiveRuns+reqQSlack),
		reqWorkerDone: make(chan struct{}),
	}
	go c.readLoop()
	go c.reqWorker()
	return c
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
	// The failure write is deadline-bounded: it runs INSIDE the read pump,
	// so an unbounded write on a wedged socket would stall the pump — the
	// exact thing B8 exists to prevent, on B8's own overflow path.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.respondErrorCtx(ctx, sr.ID, "server request queue overflow: "+sr.Method)
}

// respondError answers a server request with an error result (JSON-RPC 2.0:
// error responses carry error, never result — sending both would make the
// server-side parser pick one arbitrarily).
func (c *Client) respondErrorCtx(ctx context.Context, id json.RawMessage, msg string) error {
	errObj := rpcError{Code: -32000, Message: msg}
	msg2 := rpcMessage{JSONRPC: "2.0", ID: &id, Error: &errObj}
	data, err := json.Marshal(msg2)
	if err != nil {
		return err
	}
	return c.ws.writeTextCtx(ctx, data)
}

// reqWorker drains the reply-required queue. It exits when reqQ is closed.
func (c *Client) reqWorker() {
	defer close(c.reqWorkerDone)
	for sr := range c.reqQ {
		c.reqInFlight.Add(1)
		func() {
			defer func() {
				_ = recover()
				c.reqInFlight.Add(-1)
			}()
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
		if len(c.reqQ) == 0 && c.reqInFlight.Load() == 0 {
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
				// Synchronous, by design (B14b recut): onNative is a bounded
				// state-apply (B14a moved the native ack outside e.mu), so it
				// cannot wedge the pump — only reply-required approval
				// handlers can, and those run off-pump via reqQ.
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
	// ORDER IS THE BLOCKER FIX: the socket closes FIRST and the read pump is
	// DRAINED (readLoop closes c.closed on exit) BEFORE reqQ closes. The
	// previous order (close queues -> drain -> ws.close) left the pump live
	// during the whole drain window, so an inbound approval frame could send
	// on a CLOSED reqQ and panic the process. After the pump is dead,
	// dispatchServerRequest can no longer fire and closing reqQ is safe.
	c.stopOnce.Do(func() {
		_ = c.ws.close() // aborts any blocked read/write immediately
		<-c.closed       // readLoop exited: no more reqQ sends
		close(c.reqQ)    // only Close closes it; pump is gone
	})
	<-c.reqWorkerDone
	c.waitCallbacks()
	return nil
}
