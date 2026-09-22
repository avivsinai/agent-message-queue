package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	testMe = "cursor"
	testTo = "codex"
)

// testConfig returns a routing context pointed at a fresh queue root. Delivery
// creates the recipient mailbox itself, so a bare directory is a valid root.
// Every wait is bounded well under a second so the suite stays fast.
func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		Root:              root,
		Me:                testMe,
		To:                testTo,
		StateDir:          filepath.Join(root, "meta", "acp"),
		TurnTimeout:       40 * time.Millisecond,
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 25 * time.Millisecond,
	}
}

// lineFeeder feeds the prepared request lines, then blocks until released so
// the stream stays open while prompt turns run — exactly what a real client
// does — and returns a clean EOF on release.
type lineFeeder struct {
	lines  []string
	i      int
	closed chan struct{}
	once   sync.Once
}

func (r *lineFeeder) Read(p []byte) (int, error) {
	if r.i < len(r.lines) {
		line := r.lines[r.i]
		r.i++
		return copy(p, append([]byte(line), '\n')), nil
	}
	<-r.closed
	return 0, io.EOF
}

func (r *lineFeeder) close() { r.once.Do(func() { close(r.closed) }) }

type syncedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// serve feeds request lines through one server and returns the response lines.
// The stream stays open until every id-bearing request received its response —
// a prompt turn cannot finish otherwise — then closes cleanly. Reusing a
// server across calls preserves initialize and session state exactly as a
// long-lived stdio connection would. Live-turn notifications (session/update)
// are not responses, so they are filtered out here.
func serve(t *testing.T, server *Server, lines ...string) []string {
	t.Helper()
	feeder := &lineFeeder{lines: lines, closed: make(chan struct{})}
	out := &syncedWriter{}
	done := make(chan error, 1)
	go func() { done <- server.Serve(feeder, out) }()

	want := 0
	for _, line := range lines {
		if !isNotificationLine(line) {
			want++
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for countResponses(out.String()) < want && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	feeder.close()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	written := strings.TrimSpace(out.String())
	if written == "" {
		return nil
	}
	var responses []string
	for _, line := range strings.Split(written, "\n") {
		if isNotificationLine(line) {
			continue
		}
		responses = append(responses, line)
	}
	return responses
}

// countResponses counts result/error lines (every response carries an id).
func countResponses(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if isNotificationLine(line) {
			continue
		}
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func isNotificationLine(line string) bool {
	var probe struct {
		ID     *json.RawMessage `json:"id"`
		Error  *json.RawMessage `json:"error"`
		Method string           `json:"method"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return false
	}
	return probe.ID == nil && probe.Error == nil && probe.Method != ""
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
	server, sessionID, _ := initializedServerWithThread(t, cfg)
	return server, sessionID
}

// initializedServerWithThread also returns the durable cockpit thread reported
// for the session's channel.
func initializedServerWithThread(t *testing.T, cfg Config) (*Server, string, string) {
	t.Helper()
	server := NewServer(cfg, "test")
	responses := serve(t, server,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"clientCapabilities":{"fs":{"readTextFile":true,"writeTextFile":true},"terminal":true}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`,
	)
	if len(responses) != 2 {
		t.Fatalf("setup wrote %d responses, want 2: %v", len(responses), responses)
	}
	var created struct {
		Result struct {
			SessionID string `json:"sessionId"`
			Meta      struct {
				Thread string `json:"thread"`
			} `json:"_meta"`
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
	return server, created.Result.SessionID, created.Result.Meta.Thread
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

// The live-server helpers below drive Serve through real pipes, the way an
// ACP client does: notifications interleave with the pending prompt response.

type liveServer struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Reader
	done   chan error
	server *Server
}

func startServer(t *testing.T, cfg Config) *liveServer {
	t.Helper()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	server := NewServer(cfg, "test")
	done := make(chan error, 1)
	go func() {
		err := server.Serve(inReader, outWriter)
		_ = outWriter.Close()
		done <- err
	}()
	live := &liveServer{t: t, in: inWriter, out: bufio.NewReader(outReader), done: done, server: server}
	t.Cleanup(func() {
		_ = live.in.Close()
		select {
		case err := <-live.done:
			if err != nil {
				t.Errorf("ACP server: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("ACP server did not stop")
		}
	})
	return live
}

func (s *liveServer) send(line string) {
	s.t.Helper()
	if _, err := fmt.Fprintln(s.in, line); err != nil {
		s.t.Fatalf("send ACP request: %v", err)
	}
}

func (s *liveServer) read() map[string]any {
	s.t.Helper()
	lineCh := make(chan string, 1)
	go func() {
		line, err := s.out.ReadString('\n')
		if err != nil {
			lineCh <- ""
			return
		}
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		if line == "" {
			s.t.Fatal("ACP output closed")
		}
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			s.t.Fatalf("decode ACP output %q: %v", line, err)
		}
		return value
	case <-time.After(2 * time.Second):
		s.t.Fatal("timed out waiting for ACP output")
		return nil
	}
}

func (s *liveServer) readUntilResult() map[string]any {
	s.t.Helper()
	for {
		value := s.read()
		if _, ok := value["result"]; ok {
			return value
		}
		if _, ok := value["error"]; ok {
			return value
		}
	}
}

func (s *liveServer) readUntilUpdate(updateType string) map[string]any {
	s.t.Helper()
	for {
		value := s.read()
		if value["method"] != "session/update" {
			continue
		}
		params, ok := value["params"].(map[string]any)
		if !ok {
			continue
		}
		update, ok := params["update"].(map[string]any)
		if ok && update["sessionUpdate"] == updateType {
			return value
		}
	}
}

// newLiveSession initializes and opens a session bound to channelID, returning
// the server, session id, and the durable cockpit thread.
func newLiveSession(t *testing.T, cfg Config, channelID string) (*liveServer, string, string) {
	t.Helper()
	live := startServer(t, cfg)
	live.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"clientCapabilities":{}}}`)
	initialize := live.read()
	result := initialize["result"].(map[string]any)
	if result["protocolVersion"] != float64(2) {
		t.Fatalf("ACP protocolVersion = %v, want 2", result["protocolVersion"])
	}

	request := `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","_meta":{"channelId":"` + channelID + `"}}}`
	live.send(request)
	created := live.read()["result"].(map[string]any)
	sessionID := created["sessionId"].(string)
	newMeta := created["_meta"].(map[string]any)
	threadID := newMeta["thread"].(string)
	return live, sessionID, threadID
}

func promptRequest(id int, sessionID, body string) string {
	encoded, _ := json.Marshal(body)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":%s}]}}`, id, sessionID, encoded)
}

func readInboxMessage(t *testing.T, root, agent string) format.Message {
	return readInboxMessageSubject(t, root, agent, "")
}

func readInboxMessageSubject(t *testing.T, root, agent, subject string) format.Message {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(fsq.AgentInboxNew(root, agent))
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
					continue
				}
				message, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, agent), entry.Name()))
				if err == nil && (subject == "" || message.Header.Subject == subject) {
					return message
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for message in %s", fsq.AgentInboxNew(root, agent))
	return format.Message{}
}

// deliverReply delivers a message from the configured peer on threadID, the
// same way an amq reply arrives.
func deliverReply(t *testing.T, cfg Config, threadID, body string) {
	t.Helper()
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		t.Fatalf("new reply id: %v", err)
	}
	message := format.Message{Header: format.Header{
		Schema:  format.CurrentSchema,
		ID:      id,
		From:    cfg.To,
		To:      []string{cfg.Me},
		Thread:  threadID,
		Subject: "reply",
		Created: now.UTC().Format(time.RFC3339Nano),
	}, Body: body}
	data, err := message.Marshal()
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(cfg.Root)
	if err != nil {
		t.Fatalf("snapshot reply root: %v", err)
	}
	root, err := fsq.OpenDeliveryRoot(cfg.Root, identity)
	if err != nil {
		t.Fatalf("open reply root: %v", err)
	}
	defer func() { _ = root.Close() }()
	if _, err := fsq.DeliverToInboxes(root, []string{cfg.Me}, id+".md", data); err != nil {
		t.Fatalf("deliver reply: %v", err)
	}
}

