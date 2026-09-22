package acp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// ProtocolVersion is the live ACP bridge version. Prompt turns remain open
// until a fresh reply is observed on the pinned AMQ thread or the bounded
// timeout expires.
const ProtocolVersion = 2

// StopReasonEndTurn ends a prompt turn once a fresh reply was observed.
const StopReasonEndTurn = "end_turn"

// StopReasonRefusal ends a prompt turn without a reply: the bounded wait
// expired before the recipient answered.
const StopReasonRefusal = "refusal"

// DeliveryStateQueued is the honest post-delivery state. The message sits in the
// recipient's inbox/new; consumption is a separate, unproven event.
const DeliveryStateQueued = "queued_to_inbox"

// DeliveryStateReplied marks a turn answered by a fresh reply on the thread.
const DeliveryStateReplied = "replied"

// DeliveryStateNoReply marks a turn whose bounded reply wait expired.
const DeliveryStateNoReply = "no_reply"

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

const jsonRPCVersion = "2.0"

type request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params"`
}

type response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return e.Message
}

func newRPCError(code int, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// sessionState is one live ACP session bound to a durable cockpit thread.
type sessionState struct {
	ID         string
	ChannelID  string
	Thread     string
	InFlight   bool
	turnCancel chan struct{}
}

// Server is one long-lived ACP stdio connection bound to one authenticated
// AMQ routing context. Prompt requests run in goroutines so the reader stays
// available while a turn waits for its reply.
type Server struct {
	cfg      Config
	version  string
	mu       sync.Mutex
	sessions map[string]*sessionState
	store    *sessionStore
	ready    bool
	// streamClosed records that the ACP input stream ended. A prompt that
	// begins after this point never had a connected client, so it refuses
	// immediately instead of waiting out the bounded timeout.
	streamClosed bool
}

// NewServer builds a server bound to one already authenticated routing context.
func NewServer(cfg Config, version string) *Server {
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(cfg.Root, "meta", "acp")
	}
	if cfg.TurnTimeout <= 0 {
		cfg.TurnTimeout = defaultTurnTimeout
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaultHeartbeatInterval
	}
	return &Server{
		cfg:      cfg,
		version:  version,
		sessions: make(map[string]*sessionState),
		store:    newSessionStore(cfg),
	}
}

// responseWriter serializes JSON-RPC writes. Prompt turns respond from
// goroutines while the reader loop may still write, so every write locks.
type responseWriter struct {
	mu      sync.Mutex
	writer  *bufio.Writer
	encoder *json.Encoder
}

func newResponseWriter(out io.Writer) *responseWriter {
	writer := bufio.NewWriter(out)
	return &responseWriter{writer: writer, encoder: json.NewEncoder(writer)}
}

func (w *responseWriter) write(value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.encoder.Encode(value); err != nil {
		return err
	}
	return w.writer.Flush()
}

// Serve reads newline-delimited JSON-RPC objects. Prompt requests run in
// goroutines while the reader remains available; notifications produce no
// response. It returns once in is exhausted.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	// streamClosed is per-stream state: a fresh input stream is a fresh client
	// connection, even on a reused server.
	s.mu.Lock()
	s.streamClosed = false
	s.mu.Unlock()
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), format.MaxMessageSize+1024)
	writer := newResponseWriter(out)
	var pending sync.WaitGroup
	var errMu sync.Mutex
	var writeErr error
	recordWriteErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		if writeErr == nil {
			writeErr = err
		}
		errMu.Unlock()
	}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		lineCopy := append([]byte(nil), line...)
		if requestMethod(lineCopy) == "session/prompt" {
			pending.Add(1)
			go func() {
				defer pending.Done()
				resp, ok := s.handleWithNotify(lineCopy, writer.write)
				if !ok {
					return
				}
				recordWriteErr(writer.write(resp))
			}()
			continue
		}
		resp, ok := s.handle(lineCopy)
		if !ok {
			continue
		}
		if err := writer.write(resp); err != nil {
			recordWriteErr(err)
			break
		}
	}
	scanErr := scanner.Err()
	// The ACP input stream has ended — a clean EOF or a scanner error such as
	// an oversized line: either way the client is gone, so no in-flight turn
	// may hold the process until the turn timeout. Queued AMQ messages stay
	// intact; the scanner error itself is returned below.
	s.cancelAll()
	pending.Wait()
	errMu.Lock()
	deferredWriteErr := writeErr
	errMu.Unlock()
	if deferredWriteErr != nil {
		return deferredWriteErr
	}
	return scanErr
}

