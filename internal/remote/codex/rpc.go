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
	c := &Client{ws: ws, pending: map[int64]chan rpcMessage{}, closed: make(chan struct{})}
	go c.readLoop()
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
				c.OnServerRequest(ServerRequest{ID: *msg.ID, Method: msg.Method, Params: msg.Params})
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
	// Bound the write itself by the context: a blocked socket write must abort
	// at the deadline, not hang until the connection is closed (Pro B14).
	if dl, ok := ctx.Deadline(); ok {
		c.ws.setWriteDeadline(dl)
	}
	if err := c.ws.writeText(data); err != nil {
		c.ws.setWriteDeadline(time.Time{})
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	c.ws.setWriteDeadline(time.Time{})
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
func (c *Client) Close() error { return c.ws.close() }