// TestInitializeAnswersProtocolVersionTwo proves the bridge reports ACP v2,
// the version whose prompt turns stay open for live replies.
func TestInitializeAnswersProtocolVersionTwo(t *testing.T) {
	server := NewServer(testConfig(t), "test")
	line := serveOne(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"clientCapabilities":{}}}`)

	result := decodeResult[struct {
		ProtocolVersion int `json:"protocolVersion"`
	}](t, line)
	if result.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocolVersion = %d, want %d", result.ProtocolVersion, ProtocolVersion)
	}
}

func TestInitializeRejectsUnknownField(t *testing.T) {
	server := NewServer(testConfig(t), "test")
	line := serveOne(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"bogus":true}}`)
	if code := decodeError(t, line).Code; code != codeInvalidParams {
		t.Fatalf("error code = %d, want %d", code, codeInvalidParams)
	}
}

func TestInitializeAcceptsReservedMetaAndRicherCapabilities(t *testing.T) {
	server := NewServer(testConfig(t), "test")
	line := serveOne(t, server, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"clientCapabilities":{"fs":{"readTextFile":true}},"_meta":{"untouched":true}}}`)
	decodeResult[map[string]json.RawMessage](t, line)
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	server := NewServer(testConfig(t), "test")
	for _, method := range []string{"session/load", "totally/unknown"} {
		t.Run(method, func(t *testing.T) {
			line := serveOne(t, server, `{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{}}`)
			if code := decodeError(t, line).Code; code != codeMethodNotFound {
				t.Errorf("error code = %d, want %d", code, codeMethodNotFound)
			}
		})
	}
}

// TestPromptRoundTripAndThreadState proves the exercisable core: the prompt
// lands on the session's durable cockpit thread, the client sees the live
// thought update, and a fresh reply completes the turn with its text.
func TestPromptRoundTripAndThreadState(t *testing.T) {
	cfg := testConfig(t)
	live, sessionID, threadID := newLiveSession(t, cfg, "cockpit-42")
	live.send(promptRequest(3, sessionID, "please inspect the bridge"))
	thought := live.readUntilUpdate("agent_thought_chunk")
	thoughtParams := thought["params"].(map[string]any)
	if thoughtParams["update"].(map[string]any)["sessionUpdate"] != "agent_thought_chunk" {
		t.Fatalf("first update = %v, want agent_thought_chunk", thoughtParams)
	}
	prompt := readInboxMessage(t, cfg.Root, cfg.To)
	if prompt.Header.Thread != threadID || prompt.Header.Subject != CockpitPromptSubject {
		t.Fatalf("prompt header = %+v, want cockpit thread %q and subject %q", prompt.Header, threadID, CockpitPromptSubject)
	}
	if prompt.Header.Thread != "cockpit/cockpit-42" {
		t.Fatalf("prompt thread = %q, want cockpit/cockpit-42 (channel-derived)", prompt.Header.Thread)
	}
	deliverReply(t, cfg, threadID, "bridge reply")

	messageUpdate := live.readUntilUpdate("agent_message_chunk")
	update := messageUpdate["params"].(map[string]any)["update"].(map[string]any)
	if update["sessionUpdate"] != "agent_message_chunk" || update["content"].(map[string]any)["text"] != "bridge reply" {
		t.Fatalf("reply update = %v", update)
	}
	response := live.readUntilResult()["result"].(map[string]any)
	if response["stopReason"] != StopReasonEndTurn {
		t.Fatalf("stopReason = %v, want %s", response["stopReason"], StopReasonEndTurn)
	}
	responseMeta := response["_meta"].(map[string]any)["amq"].(map[string]any)
	if responseMeta["state"] != DeliveryStateReplied || responseMeta["reply"] != "bridge reply" {
		t.Fatalf("reply metadata = %v", responseMeta)
	}
}

// TestPromptTimeoutIsTypedNoReplyRefusal proves a bounded wait that expires is
// a typed refusal naming the state, never a fabricated success.
func TestPromptTimeoutIsTypedNoReplyRefusal(t *testing.T) {
	cfg := testConfig(t)
	cfg.TurnTimeout = 35 * time.Millisecond
	live, sessionID, _ := newLiveSession(t, cfg, "cockpit-timeout")
	live.send(promptRequest(3, sessionID, "no one will answer"))
	_ = live.read()
	response := live.readUntilResult()["result"].(map[string]any)
	if response["stopReason"] != StopReasonRefusal {
		t.Fatalf("stopReason = %v, want %s", response["stopReason"], StopReasonRefusal)
	}
	meta := response["_meta"].(map[string]any)["amq"].(map[string]any)
	if meta["state"] != DeliveryStateNoReply || meta["reason"] != "reply_timeout" {
		t.Fatalf("timeout metadata = %v", meta)
	}
}

// TestStaleReplyIsNeverPickedUp proves reply correlation is fresh-only: a
// message the peer sent before the prompt cannot answer the turn.
func TestStaleReplyIsNeverPickedUp(t *testing.T) {
	cfg := testConfig(t)
	live, sessionID, threadID := newLiveSession(t, cfg, "cockpit-stale")
	deliverReply(t, cfg, threadID, "answer to a previous question")
	live.send(promptRequest(3, sessionID, "a brand new question"))
	_ = live.read()
	response := live.readUntilResult()["result"].(map[string]any)
	if response["stopReason"] != StopReasonRefusal {
		t.Fatalf("stopReason = %v, want %s after only a stale reply", response["stopReason"], StopReasonRefusal)
	}
	meta := response["_meta"].(map[string]any)["amq"].(map[string]any)
	if meta["state"] != DeliveryStateNoReply {
		t.Fatalf("state = %v, want %s", meta["state"], DeliveryStateNoReply)
	}
}

// TestChannelThreadMappingSurvivesRespawn proves the channel-to-thread mapping
// persists under the state dir, so a companion restart resumes the same AMQ
// thread instead of minting a new one.
func TestChannelThreadMappingSurvivesRespawn(t *testing.T) {
	cfg := testConfig(t)
	first, _, threadID := newLiveSession(t, cfg, "cockpit-persist")
	_ = first
	second, _, resumedThread := newLiveSession(t, cfg, "cockpit-persist")
	if resumedThread != threadID {
		t.Fatalf("resumed thread = %q, want %q", resumedThread, threadID)
	}
	state, err := os.ReadFile(filepath.Join(cfg.StateDir, sessionStateFilename))
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}
	if !strings.Contains(string(state), `"cockpit-persist"`) || !strings.Contains(string(state), threadID) {
		t.Fatalf("persisted state lacks channel/thread mapping: %s", state)
	}
	_ = second
}

// TestClientDisconnectDoesNotHoldTheTurn proves a closed ACP input stream ends
// in-flight turns promptly with a typed refusal instead of waiting out the
// full turn timeout, and never retracts the queued AMQ message. The input is a
// reader that returns a clean EOF once released — exactly what a client
// closing its stdin looks like to Serve.
type eofOnCloseReader struct {
	lines  [][]byte
	at     int
	gate   chan struct{} // released when the prompt line is set
	closed chan struct{} // released to return the clean EOF
}

func (r *eofOnCloseReader) Read(p []byte) (int, error) {
	if r.at < len(r.lines) {
		if r.at == len(r.lines)-1 {
			<-r.gate
		}
		line := r.lines[r.at]
		r.at++
		n := copy(p, append(line, '\n'))
		return n, nil
	}
	<-r.closed
	return 0, io.EOF
}

func TestClientDisconnectDoesNotHoldTheTurn(t *testing.T) {
	cfg := testConfig(t)
	cfg.TurnTimeout = 30 * time.Second // would hang the suite if the turn leaked
	server := NewServer(cfg, "test")
	reader := &eofOnCloseReader{gate: make(chan struct{}), closed: make(chan struct{})}
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"clientCapabilities":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}`,
		promptRequest(3, "", "placeholder"),
	} {
		reader.lines = append(reader.lines, []byte(line))
	}
	out := &syncedWriter{}
	done := make(chan error, 1)
	go func() { done <- server.Serve(reader, out) }()

	// Read the initialize and session/new responses, capture the session id.
	sessionID := ""
	deadline := time.Now().Add(2 * time.Second)
	for sessionID == "" && time.Now().Before(deadline) {
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) >= 2 {
			var created struct {
				Result struct {
					SessionID string `json:"sessionId"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(lines[1]), &created); err == nil {
				sessionID = created.Result.SessionID
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sessionID == "" {
		t.Fatalf("no session id in output: %s", out.String())
	}
	reader.lines[2] = []byte(promptRequest(3, sessionID, "still running when the client leaves"))
	close(reader.gate)

	// The prompt response must be the no-reply refusal with a reason naming
	// the disconnect, never the 30s reply_timeout.
	close(reader.closed)
	var refusal string
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		for _, line := range strings.Split(out.String(), "\n") {
			if isNotificationLine(line) {
				continue
			}
			var envelope struct {
				ID     *int `json:"id"`
				Result struct {
					Meta struct {
						AMQ struct {
							Reason string `json:"reason"`
						} `json:"amq"`
					} `json:"_meta"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(line), &envelope); err == nil && envelope.ID != nil && *envelope.ID == 3 {
				refusal = envelope.Result.Meta.AMQ.Reason
			}
		}
		if refusal == "client_disconnected" {
			break
		}
	}
	if refusal != "client_disconnected" {
		t.Fatalf("in-flight turn refusal reason = %q, want client_disconnected; output:\n%s", refusal, out.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after disconnect: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after the client disconnected")
	}
}

