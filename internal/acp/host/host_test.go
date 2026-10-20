package host

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// fakeAgent is an ACP agent on two pipes. It answers initialize and
// session/new, and answers each session/prompt with two agent_message_chunk
// updates and end_turn. With askPermission it first asks the client to allow
// a tool call and records the option the client selected.
type fakeAgent struct {
	askPermission bool

	mu       sync.Mutex
	methods  []string
	prompts  []string
	selected string
}

func (f *fakeAgent) serve(in io.Reader, out io.WriteCloser) {
	defer func() { _ = out.Close() }()
	scanner := bufio.NewScanner(in)
	enc := json.NewEncoder(out)
	respond := func(id json.RawMessage, result any) {
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	for scanner.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				SessionID string `json:"sessionId"`
				Prompt    []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.Method == "" {
			continue
		}
		f.mu.Lock()
		f.methods = append(f.methods, msg.Method)
		f.mu.Unlock()
		switch msg.Method {
		case "initialize":
			respond(msg.ID, map[string]any{"protocolVersion": 1, "agentInfo": map[string]string{"name": "fake", "version": "1"}})
		case "session/new":
			respond(msg.ID, map[string]string{"sessionId": "s1"})
		case "session/prompt":
			f.mu.Lock()
			f.prompts = append(f.prompts, msg.Params.Prompt[0].Text)
			f.mu.Unlock()
			if f.askPermission {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": "perm-1", "method": "session/request_permission", "params": map[string]any{
					"sessionId": "s1",
					"toolCall":  map[string]string{"toolCallId": "t1", "title": "rm -rf build"},
					"options": []map[string]string{
						{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
						{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
					},
				}})
				if !scanner.Scan() {
					return
				}
				var answer struct {
					Result struct {
						Outcome struct {
							OptionID string `json:"optionId"`
						} `json:"outcome"`
					} `json:"result"`
				}
				_ = json.Unmarshal(scanner.Bytes(), &answer)
				f.mu.Lock()
				f.selected = answer.Result.Outcome.OptionID
				f.mu.Unlock()
			}
			for _, chunk := range []string{"four, ", "as asked"} {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "s1",
					"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": chunk}},
				}})
			}
			respond(msg.ID, map[string]string{"stopReason": "end_turn"})
		}
	}
}

// startHost runs a host for grok against the fake until the test ends.
func startHost(t *testing.T, root string, agent *fakeAgent) {
	t.Helper()
	h, err := Open(Config{Root: root, Me: "grok", Cwd: root, Version: "test", Agent: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	hostOut, agentIn := io.Pipe()
	agentOut, hostIn := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); agent.serve(hostOut, hostIn) }()
	go func() {
		defer wg.Done()
		if err := h.Run(ctx, agentOut, agentIn); err != nil {
			t.Errorf("host run: %v", err)
		}
		_ = agentIn.Close()
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		h.Close()
	})
}

func newRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{"codex", "grok"} {
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// send delivers one message from codex to grok and returns it.
func send(t *testing.T, root, kind, body string) format.Message {
	t.Helper()
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: "codex", To: []string{"grok"},
		Thread: "p2p/codex__grok", Subject: "sum", Created: now.UTC().Format(time.RFC3339Nano), Kind: kind,
	}, Body: body}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dr.Close() }()
	if _, err := fsq.DeliverToInbox(dr, "grok", id+".md", data); err != nil {
		t.Fatal(err)
	}
	return msg
}

// waitReply waits for the one message in codex's inbox.
func waitReply(t *testing.T, root string) format.Message {
	t.Helper()
	dir := filepath.Join(root, "agents", "codex", "inbox", "new")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(dir)
		if len(entries) > 0 {
			if len(entries) != 1 {
				t.Fatalf("codex inbox holds %d messages; want one reply", len(entries))
			}
			msg, err := format.ReadMessageFile(filepath.Join(dir, entries[0].Name()))
			if err != nil {
				t.Fatal(err)
			}
			return msg
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no reply reached codex")
	return format.Message{}
}

func TestHostAnswersAMessageOnItsThread(t *testing.T) {
	root := newRoot(t)
	question := send(t, root, format.KindQuestion, "what is 2+2?")
	agent := &fakeAgent{}
	startHost(t, root, agent)

	reply := waitReply(t, root)
	h := reply.Header
	if h.From != "grok" || h.Thread != question.Header.Thread || h.Kind != format.KindAnswer {
		t.Fatalf("reply from=%q thread=%q kind=%q; want grok, %q, answer", h.From, h.Thread, h.Kind, question.Header.Thread)
	}
	if !slices.Equal(h.Refs, []string{question.Header.ID}) {
		t.Fatalf("reply refs=%v; want [%s]", h.Refs, question.Header.ID)
	}
	if strings.TrimRight(reply.Body, "\n") != "four, as asked" {
		t.Fatalf("reply body=%q; want the two chunks", reply.Body)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "grok", "receipts", question.Header.ID+"__grok__drained.json")); err != nil {
		t.Fatalf("no drained receipt from grok: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.prompts) != 1 || !strings.Contains(agent.prompts[0], "what is 2+2?") {
		t.Fatalf("agent prompts=%q; want one holding the body", agent.prompts)
	}
}

func TestHostRejectsAToolCall(t *testing.T) {
	root := newRoot(t)
	send(t, root, format.KindTodo, "clean the build")
	agent := &fakeAgent{askPermission: true}
	startHost(t, root, agent)

	waitReply(t, root)
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.selected != "reject" {
		t.Fatalf("host selected option %q; want the reject_once option", agent.selected)
	}
}

// A status message runs no turn: it is drained with a receipt and gets no
// reply, so two hosts can never answer each other forever.
func TestHostDrainsAStatusWithoutReply(t *testing.T) {
	root := newRoot(t)
	status := send(t, root, format.KindStatus, "done with the build")
	agent := &fakeAgent{}
	startHost(t, root, agent)

	record := filepath.Join(root, "agents", "grok", "extensions", extensionLayer, "turns", status.Header.ID+".json")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(record)
		if strings.Contains(string(data), `"state":"skipped"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status message was not skipped; record: %s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "grok", "receipts", status.Header.ID+"__grok__drained.json")); err != nil {
		t.Fatalf("no drained receipt for the status: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "agents", "codex", "inbox", "new")); len(entries) != 0 {
		t.Fatalf("codex got %d messages; a status gets no reply", len(entries))
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if slices.Contains(agent.methods, "session/prompt") {
		t.Fatal("a status message ran an ACP turn")
	}
}
