package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

const (
	testMe = "cursor"
	testTo = "codex"
)

// testConfig returns a routing context pointed at a fresh queue root. Delivery
// creates the recipient mailbox itself, so a bare directory is a valid root.
func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{Root: t.TempDir(), Me: testMe, To: testTo}
}

// serve feeds request lines through one server and returns the response lines.
// Reusing a server across calls preserves initialize and session state exactly
// as a long-lived stdio connection would.
func serve(t *testing.T, server *Server, lines ...string) []string {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	out := &strings.Builder{}
	if err := server.Serve(in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	written := strings.TrimSpace(out.String())
	if written == "" {
		return nil
	}
	return strings.Split(written, "\n")
}

func serveOne(t *testing.T, server *Server, line string) string {
	t.Helper()
	lines := serve(t, server, line)
	if len(lines) != 1 {
		t.Fatalf("Serve wrote %d response lines, want 1: %v", len(lines), lines)
	}
	return lines[0]
}

// initializedServer completes initialize and session/new, returning the live
// session id.
func initializedServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	server := NewServer(cfg, "test")
	responses := serve(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`,
	)
	if len(responses) != 2 {
		t.Fatalf("setup wrote %d responses, want 2: %v", len(responses), responses)
	}
	var created struct {
		Result struct {
			SessionID string `json:"sessionId"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal([]byte(responses[1]), &created); err != nil {
		t.Fatalf("decode session/new response: %v", err)
	}
	if created.Error != nil {
		t.Fatalf("session/new failed: %v", created.Error)
	}
	if created.Result.SessionID == "" {
		t.Fatal("session/new returned an empty sessionId")
	}
	return server, created.Result.SessionID
}

func decodeResult[T any](t *testing.T, line string) T {
	t.Helper()
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   *rpcError       `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatalf("decode response %q: %v", line, err)
	}
	if envelope.JSONRPC != "2.0" {
		t.Fatalf("response jsonrpc = %q, want \"2.0\"", envelope.JSONRPC)
	}
	if envelope.Error != nil {
		t.Fatalf("unexpected error response: %v", envelope.Error)
	}
	var result T
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("decode result %q: %v", envelope.Result, err)
	}
	return result
}

func decodeError(t *testing.T, line string) rpcError {
	t.Helper()
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatalf("decode response %q: %v", line, err)
	}
	if envelope.Error == nil {
		t.Fatalf("response %q has no error, want one", line)
	}
	if len(envelope.Result) != 0 {
		t.Fatalf("response %q carries both result and error", line)
	}
	return *envelope.Error
}

// TestInitializeAnswersProtocolVersionOne proves the companion reports ACP v1
// using v1's own response shape.
func TestInitializeAnswersProtocolVersionOne(t *testing.T) {
	server := NewServer(testConfig(t), "test")
	line := serveOne(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`)

	result := decodeResult[struct {
		ProtocolVersion   json.Number `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image           bool `json:"image"`
				Audio           bool `json:"audio"`
				EmbeddedContext bool `json:"embeddedContext"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
		AgentInfo struct {
			Name string `json:"name"`
		} `json:"agentInfo"`
	}](t, line)

	if result.ProtocolVersion.String() != "1" {
		t.Fatalf("protocolVersion = %s, want 1", result.ProtocolVersion)
	}
	if result.AgentCapabilities.LoadSession {
		t.Error("loadSession = true, want false: session/load is not implemented")
	}
	capabilities := result.AgentCapabilities.PromptCapabilities
	if capabilities.Image || capabilities.Audio || capabilities.EmbeddedContext {
		t.Errorf("promptCapabilities = %+v, want every capability false", capabilities)
	}
	if result.AgentInfo.Name != "amq-acp" {
		t.Errorf("agentInfo.name = %q, want \"amq-acp\"", result.AgentInfo.Name)
	}
}

