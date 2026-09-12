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

// callReadDeadline bounds a client's wait for the endpoint's response. A wait
// request adds its own server-side timeout on top of this.
const callReadDeadline = 30 * time.Second

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
		// Wait never cancels work, so NO context error may be reported as
		// failure. DeadlineExceeded is the caller's own timeout; Canceled is
		// the endpoint shutting down underneath a live wait. Both mean "not
		// observed yet" (exit 4), never "the work failed" (exit 1) — an
		// orchestrator told its request failed during a routine endpoint
		// restart would take a destructive recovery path for a request that
		// is still running (agent-message-queue-611.22.27).
		if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
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
	// DialTimeout bounds only the connect. Without a read deadline every CLI
	// verb (submit, status, cancel, sessions, doctor) blocks forever against a
	// wedged endpoint. A wait carries its own server-side timeout, so allow
	// for it plus slack rather than cutting a legitimate long wait short.
	readBound := callReadDeadline
	if req.Wait != nil && req.Wait.TimeoutMS > 0 {
		readBound = time.Duration(req.Wait.TimeoutMS)*time.Millisecond + callReadDeadline
	}
	_ = conn.SetReadDeadline(time.Now().Add(readBound))
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
	// Cap at the BOUNDARY, not after the fact: ReadBytes grows without limit,
	// so a peer streaming megabytes with no newline could allocate freely
	// inside the read deadline before the size check ever ran. One extra byte
	// is read so an over-long record is still detected rather than silently
	// truncated into a parse error.
	br := bufio.NewReaderSize(io.LimitReader(r, MaxRecordBytes+1), 64*1024)
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

// writeResponseDeadline bounds one response write. A client that sends its
// request and then stops reading (a stopped shell, a wedged reader) used to
// block the handler goroutine forever on a full socket buffer, leaking the
// goroutine and its fd for the life of the process.
const writeResponseDeadline = 10 * time.Second

func writeRecord(w io.Writer, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	// Bound the write when the writer is a connection: an unread socket must
	// not hold a handler (or a CLI) indefinitely.
	if c, ok := w.(net.Conn); ok {
		_ = c.SetWriteDeadline(time.Now().Add(writeResponseDeadline))
		defer func() { _ = c.SetWriteDeadline(time.Time{}) }()
	}
	_, _ = w.Write(data)
}