// oversizedLineReader feeds the prepared request lines, then — once closed
// is released — serves an endless chunk larger than the scanner's max token,
// so the scanner fails with "token too long" instead of returning EOF.
type oversizedLineReader struct {
	lines  [][]byte
	at     int
	broken []byte
	gate   chan struct{}
	closed chan struct{}
}

func (r *oversizedLineReader) Read(p []byte) (int, error) {
	if r.at < len(r.lines) {
		if r.at == len(r.lines)-1 {
			<-r.gate
		}
		line := r.lines[r.at]
		r.at++
		return copy(p, append(line, '\n')), nil
	}
	<-r.closed
	return copy(p, r.broken), nil
}

// TestOversizedInputEndsTheTurnPromptly is the regression for the observed
// wedge: an oversized input line (a scanner error) left in-flight turns
// running until the full turn timeout, because cancelAll ran only on a clean
// EOF. The oversized line must end the turn with the typed
// client_disconnected refusal, Serve must return the scanner error promptly,
// and the queued AMQ message must remain.
func TestOversizedInputEndsTheTurnPromptly(t *testing.T) {
	cfg := testConfig(t)
	cfg.TurnTimeout = 30 * time.Second // would hang the suite if the turn leaked
	server := NewServer(cfg, "test")
	reader := &oversizedLineReader{
		lines: [][]byte{
			[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"clientCapabilities":{}}}`),
			[]byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}`),
			[]byte(promptRequest(3, "", "placeholder")),
		},
		// Larger than the scanner's max token (format.MaxMessageSize+1024).
		broken: bytes.Repeat([]byte{'x'}, format.MaxMessageSize+2048),
		gate:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	out := &syncedWriter{}
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- server.Serve(reader, out) }()

	sessionID := ""
	deadline := time.Now().Add(2 * time.Second)
	for sessionID == "" && time.Now().Before(deadline) {
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) >= 2 {
			var created struct {
				Result struct {
					SessionID string `json:"sessionId"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(lines[1]), &created); err == nil {
				sessionID = created.Result.SessionID
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if sessionID == "" {
		t.Fatalf("no session id in output: %s", out.String())
	}
	reader.lines[2] = []byte(promptRequest(3, sessionID, "still running when the line breaks"))
	close(reader.gate)
	// Let the turn start before the input stream breaks.
	time.Sleep(50 * time.Millisecond)
	close(reader.closed)

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "token too long") {
			t.Fatalf("Serve error = %v, want the scanner token-too-long error", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("Serve took %v after the oversized line; the turn must not hold the process", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the oversized input line")
	}

	var refusal, messageID string
	for _, line := range strings.Split(out.String(), "\n") {
		if isNotificationLine(line) {
			continue
		}
		var envelope struct {
			ID     *int `json:"id"`
			Result struct {
				Meta struct {
					AMQ struct {
						MessageID string `json:"messageId"`
						Reason    string `json:"reason"`
					} `json:"amq"`
				} `json:"_meta"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err == nil && envelope.ID != nil && *envelope.ID == 3 {
			refusal = envelope.Result.Meta.AMQ.Reason
			messageID = envelope.Result.Meta.AMQ.MessageID
		}
	}
	if refusal != "client_disconnected" {
		t.Fatalf("in-flight turn refusal reason = %q, want client_disconnected; output:\n%s", refusal, out.String())
	}
	// The queued AMQ message must remain: a broken stream never retracts delivery.
	if messageID == "" {
		t.Fatal("no message id in the prompt response; the delivery evidence is missing")
	}
	msgPath := filepath.Join(cfg.Root, "agents", testTo, "inbox", "new", messageID+".md")
	if _, err := os.Stat(msgPath); err != nil {
		t.Fatalf("queued message missing after the oversized input: %v", err)
	}
}

// TestSessionPromptDeliversMessageIntoRecipientInbox proves the prompt becomes a
// real, parseable message in the recipient's inbox/new.
func TestSessionPromptDeliversMessageIntoRecipientInbox(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID, threadID := initializedServerWithThread(t, cfg)

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
				Reason    string `json:"reason"`
			} `json:"amq"`
		} `json:"_meta"`
	}](t, line)

	if result.StopReason != StopReasonRefusal {
		t.Errorf("stopReason = %q, want %q; no reply was delivered", result.StopReason, StopReasonRefusal)
	}
	if result.Meta.AMQ.State != DeliveryStateNoReply {
		t.Errorf("delivery state = %q, want %q", result.Meta.AMQ.State, DeliveryStateNoReply)
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
	if message.Header.Thread != threadID {
		t.Errorf("header thread = %q, want the session's durable thread %q", message.Header.Thread, threadID)
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

// TestSessionMethodsRefuseBeforeInitialize keeps the ordering requirement
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

// TestSessionPromptDeliversResourceLink proves a resource_link block renders as
// a markdown link in the delivered body, labeled by name or by title when set.
func TestSessionPromptDeliversResourceLink(t *testing.T) {
	for _, tc := range []struct {
		block string
		want  string
	}{
		{`{"type":"resource_link","uri":"file:///tmp/report.md","name":"report.md"}`, "[report.md](file:///tmp/report.md)"},
		{`{"type":"resource_link","uri":"file:///tmp/report.md","name":"report.md","title":"Weekly Report"}`, "[Weekly Report](file:///tmp/report.md)"},
	} {
		cfg := testConfig(t)
		server, sessionID := initializedServer(t, cfg)

		line := serveOne(t, server, `{"jsonrpc":"2.0","id":10,"method":"session/prompt","params":{"sessionId":"`+sessionID+`","prompt":[`+tc.block+`]}}`)
		result := decodeResult[struct {
			Meta struct {
				AMQ struct {
					MessageID string `json:"messageId"`
				} `json:"amq"`
			} `json:"_meta"`
		}](t, line)

		path := filepath.Join(cfg.Root, "agents", testTo, "inbox", "new", result.Meta.AMQ.MessageID+".md")
		message, err := format.ReadMessageFile(path)
		if err != nil {
			t.Fatalf("read delivered message %s: %v", path, err)
		}
		if got := strings.TrimSpace(message.Body); got != tc.want {
			t.Errorf("delivered body = %q, want %q", got, tc.want)
		}
	}
}

// TestSessionPromptJoinsTextAndResourceLink proves mixed baseline blocks join
// with a newline in prompt order.
func TestSessionPromptJoinsTextAndResourceLink(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)

	line := serveOne(t, server, `{"jsonrpc":"2.0","id":11,"method":"session/prompt","params":{"sessionId":"`+sessionID+`","prompt":[{"type":"text","text":"please review"},{"type":"resource_link","uri":"file:///tmp/diff.patch","name":"diff.patch"}]}}`)
	result := decodeResult[struct {
		Meta struct {
			AMQ struct {
				MessageID string `json:"messageId"`
			} `json:"amq"`
		} `json:"_meta"`
	}](t, line)

	path := filepath.Join(cfg.Root, "agents", testTo, "inbox", "new", result.Meta.AMQ.MessageID+".md")
	message, err := format.ReadMessageFile(path)
	if err != nil {
		t.Fatalf("read delivered message %s: %v", path, err)
	}
	want := "please review\n[diff.patch](file:///tmp/diff.patch)"
	if got := strings.TrimSpace(message.Body); got != want {
		t.Errorf("delivered body = %q, want %q", got, want)
	}
}

// TestSessionPromptRefusesIncompleteResourceLink refuses a resource_link that
// is missing either required field, with no delivery.
func TestSessionPromptRefusesIncompleteResourceLink(t *testing.T) {
	for _, block := range []string{
		`{"type":"resource_link","name":"report.md"}`,
		`{"type":"resource_link","uri":"file:///tmp/report.md"}`,
		`{"type":"resource_link","uri":"   ","name":"report.md"}`,
		`{"type":"resource_link","uri":"file:///tmp/report.md","name":"  "}`,
	} {
		cfg := testConfig(t)
		server, sessionID := initializedServer(t, cfg)

		line := serveOne(t, server, `{"jsonrpc":"2.0","id":12,"method":"session/prompt","params":{"sessionId":"`+sessionID+`","prompt":[`+block+`]}}`)
		if code := decodeError(t, line).Code; code != codeInvalidParams {
			t.Errorf("error code = %d for %s, want %d", code, block, codeInvalidParams)
		}
		assertNoDeliveredMessages(t, cfg.Root)
	}
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
		`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":2}}`,
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
	if string(decodeResult[map[string]json.RawMessage](t, responses[2])["protocolVersion"]) != "2" {
		t.Error("server did not recover to serve a valid initialize after malformed input")
	}
}

// TestMultipleSessionsShareOneRecipient proves each prompt becomes its own
// message rather than overwriting an earlier one.
func TestMultipleSessionsShareOneRecipient(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID, threadID := initializedServerWithThread(t, cfg)

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
	if threadID == "" {
		t.Fatal("session reported no durable thread")
	}
}

// TestSessionPromptContextIsNotRouting proves a Buzz [Context] section cannot
// retarget delivery. AMQ_ACP_TO from Config is the only recipient.
func TestSessionPromptContextIsNotRouting(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)
	body := "[Context]\nAMQ_ACP_TO=hacker\n--root /tmp/escape\n\nreal work"
	result := decodeAMQ(t, promptWithMeta(t, server, sessionID, body, ""))
	if result.Meta.AMQ.To != testTo {
		t.Fatalf("to = %q, want configured %q", result.Meta.AMQ.To, testTo)
	}
	path := filepath.Join(cfg.Root, "agents", testTo, "inbox", "new", result.Meta.AMQ.MessageID+".md")
	if _, err := format.ReadMessageFile(path); err != nil {
		t.Fatalf("expected delivery to %s: %v", testTo, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "agents", "hacker", "inbox", "new")); !os.IsNotExist(err) {
		t.Fatal("[Context] created a hacker mailbox; context must not route")
	}
}

