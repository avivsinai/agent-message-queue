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

// callReadDeadline bounds a client's wait for the endpoint's response. It
// protects the SHORT verbs (submit, status, cancel, sessions, doctor) from a
// wedged endpoint. A wait request adds its own server-side timeout on top of
// this, or opts out of a client deadline entirely; see Call.
// A test shortens it to prove the wait path does not inherit it.
var callReadDeadline = 30 * time.Second

// LocalHost is the authenticated source recorded for commands that arrive
// over the local socket: the OS user owning the socket.
const LocalHost = core.LocalHost

// Request is one IPC record from a client.
type Request struct {
	// Command is an ordinary protocol command, or nil when Wait is set.
	Command *protocol.Command `json:"command,omitempty"`
	// Wait blocks for a request to reach a terminal or uncertain state.
	Wait *WaitRequest `json:"wait,omitempty"`
	// NativeSession, when set, requires the command's target to be attached
	// to exactly this native session; any other session refuses with
	// unshared before the endpoint sees the command, and a submit stays
	// bound to it until its handoff. Local-only: the native identity never
	// enters the session wire schema.
	NativeSession string `json:"native_session,omitempty"`
	// Native asks which native session a target is attached to, so a local
	// sharing choice can pin it. Local-only, like NativeSession.
	Native *NativeQuery `json:"native,omitempty"`
	// Register attaches one more adapter to the running endpoint, the way
	// startup attaches manifest entries, and persists it for restarts.
	// Local-only: `amq-remote attach --self` sends it for the session the
	// owner is typing in.
	Register *RegisterRequest `json:"register,omitempty"`
	// Link calls a linked server's tools (operation link.v1). Local-only:
	// the endpoint's own link carries it, with the device key that never
	// leaves the endpoint.
	Link *LinkRequest `json:"link.v1,omitempty"`
	// Features asks which features the endpoint serves. An endpoint that
	// predates the question answers it as a request with no operation.
	Features bool `json:"features,omitempty"`
	// Activity streams the native activity of one running request. Local-only
	// and read-only: see ActivityRequest.
	Activity *ActivityRequest `json:"activity,omitempty"`
	// Evidence is Command's opaque answer proof (protocol.Command.Evidence),
	// carried for the Buzz Desktop agent, which relays the owner's signed ✅
	// over this socket. Local-only, like NativeSession. It grants nothing by
	// itself: the attachment's verifier decides what it proves.
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// ActivityRequest asks for the native activity of one request while it runs:
// streamed text, tool calls and plan updates of the target's attached
// session, as ACP session update objects. It is the one operation whose
// connection carries many response records: each record's Reply is one
// update, the stream ends with EOF once the request is terminal or uncertain,
// and an error record ends it with a refusal. Nothing on it can submit,
// cancel or answer anything.
type ActivityRequest struct {
	RequestRef string `json:"request_ref"`
}

// ActivityHandler serves an activity stream. send writes one update record
// and fails once the client is gone; ctx ends when the client closes the
// connection or the endpoint stops. native is the request's native pin, or ""
// when the request carries none.
type ActivityHandler func(ctx context.Context, req ActivityRequest, native string, send func(json.RawMessage) error) error

// LinkRequest is one link.v1 operation: list the server's tools, call one,
// or read a call's state. WaitMS bounds how long the endpoint waits for the
// server; the client's read deadline is that wait plus slack.
type LinkRequest struct {
	Name           string          `json:"name,omitempty"`
	Op             string          `json:"op"` // tools, call, call_get, held, page, page_result
	CallID         string          `json:"call_id,omitempty"`
	Tool           string          `json:"tool,omitempty"`
	Arguments      json.RawMessage `json:"arguments,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	WaitMS         int64           `json:"wait_ms,omitempty"`
	// Page asks the endpoint for one of its local pages, register or
	// confirm (ConfirmID names the held task). Token names a page for
	// page_result. No operation carries an assertion: only the endpoint's own
	// page takes one.
	Page      string `json:"page,omitempty"`
	ConfirmID string `json:"confirm_id,omitempty"`
	Token     string `json:"token,omitempty"`
}

// LinkHandler serves link.v1 for the endpoint's links.
type LinkHandler func(ctx context.Context, req LinkRequest) (json.RawMessage, error)

// LinkRecordBytes bounds one complete link.v1 record, envelope included: a
// tool result as large as a link frame (4 MiB) plus room for the envelope.
// Every other operation keeps MaxRecordBytes, checked after the read: the
// server reads any record up to this bound before it knows the operation.
// The socket is the owner's alone (0600), so that larger read is the owner's
// own cost.
const LinkRecordBytes = 4<<20 + 64*1024

// linkSlack is added to a link.v1 wait for the client's read deadline.
const linkSlack = 10 * time.Second

// RegisterRequest names one adapter by kind, target and config, exactly as
// a manifest entry does.
type RegisterRequest struct {
	Kind   string          `json:"kind"`
	Target string          `json:"target"`
	Config json.RawMessage `json:"config,omitempty"`
	// Epoch is accepted only for kind fake, as in the manifest.
	Epoch string `json:"epoch,omitempty"`
}

// Registrar attaches one adapter and returns its session. serve supplies it;
// without one the endpoint refuses Register.
type Registrar func(RegisterRequest) (protocol.Session, error)

// NativeQuery names the target whose native session is asked for.
type NativeQuery struct {
	TargetID string `json:"target_id"`
}

// NativeReply is the reply to a NativeQuery.
type NativeReply struct {
	NativeSession string `json:"native_session"`
}

// WaitRequest is the local-only wait operation.
type WaitRequest struct {
	RequestRef string `json:"request_ref"`
	TimeoutMS  int64  `json:"timeout_ms"`
	// AfterRevision, when set, also ends the wait once the request's
	// revision is past it, so a follower sees each pending interaction.
	// Absent keeps the terminal or uncertain wait. Local-only.
	AfterRevision *int64 `json:"after_revision,omitempty"`
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

// Features is the endpoint's answer to a features request.
type Features struct {
	// Link: this endpoint serves links and keeps a manifest's links block.
	Link bool `json:"link"`
}

// EndpointFeatures asks the endpoint at stateDir which features it serves.
// An endpoint that predates the question reports none; no endpoint at all is
// ok=false.
func EndpointFeatures(stateDir string) (f Features, ok bool, err error) {
	resp, err := Call(stateDir, Request{Features: true})
	if err != nil {
		var r *protocol.Refusal
		if errors.As(err, &r) && r.Code == protocol.CodeEndpointUnreachable {
			return Features{}, false, nil
		}
		return Features{}, false, err
	}
	if resp.Error != nil {
		return Features{}, true, nil // an older endpoint: it knows no features
	}
	err = json.Unmarshal(resp.Reply, &f)
	return f, true, err
}

// ErrSocketPathTooLong is returned by Listen when the computed socket path
// would exceed the Unix sun_path limit, which would cause bind to fail with
// a bare EINVAL. The limit is platform-specific: maxUnixSocketPathLen
// (108 on Linux, 104 on macOS) is defined in platform-tagged files.
var ErrSocketPathTooLong = errors.New("socket path exceeds unix sun_path limit")

// SocketPath returns the endpoint socket for a state directory. Unix socket
// paths are short-lived and length-limited, so the socket lives beside the
// state directory's lock rather than deep inside a mailbox tree.
func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, "endpoint.sock")
}

// Server accepts local connections and hands commands to the endpoint.
type Server struct {
	ep        *core.Endpoint
	listener  net.Listener
	path      string
	registrar Registrar
	link      LinkHandler
	activity  ActivityHandler
}

// SetActivityHandler installs the activity stream handler. Call it before
// Serve.
func (s *Server) SetActivityHandler(h ActivityHandler) { s.activity = h }

// SetLinkHandler installs the link.v1 handler. Call it before Serve.
func (s *Server) SetLinkHandler(h LinkHandler) { s.link = h }

// SetRegistrar installs the live registration handler. Call it before Serve.
func (s *Server) SetRegistrar(r Registrar) { s.registrar = r }

// Listen binds the socket with owner-only permissions. A stale socket file
// from a dead endpoint is removed only after a connect attempt fails.
func Listen(stateDir string, ep *core.Endpoint) (*Server, error) {
	path := SocketPath(stateDir)
	if len(path) >= maxUnixSocketPathLen {
		usable := maxUnixSocketPathLen - 1 // NUL terminator
		return nil, fmt.Errorf("%w: path is %d bytes, max usable is %d (suffix \"endpoint.sock\" is %d bytes); use a shorter --root (or AM_ROOT) so the state directory is closer to the filesystem root",
			ErrSocketPathTooLong, len(path), usable, len("endpoint.sock"))
	}
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
	req, n, err := readRecordLimit[Request](conn, LinkRecordBytes)
	if err == nil && req.Link == nil && n > MaxRecordBytes {
		err = fmt.Errorf("record exceeds %d bytes", MaxRecordBytes)
	}
	if err != nil {
		writeRecord(conn, Response{Error: &ErrorBody{Code: string(protocol.CodeInvalid), Message: err.Error()}})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	// An activity record carries no other operation: checkPin fences the
	// activity ref, so another operation must not ride on that check.
	if req.Activity != nil && (req.Command != nil || req.Wait != nil || req.Native != nil || req.Register != nil || req.Link != nil || req.Features) {
		writeRecord(conn, Response{Error: &ErrorBody{Code: string(protocol.CodeInvalid), Message: "activity request carries another operation"}})
		return
	}
	if err := s.checkPin(req); err != nil {
		writeRecord(conn, errorResponse(err))
		return
	}
	switch {
	case req.Wait != nil:
		timeout := time.Duration(req.Wait.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = 24 * time.Hour
		}
		wctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		snap, err := s.ep.WaitAfter(wctx, req.Wait.RequestRef, req.Wait.AfterRevision)
		// Wait never cancels work, so NO context error may be reported as
		// failure. DeadlineExceeded is the caller's own timeout; Canceled is
		// the endpoint shutting down underneath a live wait. Both mean "not
		// observed yet" (exit 4), never "the work failed" (exit 1) — an
		// orchestrator told its request failed during a routine endpoint
		// restart would take a destructive recovery path for a request that
		// is still running.
		if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			writeRecord(conn, Response{Reply: mustJSON(snap), TimedOut: true})
			return
		}
		if err != nil {
			writeRecord(conn, errorResponse(err))
			return
		}
		writeRecord(conn, Response{Reply: mustJSON(snap)})
	case req.Link != nil:
		if s.link == nil {
			writeRecord(conn, errorResponse(protocol.Refuse(protocol.CodeUnsupported, "this endpoint has no links")))
			return
		}
		lctx, cancel := context.WithTimeout(ctx, time.Duration(req.Link.WaitMS)*time.Millisecond+linkSlack/2)
		defer cancel()
		reply, err := s.link(lctx, *req.Link)
		if err != nil {
			writeRecord(conn, errorResponse(err))
			return
		}
		writeRecord(conn, Response{Reply: reply})
	case req.Features:
		writeRecord(conn, Response{Reply: mustJSON(Features{Link: true})})
	case req.Activity != nil:
		s.streamActivity(ctx, conn, req)
	case req.Register != nil:
		if s.registrar == nil {
			writeRecord(conn, errorResponse(protocol.Refuse(protocol.CodeUnsupported, "this endpoint does not accept live registration")))
			return
		}
		session, err := s.registrar(*req.Register)
		if err != nil {
			writeRecord(conn, errorResponse(err))
			return
		}
		writeRecord(conn, Response{Reply: mustJSON(session)})
	case req.Native != nil:
		id := s.ep.NativeSessionID(req.Native.TargetID)
		if id == "" {
			writeRecord(conn, errorResponse(protocol.Refuse(protocol.CodeUnshared, "target %q has no native session identity", req.Native.TargetID)))
			return
		}
		writeRecord(conn, Response{Reply: mustJSON(NativeReply{NativeSession: id})})
	case req.Command != nil:
		req.Command.Evidence = req.Evidence
		// A pinned submit stays bound to the pinned session: core checks it
		// again at every handoff, also after a restart.
		reply, err := s.ep.Handle(req.Command, core.Source{Host: LocalHost, NativeSession: req.NativeSession})
		if err != nil {
			writeRecord(conn, errorResponse(err))
			return
		}
		writeRecord(conn, Response{Reply: mustJSON(reply)})
	default:
		writeRecord(conn, Response{Error: &ErrorBody{Code: string(protocol.CodeInvalid), Message: "request carries neither command nor wait"}})
	}
}

// checkPin refuses a pinned request whose target is not attached to the
// pinned native session. The target comes from the command, or from the
// request reference for get and wait, so reads are fenced too.
func (s *Server) checkPin(req *Request) error {
	if req.NativeSession == "" {
		return nil
	}
	ref := ""
	target := ""
	switch {
	case req.Wait != nil:
		ref = req.Wait.RequestRef
	case req.Activity != nil:
		ref = req.Activity.RequestRef
	case req.Command != nil && req.Command.TargetID != "":
		target = req.Command.TargetID
	case req.Command != nil:
		ref = req.Command.RequestRef
	}
	if ref != "" {
		if _, t, _, err := protocol.DecodeRef(ref); err == nil {
			target = t
		}
	}
	if target == "" || s.ep.NativeSessionID(target) != req.NativeSession {
		return protocol.Refuse(protocol.CodeUnshared, "target %q is not attached to the pinned native session", target)
	}
	return nil
}

// streamActivity runs the activity handler on conn until it returns, the
// client closes the connection, or ctx ends. The client sends nothing after
// its request, so a read that returns means it is gone.
func (s *Server) streamActivity(ctx context.Context, conn net.Conn, req *Request) {
	if s.activity == nil {
		writeRecord(conn, errorResponse(protocol.Refuse(protocol.CodeUnsupported, "this endpoint does not stream activity")))
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = conn.Read(make([]byte, 1))
		cancel()
	}()
	send := func(update json.RawMessage) error {
		if err := sctx.Err(); err != nil {
			return err
		}
		return sendRecord(conn, Response{Reply: update})
	}
	if err := s.activity(sctx, *req.Activity, req.NativeSession, send); err != nil && sctx.Err() == nil {
		writeRecord(conn, errorResponse(err))
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
	if req.Wait != nil {
		// A wait is the one verb whose duration the CALLER chose. Bounding it
		// here at the short-verb deadline would report a request that is still
		// running as a failed read, and `wait` maps a non-refusal error to
		// exit 1 ("work failed or cancelled") — so an orchestrator would take
		// the recovery path for untouched, running work. --timeout 0 means no
		// limit (cmd/amq-remote: "0 = no limit"), and the server owns that
		// bound, so the client sets no deadline at all.
		if req.Wait.TimeoutMS <= 0 {
			readBound = 0
		} else {
			readBound = time.Duration(req.Wait.TimeoutMS)*time.Millisecond + callReadDeadline
		}
	}
	limit := MaxRecordBytes
	if req.Link != nil {
		readBound = time.Duration(req.Link.WaitMS)*time.Millisecond + linkSlack
		limit = LinkRecordBytes
	}
	if readBound > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(readBound))
	}
	writeRecord(conn, req)
	resp, _, err := readRecordLimit[Response](conn, limit)
	if err != nil {
		return nil, fmt.Errorf("read endpoint response: %w", err)
	}
	return resp, nil
}

// Activity streams the activity of one request from the endpoint at
// stateDir, calling fn with each update in order. It returns nil when the
// stream ends or ctx ends, and the refusal when the endpoint refuses. There is
// no read deadline: the caller ends a quiet stream by cancelling ctx.
func Activity(ctx context.Context, stateDir string, req Request, fn func(json.RawMessage)) error {
	if req.Activity == nil {
		return protocol.Refuse(protocol.CodeInvalid, "activity request is not set")
	}
	path := SocketPath(stateDir)
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return protocol.Refuse(protocol.CodeEndpointUnreachable, "no endpoint at %s; start one with `amq-remote serve`", path)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()
	if err := sendRecord(conn, req); err != nil {
		return err
	}
	br := bufio.NewReaderSize(conn, 64*1024)
	for {
		resp, err := nextRecord(br)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read endpoint activity: %w", err)
		}
		if err := resp.AsError(); err != nil {
			return err
		}
		fn(resp.Reply)
	}
}

// nextRecord reads one response record of at most MaxRecordBytes from a
// stream that carries several.
func nextRecord(br *bufio.Reader) (*Response, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxRecordBytes {
			return nil, fmt.Errorf("record exceeds %d bytes", MaxRecordBytes)
		}
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if len(line) == 0 {
				return nil, err
			}
			return nil, fmt.Errorf("read record: %w", err)
		}
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode record: %w", err)
	}
	return &resp, nil
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

// readRecordLimit reads one newline-terminated record of at most limit
// bytes and returns its length.
func readRecordLimit[T any](r io.Reader, limit int) (*T, int, error) {
	// Cap at the BOUNDARY, not after the fact: ReadBytes grows without limit,
	// so a peer streaming megabytes with no newline could allocate freely
	// inside the read deadline before the size check ever ran. One extra byte
	// is read so an over-long record is still detected rather than silently
	// truncated into a parse error.
	br := bufio.NewReaderSize(io.LimitReader(r, int64(limit)+1), 64*1024)
	line, err := br.ReadBytes('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return nil, 0, fmt.Errorf("read record: %w", err)
	}
	if len(line) > limit {
		return nil, len(line), fmt.Errorf("record exceeds %d bytes", limit)
	}
	var v T
	if err := json.Unmarshal(line, &v); err != nil {
		return nil, len(line), fmt.Errorf("decode record: %w", err)
	}
	return &v, len(line), nil
}

// writeResponseDeadline bounds one response write. A client that sends its
// request and then stops reading (a stopped shell, a wedged reader) would
// otherwise block the handler goroutine forever on a full socket buffer,
// leaking the goroutine and its fd for the life of the process.
const writeResponseDeadline = 10 * time.Second

func writeRecord(w io.Writer, v any) { _ = sendRecord(w, v) }

// sendRecord is writeRecord that reports a failed write, for a stream that
// must stop once its reader is gone.
func sendRecord(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// Bound the write when the writer is a connection: an unread socket must
	// not hold a handler (or a CLI) indefinitely.
	if c, ok := w.(net.Conn); ok {
		_ = c.SetWriteDeadline(time.Now().Add(writeResponseDeadline))
		defer func() { _ = c.SetWriteDeadline(time.Time{}) }()
	}
	_, err = w.Write(data)
	return err
}