// TestInitializeNeverAdvertisesProtocolVersionTwo is the load-bearing negative
// case: an implementation that echoed the client's requested version, or that
// answered with the newest version it had heard of, would answer 2 here.
func TestInitializeNeverAdvertisesProtocolVersionTwo(t *testing.T) {
	server := NewServer(testConfig(t), "test")
	line := serveOne(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"clientCapabilities":{}}}`)

	result := decodeResult[map[string]json.RawMessage](t, line)
	version := string(result["protocolVersion"])
	if version != "1" {
		t.Fatalf("protocolVersion = %s for a v2 client, want 1", version)
	}

	// v1 names the response fields agentCapabilities and agentInfo. v2 renames
	// them to capabilities and info, so their presence would mean this
	// companion had started speaking v2 shapes.
	for _, required := range []string{"agentCapabilities", "agentInfo", "authMethods"} {
		if _, ok := result[required]; !ok {
			t.Errorf("initialize result is missing the ACP v1 field %q", required)
		}
	}
	for _, forbidden := range []string{"capabilities", "info"} {
		if _, ok := result[forbidden]; ok {
			t.Errorf("initialize result exposes the ACP v2 field %q", forbidden)
		}
	}
	if _, ok := result["agentCapabilities"]; ok && strings.Contains(string(result["agentCapabilities"]), "mcpCapabilities") {
		t.Error("agentCapabilities advertises mcpCapabilities, but no MCP support exists")
	}
}

// TestInitializeRejectsUnknownField refuses a client that expects behavior this
// preview does not implement.
func TestInitializeRejectsUnknownField(t *testing.T) {
	server := NewServer(testConfig(t), "test")
	line := serveOne(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"sessionModes":{"current":"yolo"}}}`)

	if code := decodeError(t, line).Code; code != codeInvalidParams {
		t.Fatalf("error code = %d, want %d for an unknown initialize field", code, codeInvalidParams)
	}
}

// TestInitializeAcceptsReservedMetaAndRicherCapabilities keeps strictness from
// rejecting fields ACP v1 genuinely defines.
func TestInitializeAcceptsReservedMetaAndRicherCapabilities(t *testing.T) {
	server := NewServer(testConfig(t), "test")
	line := serveOne(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":true},"terminal":true,"somethingNewer":{"nested":1}},"clientInfo":{"name":"zed"},"_meta":{"trace":"abc"}}}`)

	result := decodeResult[map[string]json.RawMessage](t, line)
	if string(result["protocolVersion"]) != "1" {
		t.Fatalf("protocolVersion = %s, want 1", result["protocolVersion"])
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	server := NewServer(testConfig(t), "test")
	for _, method := range []string{"session/load", "fs/read_text_file", "terminal/create", "tools/call", "nonsense"} {
		t.Run(method, func(t *testing.T) {
			line := serveOne(t, NewServer(server.cfg, "test"), `{"jsonrpc":"2.0","id":7,"method":"`+method+`","params":{}}`)
			failure := decodeError(t, line)
			if failure.Code != codeMethodNotFound {
				t.Fatalf("error code = %d, want %d", failure.Code, codeMethodNotFound)
			}
			if !strings.Contains(failure.Message, method) {
				t.Errorf("error message %q does not name the rejected method", failure.Message)
			}
		})
	}
}

// TestSessionPromptDeliversMessageIntoRecipientInbox proves the prompt becomes a
// real, parseable message in the recipient's inbox/new.
func TestSessionPromptDeliversMessageIntoRecipientInbox(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)

	const body = "please review the ACP companion"
	line := serveOne(t, server, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"`+sessionID+`","prompt":[{"type":"text","text":"`+body+`"}]}}`)

	result := decodeResult[struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			AMQ struct {
				MessageID string `json:"messageId"`
				To        string `json:"to"`
				Thread    string `json:"thread"`
				State     string `json:"state"`
			} `json:"amq"`
		} `json:"_meta"`
	}](t, line)

	if result.StopReason != StopReasonEndTurn {
		t.Errorf("stopReason = %q, want %q", result.StopReason, StopReasonEndTurn)
	}
	if result.Meta.AMQ.State != DeliveryStateQueued {
		t.Errorf("delivery state = %q, want %q; queuing is not consumption", result.Meta.AMQ.State, DeliveryStateQueued)
	}
	if result.Meta.AMQ.To != testTo {
		t.Errorf("delivery to = %q, want %q", result.Meta.AMQ.To, testTo)
	}

	path := filepath.Join(cfg.Root, "agents", testTo, "inbox", "new", result.Meta.AMQ.MessageID+".md")
	message, err := format.ReadMessageFile(path)
	if err != nil {
		t.Fatalf("read delivered message %s: %v", path, err)
	}
	if strings.TrimSpace(message.Body) != body {
		t.Errorf("delivered body = %q, want %q", message.Body, body)
	}
	if message.Header.From != testMe {
		t.Errorf("header from = %q, want %q", message.Header.From, testMe)
	}
	if len(message.Header.To) != 1 || message.Header.To[0] != testTo {
		t.Errorf("header to = %v, want [%s]", message.Header.To, testTo)
	}
	if message.Header.Thread != "p2p/codex__cursor" {
		t.Errorf("header thread = %q, want canonical p2p/codex__cursor", message.Header.Thread)
	}
	if message.Header.ID != result.Meta.AMQ.MessageID {
		t.Errorf("header id = %q, want the reported %q", message.Header.ID, result.Meta.AMQ.MessageID)
	}

	// tmp must be empty: a message left staged there is never delivered.
	entries, err := os.ReadDir(filepath.Join(cfg.Root, "agents", testTo, "inbox", "tmp"))
	if err != nil {
		t.Fatalf("read inbox tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("inbox/tmp holds %d leftover entries, want 0", len(entries))
	}
}

func TestSessionPromptRefusesUnknownSession(t *testing.T) {
	server, sessionID := initializedServer(t, testConfig(t))

	line := serveOne(t, server, `{"jsonrpc":"2.0","id":9,"method":"session/prompt","params":{"sessionId":"acp_not_a_real_session","prompt":[{"type":"text","text":"hello"}]}}`)
	if code := decodeError(t, line).Code; code != codeInvalidParams {
		t.Fatalf("error code = %d, want %d", code, codeInvalidParams)
	}
	if sessionID == "" {
		t.Fatal("expected a live session for contrast")
	}
}

// TestSessionMethodsRefuseBeforeInitialize keeps the v1 ordering requirement
// from silently degrading into an implicit session.
func TestSessionMethodsRefuseBeforeInitialize(t *testing.T) {
	for _, request := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/tmp"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"acp_x","prompt":[{"type":"text","text":"hi"}]}}`,
	} {
		line := serveOne(t, NewServer(testConfig(t), "test"), request)
		if code := decodeError(t, line).Code; code != codeInvalidRequest {
			t.Errorf("error code = %d for %s, want %d", code, request, codeInvalidRequest)
		}
	}
}

