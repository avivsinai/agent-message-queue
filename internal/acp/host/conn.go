package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

// codeMethodNotFound answers an agent request for a client method the host
// does not implement or advertise (fs/*, terminal/*, anything else).
const codeMethodNotFound = -32601

// errClosed means the agent's stdout ended: the agent exited or closed its
// side of the pipe.
var errClosed = errors.New("the agent closed its connection")

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// envelope is any JSON-RPC object the agent writes: a response (id with
// result or error), a request (id with method), or a notification (method
// only).
type envelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type outgoing struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  any              `json:"params,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type reply struct {
	result json.RawMessage
	err    *rpcError
}

// conn is the client side of one ACP connection: newline-delimited JSON-RPC
// in both directions. One reader goroutine routes responses to their calls by
// id, answers the agent's requests through onRequest, and hands notifications
// to onNotify in arrival order, so every session/update the agent writes
// before a response is delivered before that response.
type conn struct {
	writeMu sync.Mutex
	out     *bufio.Writer
	enc     *json.Encoder

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply

	onRequest func(method string, params json.RawMessage) (any, *rpcError)
	onNotify  func(method string, params json.RawMessage)

	done   chan struct{}
	err    error
	closed bool
}

func newConn(in io.Reader, out io.Writer, onRequest func(string, json.RawMessage) (any, *rpcError), onNotify func(string, json.RawMessage)) *conn {
	w := bufio.NewWriter(out)
	c := &conn{
		out:       w,
		enc:       json.NewEncoder(w),
		pending:   make(map[int64]chan reply),
		onRequest: onRequest,
		onNotify:  onNotify,
		done:      make(chan struct{}),
	}
	go c.read(in)
	return c
}

func (c *conn) write(v outgoing) error {
	v.JSONRPC = "2.0"
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.enc.Encode(v); err != nil {
		return err
	}
	return c.out.Flush()
}

// call sends one request and waits for its response. sent, when not nil,
// runs after the request id is known and before any byte is written, so a
// caller can journal the id first.
func (c *conn) call(ctx context.Context, method string, params any, sent func(id int64) error) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, c.closedErr()
	}
	c.nextID++
	id := c.nextID
	ch := make(chan reply, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if sent != nil {
		if err := sent(id); err != nil {
			return nil, err
		}
	}
	raw := json.RawMessage(fmt.Sprint(id))
	if err := c.write(outgoing{ID: &raw, Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return r.result, nil
	case <-c.done:
		return nil, c.closedErr()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *conn) notify(method string, params any) error {
	return c.write(outgoing{Method: method, Params: params})
}

func (c *conn) closedErr() error {
	if c.err != nil {
		return fmt.Errorf("%w: %v", errClosed, c.err)
	}
	return errClosed
}

func (c *conn) read(in io.Reader) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), format.MaxMessageSize+1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg envelope
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
		switch {
		case msg.Method != "" && hasID:
			result, rerr := c.onRequest(msg.Method, msg.Params)
			id := msg.ID
			if rerr != nil {
				_ = c.write(outgoing{ID: &id, Error: rerr})
			} else {
				_ = c.write(outgoing{ID: &id, Result: result})
			}
		case msg.Method != "":
			c.onNotify(msg.Method, msg.Params)
		case hasID:
			var id int64
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			c.mu.Unlock()
			if ch != nil {
				ch <- reply{result: msg.Result, err: msg.Error}
			}
		}
	}
	c.mu.Lock()
	c.closed = true
	// A read end the host closed itself, after the agent exited, is an
	// ordinary end of stream.
	if err := scanner.Err(); !errors.Is(err, os.ErrClosed) {
		c.err = err
	}
	c.mu.Unlock()
	close(c.done)
}
