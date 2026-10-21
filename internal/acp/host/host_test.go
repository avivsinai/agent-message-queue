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
// a tool call and records the option the client selected. With loadSession it
// advertises session loading and answers session/load after replaying an
// earlier answer. With holdUntilCancel it answers a prompt only after
// session/cancel, with stop reason cancelled.
type fakeAgent struct {
	askPermission   bool
	loadSession     bool
	holdUntilCancel bool
	// gate, when set, holds each prompt's answer until it is closed.
	gate chan struct{}

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
	var held json.RawMessage
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
			respond(msg.ID, map[string]any{
				"protocolVersion":   1,
				"agentInfo":         map[string]string{"name": "fake", "version": "1"},
				"agentCapabilities": map[string]bool{"loadSession": f.loadSession},
			})
		case "session/load":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": msg.Params.SessionID,
				"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "an earlier answer"}},
			}})
			respond(msg.ID, map[string]any{})
		case "session/new":
			respond(msg.ID, map[string]string{"sessionId": "s1"})
		case "session/cancel":
			if held != nil {
				respond(held, map[string]string{"stopReason": "cancelled"})
				held = nil
			}
		case "session/prompt":
			f.mu.Lock()
			f.prompts = append(f.prompts, msg.Params.Prompt[0].Text)
			f.mu.Unlock()
			if f.gate != nil {
				<-f.gate
			}
			if f.holdUntilCancel {
				held = msg.ID
				continue
			}
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