// TestSessionPromptRefusesNonTextContent refuses content the initialize response
// declared unsupported, instead of silently dropping it.
func TestSessionPromptRefusesNonTextContent(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)

	line := serveOne(t, server, `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"`+sessionID+`","prompt":[{"type":"image","data":"aGk=","mimeType":"image/png"}]}}`)
	failure := decodeError(t, line)
	if failure.Code != codeInvalidParams {
		t.Fatalf("error code = %d, want %d", failure.Code, codeInvalidParams)
	}
	if !strings.Contains(failure.Message, "image") {
		t.Errorf("error message %q does not name the rejected content type", failure.Message)
	}
	assertNoDeliveredMessages(t, cfg.Root)
}

func TestSessionPromptRefusesEmptyPrompt(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)

	for _, prompt := range []string{`[]`, `[{"type":"text","text":"   "}]`} {
		line := serveOne(t, server, `{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{"sessionId":"`+sessionID+`","prompt":`+prompt+`}}`)
		if len(decodeError(t, line).Message) == 0 {
			t.Errorf("prompt %s was accepted, want a refusal", prompt)
		}
	}
	assertNoDeliveredMessages(t, cfg.Root)
}

// TestNotificationsProduceNoResponse holds JSON-RPC notification semantics: a
// request without an id must not be answered.
func TestNotificationsProduceNoResponse(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)

	responses := serve(t, server,
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"`+sessionID+`"}}`,
		`{"jsonrpc":"2.0","method":"totally/unknown","params":{}}`,
	)
	if len(responses) != 0 {
		t.Fatalf("notifications produced %d responses, want 0: %v", len(responses), responses)
	}
}

func TestMalformedInputIsReportedNotFatal(t *testing.T) {
	server := NewServer(testConfig(t), "test")

	responses := serve(t, server,
		`{"jsonrpc":"2.0","id":1,"method":`,
		`{"jsonrpc":"1.0","id":2,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":1}}`,
	)
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want 3: %v", len(responses), responses)
	}
	if code := decodeError(t, responses[0]).Code; code != codeParseError {
		t.Errorf("malformed JSON error code = %d, want %d", code, codeParseError)
	}
	if code := decodeError(t, responses[1]).Code; code != codeInvalidRequest {
		t.Errorf("bad jsonrpc version error code = %d, want %d", code, codeInvalidRequest)
	}
	if string(decodeResult[map[string]json.RawMessage](t, responses[2])["protocolVersion"]) != "1" {
		t.Error("server did not recover to serve a valid initialize after malformed input")
	}
}

// TestMultipleSessionsShareOneRecipient proves each prompt becomes its own
// message rather than overwriting an earlier one.
func TestMultipleSessionsShareOneRecipient(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)

	for _, body := range []string{"first", "second", "third"} {
		line := serveOne(t, server, `{"jsonrpc":"2.0","id":6,"method":"session/prompt","params":{"sessionId":"`+sessionID+`","prompt":[{"type":"text","text":"`+body+`"}]}}`)
		decodeResult[map[string]json.RawMessage](t, line)
	}

	entries, err := os.ReadDir(filepath.Join(cfg.Root, "agents", testTo, "inbox", "new"))
	if err != nil {
		t.Fatalf("read inbox new: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("inbox holds %d messages, want 3", len(entries))
	}
}

func assertNoDeliveredMessages(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "agents", testTo, "inbox", "new"))
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read inbox new: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("inbox holds %d messages, want 0 after a refusal", len(entries))
	}
}
