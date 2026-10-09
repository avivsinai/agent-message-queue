package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Server waits of the link's tool bodies (design §6): a call answers within
// 25 s (pending past that), call_get waits up to 30 s.
const (
	linkCallWait = 30 * time.Second
	linkGetWait  = 30 * time.Second
	mcpCallLimit = 10 * time.Minute
)

// toolCallReply is the server's answer about one call. The result is opaque.
type toolCallReply struct {
	CallID    string            `json:"call_id"`
	Status    string            `json:"status"`
	Result    json.RawMessage   `json:"result,omitempty"`
	ReviewURL string            `json:"review_url,omitempty"`
	Error     *linkio.ErrorBody `json:"error,omitempty"`
}

func (r toolCallReply) final() bool { return r.Status != "pending" && r.Status != "pending_approval" }

// request serves link.v1 on the endpoint: it sends one tool body on the
// named link (or the only one) and returns the server's reply body.
func (ls *linkSet) request(ctx context.Context, req ipc.LinkRequest) (json.RawMessage, error) {
	ls.mu.Lock()
	var run *linkRun
	if req.Name != "" {
		run = ls.running[req.Name]
	} else if len(ls.running) == 1 {
		for _, r := range ls.running {
			run = r
		}
	}
	n := len(ls.running)
	ls.mu.Unlock()
	if run == nil {
		if req.Name == "" && n > 1 {
			return nil, protocol.Refuse(protocol.CodeInvalid, "this root has %d links; name one with --link", n)
		}
		return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "no running link %q in this endpoint", req.Name)
	}
	var body any
	switch req.Op {
	case "tools":
		body = map[string]string{"schema": "amq.remote.link.tools/1"}
	case "call":
		args := req.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		body = map[string]any{
			"schema": "amq.remote.link.call/1", "call_id": req.CallID, "tool": req.Tool,
			"arguments": args, "idempotency_key": req.IdempotencyKey,
		}
	case "call_get":
		body = map[string]any{"schema": "amq.remote.link.call_get/1", "call_id": req.CallID, "wait_ms": req.WaitMS}
	default:
		return nil, protocol.Refuse(protocol.CodeInvalid, "unknown link.v1 op %q", req.Op)
	}
	reply, err := run.carrier.Request(ctx, body)
	switch {
	case errors.Is(err, linkio.ErrBusy):
		return nil, protocol.Refuse(protocol.CodeBusy, "%v", err)
	case errors.Is(err, linkio.ErrUnavailable):
		return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "link %s is not connected", run.link.Name)
	case errors.Is(err, context.DeadlineExceeded):
		return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "link %s did not answer in time", run.link.Name)
	}
	return reply, err
}

// linkIPC sends one link.v1 operation to this root's endpoint.
func linkIPC(stateDir string, req ipc.LinkRequest) (json.RawMessage, error) {
	resp, err := ipc.Call(stateDir, ipc.Request{Link: &req})
	if err != nil {
		return nil, err
	}
	if err := resp.AsError(); err != nil {
		return nil, err
	}
	return resp.Reply, nil
}

// callKey is the durable handle of one call: the idempotency key, and the
// call id derived from it, so a CLI that died before printing can read the
// same call again with `link call resume KEY`.
func callKey(given string) (key, callID string) {
	key = given
	if key == "" {
		raw := make([]byte, 10)
		_, _ = rand.Read(raw)
		key = "k_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	}
	return key, "c_" + key
}

// linkCall calls one tool, or resumes a call by its key. With --wait it reads
// the call until its state is final.
func linkCall(args []string, stderr io.Writer, probe *jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("link call", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe)
	argsJSON := fs.String("args", "{}", "the tool's arguments as a JSON object")
	wait := fs.Bool("wait", false, "wait for the call's final state")
	idem := fs.String("idempotency-key", "", "the call's durable key (default: a new one, printed first)")
	linkName := fs.String("link", "", "the link to call (default: the only one)")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) < 1 || len(pos) > 2 || (pos[0] == "resume") != (len(pos) == 2) {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "usage: amq-remote link call TOOL --args JSON [--wait] | link call resume KEY [--wait]")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	var reply toolCallReply
	if pos[0] == "resume" {
		_, callID := callKey(pos[1])
		reply, err = getCall(stateDir, *linkName, callID)
	} else {
		var arguments map[string]any
		if err := json.Unmarshal([]byte(*argsJSON), &arguments); err != nil {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--args must be a JSON object: %v", err)
		}
		key, callID := callKey(*idem)
		say(stderr, "call key %s (resume with: amq-remote link call resume %s)", key, key)
		var raw json.RawMessage
		raw, err = linkIPC(stateDir, ipc.LinkRequest{Name: *linkName, Op: "call", CallID: callID, Tool: pos[0],
			Arguments: json.RawMessage(*argsJSON), IdempotencyKey: key, WaitMS: linkCallWait.Milliseconds()})
		if err == nil {
			err = json.Unmarshal(raw, &reply)
		}
	}
	for err == nil && *wait && !reply.final() {
		reply, err = getCall(stateDir, *linkName, reply.CallID)
	}
	if err != nil {
		return nil, 0, err
	}
	switch reply.Status {
	case "ok", "pending", "pending_approval":
		return reply, 0, nil
	}
	return reply, protocol.ExitError, nil
}