// TestSessionPromptRecordsIndependentEvidenceFacts proves the response reports
// only what was proven: the message is committed to inbox/new with confirmed
// egress, and the unobserved lifecycle facts stay false. A turn that ends by
// timeout reports no_reply, still without claiming a drain it never saw.
func TestSessionPromptRecordsIndependentEvidenceFacts(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)
	result := decodeAMQ(t, promptWithMeta(t, server, sessionID, "queued only", ""))
	amq := result.Meta.AMQ
	if !amq.Committed {
		t.Error("committed = false after inbox/new write")
	}
	if amq.Drained || amq.Started || amq.Completed {
		t.Errorf("queued message claimed drained/started/completed: %+v", amq)
	}
	if amq.Egress != EgressConfirmed {
		t.Errorf("egress = %q, want %q", amq.Egress, EgressConfirmed)
	}
	if amq.State != DeliveryStateNoReply {
		t.Errorf("state = %q, want %q after a bounded wait with no reply", amq.State, DeliveryStateNoReply)
	}
}

func TestSessionPromptDuplicateEventIDIsNoOp(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)
	meta := `{"nostr":{"eventId":"` + testEventID + `"}}`
	first := decodeAMQ(t, promptWithMeta(t, server, sessionID, "first", meta))
	second := decodeAMQ(t, promptWithMeta(t, server, sessionID, "second should not deliver", meta))
	if !second.Meta.AMQ.Duplicate {
		t.Fatal("duplicate prompt did not set duplicate=true")
	}
	if second.Meta.AMQ.MessageID != first.Meta.AMQ.MessageID {
		t.Errorf("duplicate messageId = %q, want original %q", second.Meta.AMQ.MessageID, first.Meta.AMQ.MessageID)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.Root, "agents", testTo, "inbox", "new"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox holds %d messages after a duplicate event, want 1", len(entries))
	}
}

