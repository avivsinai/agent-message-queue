// Package ipc is the local carrier between the amq-remote CLI and the
// endpoint process: bounded, LF-framed JSON records over an owner-only Unix
// domain socket. One connection carries one request and one response, so a
// wait never blocks another client's cancel.
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// MaxRecordBytes bounds one IPC record; commands are already bounded by the
// protocol, and replies carry at most one bounded result.
const MaxRecordBytes = 1024 * 1024

// LocalHost is the authenticated source recorded for commands that arrive
// over the local socket: the OS user owning the socket.
const LocalHost = "local"

// Request is one IPC record from a client.
type Request struct {
	// Command is an ordinary protocol command, or nil when Wait is set.
	Command *protocol.Command `json:"command,omitempty"`
	// Wait blocks for a request to reach a terminal or uncertain state.
	Wait *WaitRequest `json:"wait,omitempty"`
}

// WaitRequest is the local-only wait operation.
type WaitRequest struct {
	RequestRef string `json:"request_ref"`
	TimeoutMS  int64  `json:"timeout_ms"`
}

// Response is one IPC record from the endpoint.
type Response struct {
	Reply json.RawMessage `json:"reply,omitempty"`
	Error *ErrorBody      `json:"error,omitempty"`
	// TimedOut is set when a wait ended without a terminal state.
	TimedOut bool `json:"timed_out,omitempty"`
}

// ErrorBody carries a typed refusal across the socket.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SocketPath returns the endpoint socket for a state directory. Unix socket
// paths are short-lived and length-limited, so the socket lives beside the
// state directory's lock rather than deep inside a mailbox tree.
func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, "endpoint.sock")
}

// Server accepts local connections and hands commands to the endpoint.
type Server struct {
	ep       *core.Endpoint
	listener net.Listener
	path     string
}

// Listen binds the socket with owner-only permissions. A stale socket file
// from a dead endpoint is removed only after a connect attempt fails.
func Listen(stateDir string, ep *core.Endpoint) (*Server, error) {
	path := SocketPath(stateDir)
	if _, err := os.Stat(path); err == nil {
		conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil, protocol.Refuse(protocol.CodeEndpointAlreadyRunning, "an endpoint is listening on %s", path)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return &Server{ep: ep, listener: l, path: path}, nil
}

// Path is the bound socket path.
func (s *Server) Path() string { return s.path }

// Serve accepts connections until ctx ends. Each connection is one request.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
	}()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				_ = os.Remove(s.path)
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	req, err := readRecord[Request](conn)
	if err != nil {
		writeRecord(conn, Response{Error: &ErrorBody{Code: string(protocol.CodeInvalid), Message: err.Error()}})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	switch {
	case req.Wait != nil:
		timeout := time.Duration(req.Wait.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = 24 * time.Hour
		}
		wctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		snap, err := s.ep.Wait(wctx, req.Wait.RequestRef)
		if errors.Is(err, context.DeadlineExceeded) {
			writeRecord(conn, Response{Reply: mustJSON(snap), TimedOut: true})
			return
		}
		if err != nil {
			writeRecord(conn, errorResponse(err))
			return
		}
		writeRecord(conn, Response{Reply: mustJSON(snap)})
	case req.Command != nil:
		reply, err := s.ep.Handle(req.Command, core.Source{Host: LocalHost})
		if err != nil {
			writeRecord(conn, errorResponse(err))
			return
		}
		writeRecord(conn, Response{Reply: mustJSON(reply)})
	default:
		writeRecord(conn, Response{Error: &ErrorBody{Code: string(protocol.CodeInvalid), Message: "request carries neither command nor wait"}})
	}
}

func errorResponse(err error) Response {
	var r *protocol.Refusal
	if errors.As(err, &r) {
		return Response{Error: &ErrorBody{Code: string(r.Code), Message: r.Message}}
	}
	return Response{Error: &ErrorBody{Code: "error", Message: err.Error()}}
}

func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return data
}

// Call sends one request to the endpoint at stateDir and returns its response.
// A missing endpoint is an action-required refusal that names the fix.
func Call(stateDir string, req Request) (*Response, error) {
	path := SocketPath(stateDir)
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "no endpoint at %s; start one with `amq-remote serve`", path)
	}
	defer func() { _ = conn.Close() }()
	writeRecord(conn, req)
	resp, err := readRecord[Response](conn)
	if err != nil {
		return nil, fmt.Errorf("read endpoint response: %w", err)
	}
	return resp, nil
}

// AsError converts a response error body back into a typed refusal.
func (r *Response) AsError() error {
	if r == nil || r.Error == nil {
		return nil
	}
	if r.Error.Code == "error" {
		return errors.New(r.Error.Message)
	}
	return &protocol.Refusal{Code: protocol.Code(r.Error.Code), Message: r.Error.Message}
}

func readRecord[T any](r io.Reader) (*T, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	line, err := br.ReadBytes('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return nil, fmt.Errorf("read record: %w", err)
	}
	if len(line) > MaxRecordBytes {
		return nil, fmt.Errorf("record exceeds %d bytes", MaxRecordBytes)
	}
	var v T
	if err := json.Unmarshal(line, &v); err != nil {
		return nil, fmt.Errorf("decode record: %w", err)
	}
	return &v, nil
}

// rpcWriteDeadline bounds each response write. A client that stops reading
// must not hold a handler goroutine forever: the write aborts at the
// deadline, the handler returns, and conn.Close (deferred by handle) releases
// the connection without needing the blocked writer to progress. Local
// responses are small; 10s is generous headroom over any sane reader.
const rpcWriteDeadline = 10 * time.Second

// writeRecord marshals one response and writes it under a write deadline when
// the connection supports one (Pro B14). A timed-out or failed write is
// silent from the protocol's point of view: the client sees a closed
// connection, never a partial record.
func writeRecord(w io.Writer, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	if d, ok := w.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if derr := d.SetWriteDeadline(time.Now().Add(rpcWriteDeadline)); derr != nil {
			return
		}
		defer func() { _ = d.SetWriteDeadline(time.Time{}) }()
	}
	_, _ = w.Write(data)
}