func getCall(stateDir, name, callID string) (toolCallReply, error) {
	var reply toolCallReply
	raw, err := linkIPC(stateDir, ipc.LinkRequest{Name: name, Op: "call_get", CallID: callID, WaitMS: linkGetWait.Milliseconds()})
	if err == nil {
		err = json.Unmarshal(raw, &reply)
	}
	return reply, err
}

// linkTools prints the tools the linked server offers this machine.
func linkTools(args []string, probe *jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("link tools", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe)
	linkName := fs.String("link", "", "the link to ask (default: the only one)")
	if _, err := parseInterleaved(fs, args); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	raw, err := linkIPC(stateDir, ipc.LinkRequest{Name: *linkName, Op: "tools", WaitMS: linkCallWait.Milliseconds()})
	if err != nil {
		return nil, 0, err
	}
	return raw, 0, nil
}

// mcpServe is `amq-remote mcp --link NAME`: a stdio MCP server (newline-
// delimited JSON-RPC 2.0) whose tools are the linked server's tools. It holds
// no token: every call goes through this root's endpoint and its link.
func mcpServe(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	linkName := fs.String("link", "", "the link whose tools to serve (default: the only one)")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return protocol.ExitUsage, err
	}
	m := &mcpServer{stateDir: stateDir, link: *linkName, out: json.NewEncoder(stdout), stderr: stderr}
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64*1024), ipc.LinkRecordBytes)
	for sc.Scan() {
		m.handle(sc.Bytes())
	}
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		return protocol.ExitError, err
	}
	return 0, nil
}

type mcpServer struct {
	stateDir, link string
	out            *json.Encoder
	stderr         io.Writer
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (m *mcpServer) handle(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		m.reply(nil, nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	if len(req.ID) == 0 {
		return // a notification needs no answer
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		m.reply(req.ID, map[string]any{
			"protocolVersion": nonEmpty(p.ProtocolVersion, "2025-06-18"),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "amq-remote-link", "version": version},
		}, nil)
	case "ping":
		m.reply(req.ID, map[string]any{}, nil)
	case "tools/list":
		m.toolsList(req.ID)
	case "tools/call":
		m.toolsCall(req.ID, req.Params)
	default:
		m.reply(req.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

func (m *mcpServer) reply(id json.RawMessage, result any, e *rpcError) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		msg["id"] = nil
	}
	if e != nil {
		msg["error"] = e
	} else {
		msg["result"] = result
	}
	_ = m.out.Encode(msg)
}

func (m *mcpServer) toolsList(id json.RawMessage) {
	raw, err := linkIPC(m.stateDir, ipc.LinkRequest{Name: m.link, Op: "tools", WaitMS: linkCallWait.Milliseconds()})
	var cat struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &cat)
	}
	if err != nil {
		m.reply(id, nil, &rpcError{Code: -32603, Message: err.Error()})
		return
	}
	tools := make([]map[string]any, 0, len(cat.Tools))
	for _, t := range cat.Tools {
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
	}
	m.reply(id, map[string]any{"tools": tools}, nil)
}

// toolsCall runs one call to its final state, up to mcpCallLimit; a call
// still waiting after that (a write the owner has not decided) returns its
// key so the agent can resume it.
func (m *mcpServer) toolsCall(id json.RawMessage, params json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		m.reply(id, nil, &rpcError{Code: -32602, Message: "tools/call needs a name"})
		return
	}
	key, callID := callKey("")
	var reply toolCallReply
	raw, err := linkIPC(m.stateDir, ipc.LinkRequest{Name: m.link, Op: "call", CallID: callID, Tool: p.Name,
		Arguments: p.Arguments, IdempotencyKey: key, WaitMS: linkCallWait.Milliseconds()})
	if err == nil {
		err = json.Unmarshal(raw, &reply)
	}
	deadline := time.Now().Add(mcpCallLimit)
	for err == nil && !reply.final() && time.Now().Before(deadline) {
		reply, err = getCall(m.stateDir, m.link, reply.CallID)
	}
	if err != nil {
		m.reply(id, mcpText(err.Error(), true), nil)
		return
	}
	switch {
	case reply.Status == "ok":
		m.reply(id, mcpText(string(reply.Result), false), nil)
	case !reply.final():
		m.reply(id, mcpText(fmt.Sprintf("Still %s. Resume with: amq-remote link call resume %s --wait", reply.Status, key), true), nil)
	case reply.Error != nil:
		m.reply(id, mcpText(fmt.Sprintf("%s: %s", reply.Error.Code, reply.Error.Message), true), nil)
	default:
		m.reply(id, mcpText("The call ended "+reply.Status, true), nil)
	}
}

func mcpText(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isError}
}