// send delivers one message from a sender to grok and returns it.
func send(t *testing.T, root, from, kind, body string) format.Message {
	t.Helper()
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	msg := format.Message{Header: format.Header{
		Schema: format.CurrentSchema, ID: id, From: from, To: []string{"grok"},
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
	question := send(t, root, "codex", format.KindQuestion, "what is 2+2?")
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
	send(t, root, "codex", format.KindTodo, "clean the build")
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
	status := send(t, root, "codex", format.KindStatus, "done with the build")
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

// Review of the first host slice (P1): a question from a bridged sender
// (peer/handle) was prompted, its reply failed handle validation, and the
// host exited; every restart replayed the same reply and exited again. Such
// a sender is now skipped before any prompt, and the next message runs.
func TestHostSkipsASenderItCannotReplyTo(t *testing.T) {
	root := newRoot(t)
	bridged := send(t, root, "peer/codex", format.KindQuestion, "from another host")
	question := send(t, root, "codex", format.KindQuestion, "what is 2+2?")
	agent := &fakeAgent{}
	startHost(t, root, agent)

	reply := waitReply(t, root)
	if !slices.Equal(reply.Header.Refs, []string{question.Header.ID}) {
		t.Fatalf("reply refs=%v; want the local question %s", reply.Header.Refs, question.Header.ID)
	}
	data, err := os.ReadFile(filepath.Join(root, "agents", "grok", "extensions", extensionLayer, "turns", bridged.Header.ID+".json"))
	if err != nil || !strings.Contains(string(data), `"state":"skipped"`) {
		t.Fatalf("bridged question record=%s (%v); want skipped", data, err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.prompts) != 1 {
		t.Fatalf("agent ran %d turns; want one, for the local question", len(agent.prompts))
	}
}

func TestHostResumesItsSessionWithoutReplayingOldText(t *testing.T) {
	root := newRoot(t)
	dir := filepath.Join(root, "agents", "grok", "extensions", extensionLayer)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	saved, err := json.Marshal(sessionRecord{SessionID: "s1", Agent: "fake", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), saved, 0o600); err != nil {
		t.Fatal(err)
	}
	send(t, root, "codex", format.KindQuestion, "and now?")
	agent := &fakeAgent{loadSession: true}
	startHost(t, root, agent)

	reply := waitReply(t, root)
	if strings.TrimRight(reply.Body, "\n") != "four, as asked" {
		t.Fatalf("reply body=%q; want only this turn's chunks, not the replayed history", reply.Body)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !slices.Contains(agent.methods, "session/load") || slices.Contains(agent.methods, "session/new") {
		t.Fatalf("agent methods=%v; want session/load and no session/new", agent.methods)
	}
}

func TestCheckReportsTheAgentsCapabilities(t *testing.T) {
	hostOut, agentIn := io.Pipe()
	agentOut, hostIn := io.Pipe()
	go (&fakeAgent{loadSession: true}).serve(hostOut, hostIn)
	defer func() { _ = agentIn.Close() }()

	res, err := Check(context.Background(), agentOut, agentIn, "test")
	if err != nil {
		t.Fatal(err)
	}
	if res.ProtocolVersion != 1 || res.AgentInfo.Name != "fake" || !res.LoadSession {
		t.Fatalf("check result=%+v; want protocol 1, agent fake, load_session true", res)
	}
}

const (
	testRef  = "amqr1_abcdefghijklmnopqrst"
	otherRef = "amqr1_bcdefghijklmnopqrstu"
)

// publishRequest plays the amq-remote adapter: it reads the live session
// generation and publishes one request for it.
func publishRequest(t *testing.T, root, ref, text string) {
	t.Helper()
	publishRequests(t, root, map[string]string{ref: text})
}

// publishRequests publishes several requests at once: they appear together,
// so the host sees all of them in one scan.
func publishRequests(t *testing.T, root string, texts map[string]string) {
	t.Helper()
	dir := filepath.Join(root, "agents", "grok", "extensions", extensionLayer)
	var live bridgeLiveness
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "bridge.liveness"))
		if err == nil && json.Unmarshal(data, &live) == nil && live.Live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the host published no live bridge.liveness")
		}
		time.Sleep(10 * time.Millisecond)
	}
	staging := filepath.Join(dir, "requests.staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	for ref, text := range texts {
		req, err := json.Marshal(bridgeRequest{
			Ref: ref, Text: text, DeliverAs: "followUp", EpochHint: live.SessionGeneration, BridgeRevision: 3,
			NotAfter: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(staging, ref+".json")
		if err := os.WriteFile(path, req, 0o600); err != nil {
			t.Fatal(err)
		}
		// One publication time: the host then takes them in ref order.
		if err := os.Chtimes(path, live.at(), live.at()); err != nil {
			t.Fatal(err)
		}
	}
	requests := filepath.Join(dir, "requests")
	if _, err := os.Stat(requests); err != nil {
		// The first publication moves the whole directory in at once.
		if err := os.Rename(staging, requests); err != nil {
			t.Fatal(err)
		}
		return
	}
	for ref := range texts {
		if err := os.Rename(filepath.Join(staging, ref+".json"), filepath.Join(requests, ref+".json")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(staging); err != nil {
		t.Fatal(err)
	}
}

// waitEvent waits for an event line of the given type on one request.
func waitEvent(t *testing.T, root, ref, event string) []bridgeEvent {
	t.Helper()
	path := filepath.Join(root, "agents", "grok", "extensions", extensionLayer, "events", ref+".jsonl")
	deadline := time.Now().Add(4 * time.Second)
	for {
		data, _ := os.ReadFile(path)
		var events []bridgeEvent
		for _, line := range strings.Split(string(data), "\n") {
			var ev bridgeEvent
			if json.Unmarshal([]byte(line), &ev) == nil {
				events = append(events, ev)
			}
		}
		if slices.ContainsFunc(events, func(ev bridgeEvent) bool { return ev.Event == event }) {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s event; stream: %s", event, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHostRunsARemoteRequest(t *testing.T) {
	root := newRoot(t)
	agent := &fakeAgent{}
	startHost(t, root, agent)
	publishRequest(t, root, testRef, "summarize the diff")

	events := waitEvent(t, root, testRef, "completed")
	if _, err := os.Stat(filepath.Join(root, "agents", "grok", "extensions", extensionLayer, "receipts", testRef+".json")); err != nil {
		t.Fatalf("no receipt for the request: %v", err)
	}
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Event)
	}
	if !slices.Equal(kinds, []string{"started", "completed"}) || events[1].Text != "four, as asked" {
		t.Fatalf("events=%v text=%q; want started then completed with the answer", kinds, events[len(events)-1].Text)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !slices.Equal(agent.prompts, []string{"summarize the diff"}) {
		t.Fatalf("agent prompts=%q; want the request text verbatim", agent.prompts)
	}
}

func TestHostTakesARejectFromTheRemoteFace(t *testing.T) {
	root := newRoot(t)
	agent := &fakeAgent{askPermission: true}
	startHost(t, root, agent)
	publishRequest(t, root, testRef, "clean the build")

	events := waitEvent(t, root, testRef, "interaction")
	ask := events[len(events)-1]
	answer, err := json.Marshal(map[string]string{
		"protocol": BridgeProtocol, "ref": testRef, "interaction_id": ask.InteractionID,
		"manifest_hash": ask.ManifestHash, "option": ask.RejectOption,
	})
	if err != nil {
		t.Fatal(err)
	}
	answers := filepath.Join(root, "agents", "grok", "extensions", extensionLayer, "answers")
	if err := os.MkdirAll(answers, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(answers, answerName(testRef, ask.InteractionID)), answer, 0o600); err != nil {
		t.Fatal(err)
	}

	events = waitEvent(t, root, testRef, "completed")
	if !slices.ContainsFunc(events, func(ev bridgeEvent) bool {
		return ev.Event == "interaction_resolved" && ev.Outcome == "answered" && ev.Option == ask.RejectOption
	}) {
		t.Fatalf("no interaction_resolved answered with the reject option; events=%+v", events)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.selected != "reject" {
		t.Fatalf("host selected option %q; want the reject_once option", agent.selected)
	}
}

func TestHostCancelsTheRemoteRequestInFlight(t *testing.T) {
	root := newRoot(t)
	agent := &fakeAgent{holdUntilCancel: true}
	startHost(t, root, agent)
	publishRequest(t, root, testRef, "a long task")
	waitEvent(t, root, testRef, "started")

	cancel, err := json.Marshal(map[string]string{"protocol": BridgeProtocol, "ref": testRef})
	if err != nil {
		t.Fatal(err)
	}
	cancels := filepath.Join(root, "agents", "grok", "extensions", extensionLayer, "cancels")
	if err := os.MkdirAll(cancels, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cancels, testRef+".json"), cancel, 0o600); err != nil {
		t.Fatal(err)
	}

	waitEvent(t, root, testRef, "cancelled")
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !slices.Contains(agent.methods, "session/cancel") {
		t.Fatalf("agent methods=%v; want session/cancel", agent.methods)
	}
}

// at is the liveness record's time, a fixed instant for a test publication.
func (l bridgeLiveness) at() time.Time {
	at, _ := time.Parse(time.RFC3339Nano, l.At)
	return at
}

// writeAnswer plays the remote face: it answers one approval with option.
func writeAnswer(t *testing.T, root, ref string, ask bridgeEvent, option string) {
	t.Helper()
	answer, err := json.Marshal(map[string]string{
		"protocol": BridgeProtocol, "ref": ref, "interaction_id": ask.InteractionID,
		"manifest_hash": ask.ManifestHash, "option": option,
	})
	if err != nil {
		t.Fatal(err)
	}
	answers := filepath.Join(root, "agents", "grok", "extensions", extensionLayer, "answers")
	if err := os.MkdirAll(answers, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(answers, answerName(ref, ask.InteractionID)), answer, 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertNeverRan fails when ref got a receipt or a started event.
func assertNeverRan(t *testing.T, root, ref string) {
	t.Helper()
	dir := filepath.Join(root, "agents", "grok", "extensions", extensionLayer)
	if _, err := os.Stat(filepath.Join(dir, "receipts", ref+".json")); err == nil {
		t.Fatalf("request %s got a receipt; a request refused busy must never run", ref)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "events", ref+".jsonl"))
	if strings.Contains(string(data), `"event":"started"`) {
		t.Fatalf("request %s started; a request refused busy must never run: %s", ref, data)
	}
}

// Review of #1128 round 1 (P1): a request refused busy while another
// request's turn ran was admitted again once that turn ended, and ran.
func TestHostNeverRunsARequestRefusedBusyDuringARemoteTurn(t *testing.T) {
	root := newRoot(t)
	agent := &fakeAgent{askPermission: true}
	startHost(t, root, agent)
	publishRequests(t, root, map[string]string{testRef: "do A", otherRef: "do B"})
	events := waitEvent(t, root, testRef, "interaction")
	waitEvent(t, root, otherRef, "refused")

	writeAnswer(t, root, testRef, events[len(events)-1], events[len(events)-1].RejectOption)
	waitEvent(t, root, testRef, "completed")
	// The serve loop would take B right after A's turn, in the same pass.
	time.Sleep(300 * time.Millisecond)
	assertNeverRan(t, root, otherRef)
}

// Review of #1128 round 1 (P1): a request published during a peer turn
// shorter than the scan interval ran after that turn instead of being
// refused busy.
func TestHostRefusesARequestPublishedDuringAPeerTurn(t *testing.T) {
	root := newRoot(t)
	gate := make(chan struct{})
	agent := &fakeAgent{gate: gate}
	send(t, root, "codex", format.KindQuestion, "what is 2+2?")
	startHost(t, root, agent)
	deadline := time.Now().Add(3 * time.Second)
	for {
		agent.mu.Lock()
		n := len(agent.prompts)
		agent.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the peer turn never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	publishRequest(t, root, testRef, "summarize")
	close(gate)

	waitEvent(t, root, testRef, "refused")
	assertNeverRan(t, root, testRef)
}