func requestMethod(line []byte) string {
	var req request
	if json.Unmarshal(line, &req) != nil {
		return ""
	}
	return req.Method
}

// handleWithNotify turns one request line into at most one response, emitting
// session/update notifications through emit while the turn runs.
func (s *Server) handleWithNotify(line []byte, emit func(any) error) (response, bool) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nil, newRPCError(codeParseError, "invalid JSON: %v", err)), true
	}
	if req.JSONRPC != jsonRPCVersion || strings.TrimSpace(req.Method) == "" {
		return errorResponse(req.ID, newRPCError(codeInvalidRequest, "request must set jsonrpc %q and a method", jsonRPCVersion)), true
	}

	result, rpcErr := s.dispatchWithNotify(req.Method, req.Params, emit)
	if req.ID == nil {
		return response{}, false
	}
	if rpcErr != nil {
		return errorResponse(req.ID, rpcErr), true
	}
	return response{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result}, true
}

// handle turns one request line into at most one response.
func (s *Server) handle(line []byte) (response, bool) {
	return s.handleWithNotify(line, nil)
}

func (s *Server) dispatchWithNotify(method string, params json.RawMessage, emit func(any) error) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.initialize(params)
	case "session/new":
		return s.newSession(params)
	case "session/prompt":
		return s.prompt(params, emit)
	case "session/cancel":
		return s.cancel(params)
	default:
		return nil, newRPCError(codeMethodNotFound, "method %q is not implemented by this ACP v2 bridge", method)
	}
}

func errorResponse(id *json.RawMessage, err *rpcError) response {
	return response{JSONRPC: jsonRPCVersion, ID: id, Error: err}
}

type initializeParams struct {
	ProtocolVersion    json.RawMessage `json:"protocolVersion"`
	ClientCapabilities json.RawMessage `json:"clientCapabilities"`
	ClientInfo         json.RawMessage `json:"clientInfo"`
	Meta               json.RawMessage `json:"_meta"`
}

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AgentInfo         agentInfo         `json:"agentInfo"`
	AuthMethods       []any             `json:"authMethods"`
}

// agentCapabilities advertises the smallest honest v1 surface. Omitting
// mcpCapabilities declares no MCP support. Every prompt capability is false:
// those flags gate image, audio, and embedded context, while text and
// resource_link are the v1 baseline every agent must accept. Filesystem and
// terminal access are client capabilities this companion never calls.
type agentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	PromptCapabilities promptCapabilities `json:"promptCapabilities"`
}

type promptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type agentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

func (s *Server) initialize(params json.RawMessage) (any, *rpcError) {
	// initialize is the one strict surface: an unrecognized top-level field
	// means the client expects behavior this preview does not have.
	var parsed initializeParams
	if err := decodeParams(params, &parsed, true); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	return initializeResult{
		ProtocolVersion: ProtocolVersion,
		AgentCapabilities: agentCapabilities{
			LoadSession:        false,
			PromptCapabilities: promptCapabilities{},
		},
		AgentInfo: agentInfo{
			Name:    "amq-acp",
			Title:   "Agent Message Queue (ACP v2 live bridge)",
			Version: s.version,
		},
		AuthMethods: []any{},
	}, nil
}

type newSessionParams struct {
	Cwd        string          `json:"cwd"`
	McpServers json.RawMessage `json:"mcpServers"`
	Meta       json.RawMessage `json:"_meta"`
}

type newSessionResult struct {
	SessionID string          `json:"sessionId"`
	Meta      sessionMetaInfo `json:"_meta"`
}

type sessionMetaInfo struct {
	ChannelID string `json:"channelId"`
	Thread    string `json:"thread"`
}