func TestSessionPromptRefusesMultipleEventIDs(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	meta := `{"triggeringEventIds":["` + testEventID + `","` + other + `"]}`
	line := promptWithMeta(t, server, sessionID, "coalesced batch", meta)
	if code := decodeError(t, line).Code; code != codeInvalidParams {
		t.Fatalf("error code = %d, want %d", code, codeInvalidParams)
	}
	assertNoDeliveredMessages(t, cfg.Root)
}

func TestSessionPromptRefusesMalformedEventID(t *testing.T) {
	cfg := testConfig(t)
	server, sessionID := initializedServer(t, cfg)
	line := promptWithMeta(t, server, sessionID, "bad id", `{"nostr":{"eventId":"not-an-event-id"}}`)
	if code := decodeError(t, line).Code; code != codeInvalidParams {
		t.Fatalf("error code = %d, want %d", code, codeInvalidParams)
	}
	assertNoDeliveredMessages(t, cfg.Root)
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

const testEventID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func promptWithMeta(t *testing.T, server *Server, sessionID, body, meta string) string {
	t.Helper()
	req := `{"jsonrpc":"2.0","id":8,"method":"session/prompt","params":{"sessionId":"` + sessionID + `","prompt":[{"type":"text","text":` + mustJSONString(t, body) + `}]`
	if meta != "" {
		req += `,"_meta":` + meta
	}
	req += `}}`
	return serveOne(t, server, req)
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decodeAMQ(t *testing.T, line string) struct {
	StopReason string `json:"stopReason"`
	Meta       struct {
		AMQ amqDelivery `json:"amq"`
	} `json:"_meta"`
} {
	t.Helper()
	return decodeResult[struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			AMQ amqDelivery `json:"amq"`
		} `json:"_meta"`
	}](t, line)
}
