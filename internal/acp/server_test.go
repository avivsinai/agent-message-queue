package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	testMe = "cursor"
	testTo = "codex"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		Root:              root,
		Me:                testMe,
		To:                testTo,
		StateDir:          filepath.Join(root, "meta", "acp"),
		TurnTimeout:       250 * time.Millisecond,
		IdleTimeout:       20 * time.Millisecond,
		PollInterval:      5 * time.Millisecond,
		HeartbeatInterval: 25 * time.Millisecond,
	}
}

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

func newLiveSession(t *testing.T, cfg Config, channelID string) (*liveServer, string, string) {
	t.Helper()
	live := startServer(t, cfg)
	live.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"clientCapabilities":{}}}`)
	initialize := live.read()
	result := initialize["result"].(map[string]any)
	if result["protocolVersion"] != float64(2) {
		t.Fatalf("ACP protocolVersion = %v, want 2", result["protocolVersion"])
	}
	meta := result["_meta"].(map[string]any)
	steering := meta["steering"].(map[string]any)
	if steering["supported"] != true {
		t.Fatal("ACP initialize did not advertise steering")
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

func steeringRequest(id int, sessionID, body string) string {
	encoded, _ := json.Marshal(body)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"_session/steering","params":{"sessionId":%q,"prompt":%s}}`, id, sessionID, encoded)
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

func TestInitializeAdvertisesLiveV2Steering(t *testing.T) {
	live := startServer(t, testConfig(t))
	live.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"clientCapabilities":{}}}`)
	result := live.read()["result"].(map[string]any)
	if result["protocolVersion"] != float64(2) {
		t.Fatalf("protocolVersion = %v, want 2", result["protocolVersion"])
	}
	meta := result["_meta"].(map[string]any)
	if meta["steering"].(map[string]any)["supported"] != true {
		t.Fatal("steering capability is not supported")
	}
}

func TestPromptRoundTripAndThreadState(t *testing.T) {
	cfg := testConfig(t)
	live, sessionID, threadID := newLiveSession(t, cfg, "cockpit-42")
	live.send(promptRequest(3, sessionID, "please inspect the bridge"))
	thought := live.readUntilUpdate("agent_thought_chunk")
	if thought["method"] != "session/update" {
		t.Fatalf("first prompt output = %v, want session/update", thought)
	}
	thoughtParams := thought["params"].(map[string]any)
	if thoughtParams["update"].(map[string]any)["sessionUpdate"] != "agent_thought_chunk" {
		t.Fatalf("first update = %v, want agent_thought_chunk", thoughtParams)
	}
	prompt := readInboxMessage(t, cfg.Root, cfg.To)
	if prompt.Header.Thread != threadID || prompt.Header.Priority != format.PriorityNormal {
		t.Fatalf("prompt header = %+v, want thread %q and normal priority", prompt.Header, threadID)
	}
	deliverReply(t, cfg, threadID, "bridge reply")

	messageUpdate := live.readUntilUpdate("agent_message_chunk")
	if messageUpdate["method"] != "session/update" {
		t.Fatalf("reply update = %v, want session/update", messageUpdate)
	}
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

func TestSteeringUsesUrgentMidTurnAndNormalWhenIdle(t *testing.T) {
	cfg := testConfig(t)
	live, sessionID, threadID := newLiveSession(t, cfg, "cockpit-steer")
	live.send(promptRequest(3, sessionID, "long task"))
	_ = live.readUntilUpdate("agent_thought_chunk")
	_ = readInboxMessage(t, cfg.Root, cfg.To)

	live.send(steeringRequest(4, sessionID, "change the next step"))
	injected := live.readUntilResult()["result"].(map[string]any)
	if injected["outcome"] != "injected" {
		t.Fatalf("mid-turn steering outcome = %v, want injected", injected["outcome"])
	}
	steer := readInboxMessageSubject(t, cfg.Root, cfg.To, CockpitSteeringSubject)
	if steer.Header.Thread != threadID || steer.Header.Priority != format.PriorityUrgent {
		t.Fatalf("steer header = %+v, want urgent on %q", steer.Header, threadID)
	}
	if !contains(steer.Header.Labels, "buzz-steer") || !strings.HasPrefix(steer.Body, steeringBodyPrefix) || !strings.Contains(steer.Body, steeringBodySuffix) {
		t.Fatalf("steer labels/body do not carry the native interrupt contract: labels=%v body=%q", steer.Header.Labels, steer.Body)
	}
	deliverReply(t, cfg, threadID, "done")
	_ = live.readUntilUpdate("agent_message_chunk")
	_ = live.readUntilResult()

	idleCfg := testConfig(t)
	idle, idleSession, idleThread := newLiveSession(t, idleCfg, "cockpit-idle")
	idle.send(steeringRequest(3, idleSession, "next instruction"))
	started := idle.readUntilResult()["result"].(map[string]any)
	if started["outcome"] != "startedNewTurn" {
		t.Fatalf("idle steering outcome = %v, want startedNewTurn", started["outcome"])
	}
	idleSteer := readInboxMessage(t, idleCfg.Root, idleCfg.To)
	if idleSteer.Header.Thread != idleThread || idleSteer.Header.Priority != format.PriorityNormal || contains(idleSteer.Header.Labels, "buzz-steer") {
		t.Fatalf("idle steering header = %+v, want normal without buzz-steer", idleSteer.Header)
	}
}

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

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