func (s *Server) newSession(params json.RawMessage) (any, *rpcError) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		return nil, newRPCError(codeInvalidRequest, "initialize must complete before session/new")
	}
	var parsed newSessionParams
	if err := decodeParams(params, &parsed, false); err != nil {
		return nil, err
	}
	channelID, err := channelIDFromMeta(parsed.Meta)
	if err != nil {
		return nil, newRPCError(codeInvalidParams, "%v", err)
	}
	id, err := newSessionID()
	if err != nil {
		return nil, newRPCError(codeInternalError, "generate session id: %v", err)
	}
	if channelID == "" {
		channelID = "session/" + id
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store.err != nil {
		return nil, newRPCError(codeInternalError, "load ACP session state: %v", s.store.err)
	}
	mapping, found, err := s.store.get(channelID)
	if err != nil {
		return nil, newRPCError(codeInternalError, "load ACP session mapping: %v", err)
	}
	threadID := cockpitThread(channelID)
	if found {
		threadID = mapping.Thread
	}
	if strings.TrimSpace(threadID) == "" {
		return nil, newRPCError(codeInternalError, "stored ACP session mapping has an empty thread")
	}
	if err := s.store.put(channelID, threadID, now); err != nil {
		return nil, newRPCError(codeInternalError, "persist ACP session mapping: %v", err)
	}
	s.sessions[id] = &sessionState{
		ID:        id,
		ChannelID: channelID,
		Thread:    threadID,
	}
	return newSessionResult{
		SessionID: id,
		Meta:      sessionMetaInfo{ChannelID: channelID, Thread: threadID},
	}, nil
}

type promptParams struct {
	SessionID string          `json:"sessionId"`
	Prompt    []contentBlock  `json:"prompt"`
	Meta      json.RawMessage `json:"_meta"`
}

type contentBlock struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	URI   string `json:"uri"`
	Name  string `json:"name"`
	Title string `json:"title"`
}

type promptResult struct {
	StopReason string     `json:"stopReason"`
	Meta       promptMeta `json:"_meta"`
}

type promptMeta struct {
	AMQ amqDelivery `json:"amq"`
}

