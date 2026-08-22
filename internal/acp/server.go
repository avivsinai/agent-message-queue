package acp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

// ProtocolVersion is the only ACP version this companion speaks. It is never
// negotiated upward: a client asking for a newer version still receives 1 and
// decides for itself whether to disconnect.
const ProtocolVersion = 1

// StopReasonEndTurn ends a prompt turn once the message is queued. The turn is
// genuinely over because this companion runs no model and requests no tools.
const StopReasonEndTurn = "end_turn"

// DeliveryStateQueued is the honest post-delivery state. The message sits in the
// recipient's inbox/new; consumption is a separate, unproven event.
const DeliveryStateQueued = "queued_to_inbox"

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
}

func (e *rpcError) Error() string {
	return e.Message
}

func newRPCError(code int, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Server is a single-connection ACP v1 agent. Requests are handled one at a
// time in arrival order, so the session table needs no locking.
type Server struct {
	cfg      Config
	version  string
	sessions map[string]bool
	ready    bool
}

// NewServer builds a server bound to one already authenticated routing context.
func NewServer(cfg Config, version string) *Server {
	return &Server{cfg: cfg, version: version, sessions: make(map[string]bool)}
}

// Serve reads newline-delimited JSON-RPC objects from in and writes one
// response line per request. Notifications produce no response. It returns once
// in is exhausted.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), format.MaxMessageSize+1024)
	writer := bufio.NewWriter(out)
	encoder := json.NewEncoder(writer)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		resp, ok := s.handle(line)
		if !ok {
			continue
		}
		if err := encoder.Encode(resp); err != nil {
			return err
		}
		// Flush per response so an interactive client is never left waiting.
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// handle turns one request line into at most one response.
func (s *Server) handle(line []byte) (response, bool) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nil, newRPCError(codeParseError, "invalid JSON: %v", err)), true
	}
	if req.JSONRPC != jsonRPCVersion || strings.TrimSpace(req.Method) == "" {
		return errorResponse(req.ID, newRPCError(codeInvalidRequest, "request must set jsonrpc %q and a method", jsonRPCVersion)), true
	}

	result, rpcErr := s.dispatch(req.Method, req.Params)
	if req.ID == nil {
		return response{}, false
	}
	if rpcErr != nil {
		return errorResponse(req.ID, rpcErr), true
	}
	return response{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result}, true
}

func (s *Server) dispatch(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.initialize(params)
	case "session/new":
		return s.newSession(params)
	case "session/prompt":
		return s.prompt(params)
	case "session/cancel":
		return s.cancel(params)
	default:
		return nil, newRPCError(codeMethodNotFound, "method %q is not implemented by this ACP v1 companion", method)
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
	s.ready = true
	return initializeResult{
		ProtocolVersion: ProtocolVersion,
		AgentCapabilities: agentCapabilities{
			LoadSession:        false,
			PromptCapabilities: promptCapabilities{},
		},
		AgentInfo: agentInfo{
			Name:    "amq-acp",
			Title:   "Agent Message Queue (ACP v1 preview)",
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
	SessionID string `json:"sessionId"`
}

func (s *Server) newSession(params json.RawMessage) (any, *rpcError) {
	if !s.ready {
		return nil, newRPCError(codeInvalidRequest, "initialize must complete before session/new")
	}
	var parsed newSessionParams
	if err := decodeParams(params, &parsed, false); err != nil {
		return nil, err
	}
	id, err := newSessionID()
	if err != nil {
		return nil, newRPCError(codeInternalError, "generate session id: %v", err)
	}
	s.sessions[id] = true
	return newSessionResult{SessionID: id}, nil
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
}

func (s *Server) prompt(params json.RawMessage) (any, *rpcError) {
	if !s.ready {
		return nil, newRPCError(codeInvalidRequest, "initialize must complete before session/prompt")
	}
	var parsed promptParams
	if err := decodeParams(params, &parsed, false); err != nil {
		return nil, err
	}
	if !s.sessions[parsed.SessionID] {
		return nil, newRPCError(codeInvalidParams, "unknown sessionId %q; call session/new first", parsed.SessionID)
	}

	text, rpcErr := promptText(parsed.Prompt)
	if rpcErr != nil {
		return nil, rpcErr
	}
	delivery, err := DeliverPrompt(s.cfg, text)
	if err != nil {
		return nil, newRPCError(codeInternalError, "deliver prompt to %s: %v", s.cfg.To, err)
	}
	return promptResult{
		StopReason: StopReasonEndTurn,
		Meta: promptMeta{AMQ: amqDelivery{
			MessageID: delivery.MessageID,
			To:        delivery.To,
			Thread:    delivery.Thread,
			State:     DeliveryStateQueued,
		}},
	}, nil
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

// cancel acknowledges a cancellation. Prompt turns here complete synchronously
// inside session/prompt, so by the time a cancel arrives there is no in-flight
// model request or tool call to abort.
func (s *Server) cancel(params json.RawMessage) (any, *rpcError) {
	var parsed cancelParams
	if err := decodeParams(params, &parsed, false); err != nil {
		return nil, err
	}
	if !s.sessions[parsed.SessionID] {
		return nil, newRPCError(codeInvalidParams, "unknown sessionId %q", parsed.SessionID)
	}
	return struct{}{}, nil
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