type amqDelivery struct {
	MessageID string `json:"messageId"`
	To        string `json:"to"`
	Thread    string `json:"thread"`
	State     string `json:"state"`
	Reply     string `json:"reply,omitempty"`
	Reason    string `json:"reason,omitempty"`
	EventID   string `json:"eventId,omitempty"`
	Committed bool   `json:"committed"`
	Drained   bool   `json:"drained"`
	Started   bool   `json:"started"`
	Completed bool   `json:"completed"`
	Egress    string `json:"egress"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

func (s *Server) prompt(params json.RawMessage, emit func(any) error) (any, *rpcError) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		return nil, newRPCError(codeInvalidRequest, "initialize must complete before session/prompt")
	}
	var parsed promptParams
	if err := decodeParams(params, &parsed, false); err != nil {
		return nil, err
	}
	text, rpcErr := promptText(parsed.Prompt)
	if rpcErr != nil {
		return nil, rpcErr
	}
	eventID, rpcErr := resolveEventID(parsed.Meta)
	if rpcErr != nil {
		return nil, rpcErr
	}

	s.mu.Lock()
	if s.streamClosed {
		s.mu.Unlock()
		return disconnectedRefusal(), nil
	}
	session, cancel, rpcErr := s.beginTurnLocked(parsed.SessionID)
	s.mu.Unlock()
	if rpcErr != nil {
		return nil, rpcErr
	}
	defer s.finishTurn(session, cancel)

	delivery, err := DeliverCockpitPrompt(s.cfg, text, session.Thread, eventID)
	if err != nil {
		return nil, newRPCError(codeInternalError, "deliver prompt to %s: %v", s.cfg.To, err)
	}
	return s.waitForReply(parsed.SessionID, delivery, cancel, emit)
}

func (s *Server) beginTurnLocked(sessionID string) (*sessionState, chan struct{}, *rpcError) {
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, nil, newRPCError(codeInvalidParams, "unknown sessionId %q; call session/new first", sessionID)
	}
	if session.InFlight {
		return nil, nil, newRPCError(codeInvalidRequest, "sessionId %q already has an in-flight prompt", sessionID)
	}
	session.InFlight = true
	session.turnCancel = make(chan struct{})
	return session, session.turnCancel, nil
}

func (s *Server) finishTurn(session *sessionState, cancel chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.turnCancel == cancel {
		session.turnCancel = nil
	}
	session.InFlight = false
}

// disconnectedRefusal is the typed result for a prompt that arrives after the
// ACP input stream closed: the client can never see the answer, so the turn
// refuses without delivering a message no one will collect.
func disconnectedRefusal() promptResult {
	return promptResult{
		StopReason: StopReasonRefusal,
		Meta:       promptMeta{AMQ: amqDelivery{State: DeliveryStateNoReply, Reason: "client_disconnected"}},
	}
}

// waitForReply holds the ACP turn open while polling the pinned AMQ thread for
// a fresh reply. Every wait is bounded; a timeout is a typed refusal, never a
// fabricated success.
func (s *Server) waitForReply(sessionID string, delivery Delivery, cancel <-chan struct{}, emit func(any) error) (any, *rpcError) {
	if err := emitText(emit, sessionID, "agent_thought_chunk", fmt.Sprintf("Waiting for a reply from %s on AMQ thread %s.", s.cfg.To, delivery.Thread)); err != nil {
		return nil, newRPCError(codeInternalError, "emit ACP session update: %v", err)
	}

	deadline := time.NewTimer(s.cfg.TurnTimeout)
	defer deadline.Stop()
	poll := time.NewTicker(s.cfg.PollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(s.cfg.HeartbeatInterval)
	defer heartbeat.Stop()

	for {
		reply, found, err := s.replyForDelivery(delivery)
		if err != nil {
			return nil, newRPCError(codeInternalError, "poll AMQ thread %s: %v", delivery.Thread, err)
		}
		if found {
			if err := emitText(emit, sessionID, "agent_message_chunk", reply); err != nil {
				return nil, newRPCError(codeInternalError, "emit ACP reply update: %v", err)
			}
			return promptResult{
				StopReason: StopReasonEndTurn,
				Meta: promptMeta{AMQ: amqDelivery{
					MessageID: delivery.MessageID,
					To:        delivery.To,
					Thread:    delivery.Thread,
					State:     DeliveryStateReplied,
					Reply:     reply,
					EventID:   delivery.EventID,
					Committed: delivery.Committed,
					Drained:   delivery.Drained,
					Started:   delivery.Started,
					Completed: delivery.Completed,
					Egress:    delivery.Egress,
					Duplicate: delivery.Duplicate,
				}},
			}, nil
		}

		select {
		case <-cancel:
			// The stream closed: the client is gone. The message stays queued
			// in AMQ; the turn itself reports the typed no-reply refusal.
			return promptResult{
				StopReason: StopReasonRefusal,
				Meta: promptMeta{AMQ: amqDelivery{
					MessageID: delivery.MessageID,
					To:        delivery.To,
					Thread:    delivery.Thread,
					State:     DeliveryStateNoReply,
					Reason:    "client_disconnected",
					EventID:   delivery.EventID,
					Committed: delivery.Committed,
					Drained:   delivery.Drained,
					Started:   delivery.Started,
					Completed: delivery.Completed,
					Egress:    delivery.Egress,
					Duplicate: delivery.Duplicate,
				}},
			}, nil
		case <-deadline.C:
			return promptResult{
				StopReason: StopReasonRefusal,
				Meta: promptMeta{AMQ: amqDelivery{
					MessageID: delivery.MessageID,
					To:        delivery.To,
					Thread:    delivery.Thread,
					State:     DeliveryStateNoReply,
					Reason:    "reply_timeout",
					EventID:   delivery.EventID,
					Committed: delivery.Committed,
					Drained:   delivery.Drained,
					Started:   delivery.Started,
					Completed: delivery.Completed,
					Egress:    delivery.Egress,
					Duplicate: delivery.Duplicate,
				}},
			}, nil
		case <-poll.C:
		case <-heartbeat.C:
			if err := emitText(emit, sessionID, "agent_thought_chunk", fmt.Sprintf("Still waiting for a reply from %s on AMQ thread %s.", s.cfg.To, delivery.Thread)); err != nil {
				return nil, newRPCError(codeInternalError, "emit ACP heartbeat: %v", err)
			}
		}
	}
}

// replyForDelivery returns the freshest reply from the configured peer that
// was created no earlier than this prompt. Stale or thread-rent messages are
// never picked up; a malformed unrelated mailbox item does not fail the poll.
func (s *Server) replyForDelivery(delivery Delivery) (string, bool, error) {
	entries, err := thread.Collect(s.cfg.Root, delivery.Thread, []string{s.cfg.Me, s.cfg.To}, true, func(_ string, _ error) error {
		return nil
	})
	if err != nil {
		return "", false, err
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.From != s.cfg.To || entry.RawTime.IsZero() || entry.RawTime.Before(delivery.Created) {
			continue
		}
		if strings.TrimSpace(entry.Body) == "" {
			continue
		}
		return strings.TrimRight(entry.Body, "\n"), true, nil
	}
	return "", false, nil
}

// promptText renders the v1 baseline block types: text passes through and
// resource_link becomes a markdown link. Any other block type is refused rather
// than silently dropped, because the false promptCapabilities declared image,
// audio, and embedded context unsupported.
func promptText(blocks []contentBlock) (string, *rpcError) {
	if len(blocks) == 0 {
		return "", newRPCError(codeInvalidParams, "prompt must contain at least one content block")
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "resource_link":
			if strings.TrimSpace(block.URI) == "" || strings.TrimSpace(block.Name) == "" {
				return "", newRPCError(codeInvalidParams, "resource_link block requires non-empty uri and name")
			}
			label := strings.TrimSpace(block.Title)
			if label == "" {
				label = block.Name
			}
			parts = append(parts, fmt.Sprintf("[%s](%s)", label, block.URI))
		default:
			return "", newRPCError(
				codeInvalidParams,
				"content block type %q is not supported; this companion accepts text and resource_link only",
				block.Type,
			)
		}
	}
	return strings.Join(parts, "\n"), nil
}

type cancelParams struct {
	SessionID string          `json:"sessionId"`
	Meta      json.RawMessage `json:"_meta"`
}

// cancel acknowledges a cancellation of bookkeeping state. Tearing down an
// in-flight turn is not wired yet; the turn completes on its own bounded wait.
func (s *Server) cancel(params json.RawMessage) (any, *rpcError) {
	var parsed cancelParams
	if err := decodeParams(params, &parsed, false); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[parsed.SessionID]
	if !ok {
		return nil, newRPCError(codeInvalidParams, "unknown sessionId %q", parsed.SessionID)
	}
	return struct{}{}, nil
}

// cancelAll ends in-flight waits when the ACP stream closes: the client is
// gone, so no turn may hold the process until the turn timeout. Queued AMQ
// messages are left intact; delivery is never retracted.
func (s *Server) cancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamClosed = true
	for _, session := range s.sessions {
		if session.turnCancel != nil {
			close(session.turnCancel)
			session.turnCancel = nil
		}
	}
}

type sessionUpdateNotification struct {
	JSONRPC string              `json:"jsonrpc"`
	Method  string              `json:"method"`
	Params  sessionUpdateParams `json:"params"`
}

type sessionUpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    sessionUpdate `json:"update"`
}

type sessionUpdate struct {
	SessionUpdate string      `json:"sessionUpdate"`
	Content       textContent `json:"content"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func emitText(emit func(any) error, sessionID, updateType, text string) error {
	if emit == nil {
		return nil
	}
	return emit(sessionUpdateNotification{
		JSONRPC: jsonRPCVersion,
		Method:  "session/update",
		Params: sessionUpdateParams{
			SessionID: sessionID,
			Update: sessionUpdate{
				SessionUpdate: updateType,
				Content:       textContent{Type: "text", Text: text},
			},
		},
	})
}

// decodeParams decodes request params. When strict, unrecognized top-level
// fields are rejected; nested objects captured as raw JSON stay opaque so a
// richer but non-routing client payload is not refused.
func decodeParams(params json.RawMessage, target any, strict bool) *rpcError {
	if len(bytes.TrimSpace(params)) == 0 {
		params = json.RawMessage("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(params))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return newRPCError(codeInvalidParams, "invalid params: %v", err)
	}
	return nil
}

func newSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "acp_" + hex.EncodeToString(buf), nil
}
