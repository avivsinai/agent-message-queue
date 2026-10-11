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
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/linkio"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Server waits of the link's tool bodies (design §6): a call answers within
// 25 s (pending past that), call_get waits up to 30 s.
const (
	linkCallWait = 30 * time.Second
	linkGetWait  = 30 * time.Second
	mcpCallLimit = 10 * time.Minute
	// minPoll is the least time between two reads of one call, so a server
	// that answers pending at once is never spun on.
	minPoll = time.Second
)

// idempotencyKeyRe keeps the wire call id ("c_" + key) inside the contract's
// opaque id grammar.
var idempotencyKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,126}$`)

// mcpVersions are the MCP protocol versions this server speaks, newest last.
var mcpVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18"}

// toolCallReply is the server's answer about one call. The result is opaque.
type toolCallReply struct {
	CallID    string            `json:"call_id"`
	Status    string            `json:"status"`
	Result    json.RawMessage   `json:"result,omitempty"`
	ReviewURL string            `json:"review_url,omitempty"`
	Error     *linkio.ErrorBody `json:"error,omitempty"`
}

// busy is the server asking to try again later; the call is not over.
func (r toolCallReply) busy() bool {
	return r.Error != nil && r.Error.Code == string(protocol.CodeBusy)
}

// final reports a call whose state no longer changes.
func (r toolCallReply) final() bool {
	return !r.busy() && r.Status != "pending" && r.Status != "pending_approval"
}

// request serves link.v1 on the endpoint: it sends one tool body on the
// named link (or the only one) and returns the server's reply body.
func (ls *linkSet) request(ctx context.Context, req ipc.LinkRequest) (json.RawMessage, error) {
	if req.Op == "page" || req.Op == "page_result" {
		return ls.pageRequest(req)
	}
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
		switch {
		case req.Name == "" && n > 1:
			return nil, protocol.Refuse(protocol.CodeInvalid, "this root has %d links; name one with --link", n)
		case req.Name == "":
			return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "this endpoint has no running link")
		}
		return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "no running link %q in this endpoint", req.Name)
	}
	switch req.Op {
	case "held":
		return json.Marshal(run.carrier.HeldTasks())
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
		wait := min(max(req.WaitMS, 0), linkGetWait.Milliseconds()) // the contract's 0..30000
		body = map[string]any{"schema": "amq.remote.link.call_get/1", "call_id": req.CallID, "wait_ms": wait}
	default:
		return nil, protocol.Refuse(protocol.CodeInvalid, "unknown link.v1 op %q", req.Op)
	}
	reply, err := run.carrier.Request(ctx, body)
	switch {
	case errors.Is(err, linkio.ErrBusy):
		return nil, protocol.Refuse(protocol.CodeBusy, "%v", err)
	case errors.Is(err, linkio.ErrTooLarge):
		return nil, protocol.Refuse(protocol.CodeInvalid, "the request is too large for link %s: %v", run.link.Name, err)
	case errors.Is(err, linkio.ErrUnavailable):
		return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "link %s is not connected", run.link.Name)
	case errors.Is(err, linkio.ErrDropped):
		return nil, protocol.Refuse(protocol.CodeEndpointUnreachable, "link %s dropped before the answer; the call may have run, read it again with its key", run.link.Name)
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
// wire call id derived from it ("c_" + key), a client-scoped alias the server
// echoes and answers call_get for (ruling v). A CLI that died before printing
// reads the same call again with `link call resume KEY`.
func callKey(given string) (key, callID string) {
	key = given
	if key == "" {
		raw := make([]byte, 10)
		_, _ = rand.Read(raw)
		key = "k_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	}
	return key, "c_" + key
}

// caller reads one call over this root's endpoint.
type caller struct {
	stateDir, link string
}

// send runs one link.v1 call or call_get. Busy, from the server or from the
// link's own budget, is never an answer: it waits at least minPoll (or the
// server's retry_after_ms) and sends again, until ctx ends.
func (cl caller) send(ctx context.Context, req ipc.LinkRequest) (toolCallReply, error) {
	for {
		var reply toolCallReply
		raw, err := linkIPC(cl.stateDir, req)
		if err == nil {
			err = json.Unmarshal(raw, &reply)
		}
		wait := minPoll
		switch {
		case err != nil && protocol.RefusalCode(err) == protocol.CodeBusy:
		case err != nil:
			return reply, err
		case reply.busy():
			wait = max(wait, time.Duration(reply.Error.RetryAfterMS)*time.Millisecond)
		default:
			return reply, nil
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return reply, protocol.Refuse(protocol.CodeBusy, "the link stayed busy; read the call again later with its key")
		}
	}
}

// follow reads a call until it is final, or, with untilApproval, until it
// waits for the owner's decision. Reads are at least minPoll apart.
func (cl caller) follow(ctx context.Context, reply toolCallReply, untilApproval bool) (toolCallReply, error) {
	for !reply.final() && (!untilApproval || reply.Status != "pending_approval") {
		if err := sleepCtx(ctx, minPoll); err != nil {
			return reply, nil // the caller's deadline: report the last state
		}
		next, err := cl.send(ctx, ipc.LinkRequest{Name: cl.link, Op: "call_get", CallID: reply.CallID, WaitMS: linkGetWait.Milliseconds()})
		if err != nil {
			return reply, err
		}
		reply = next
	}
	return reply, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
	given := *idem
	if pos[0] == "resume" {
		given = pos[1]
	}
	if given != "" && !idempotencyKeyRe.MatchString(given) {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "the key must be 1-126 of A-Z a-z 0-9 _ . : -")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	cl := caller{stateDir: stateDir, link: *linkName}
	ctx, cancel := context.WithTimeout(context.Background(), linkCallWait)
	if *wait {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()
	key, callID := callKey(given)
	var reply toolCallReply
	if pos[0] == "resume" {
		reply, err = cl.send(ctx, ipc.LinkRequest{Name: *linkName, Op: "call_get", CallID: callID, WaitMS: linkGetWait.Milliseconds()})
	} else {
		var arguments map[string]any
		if err := json.Unmarshal([]byte(*argsJSON), &arguments); err != nil {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--args must be a JSON object: %v", err)
		}
		say(stderr, "call key %s (resume with: amq-remote link call resume %s)", key, key)
		reply, err = cl.send(ctx, ipc.LinkRequest{Name: *linkName, Op: "call", CallID: callID, Tool: pos[0],
			Arguments: json.RawMessage(*argsJSON), IdempotencyKey: key, WaitMS: linkCallWait.Milliseconds()})
	}
	if err == nil && *wait {
		reply, err = cl.follow(ctx, reply, false)
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
// no token: every call goes through this root's endpoint and its link. Each
// request runs on its own goroutine, so a long call never holds up a ping,
// and notifications/cancelled stops that call's wait.
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
	m := &mcpServer{cl: caller{stateDir: stateDir, link: *linkName}, out: json.NewEncoder(stdout), stderr: stderr,
		cancels: map[string]context.CancelFunc{}}
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64*1024), ipc.LinkRecordBytes)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.handle(line)
		}()
	}
	m.wg.Wait()
	if err := sc.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		return protocol.ExitError, err
	}
	return 0, nil
}

type mcpServer struct {
	cl     caller
	stderr io.Writer
	wg     sync.WaitGroup

	mu      sync.Mutex // guards out and cancels
	out     *json.Encoder
	cancels map[string]context.CancelFunc // request id -> its call's cancel
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
		if req.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			_ = json.Unmarshal(req.Params, &p)
			m.mu.Lock()
			if cancel := m.cancels[string(p.RequestID)]; cancel != nil {
				cancel()
			}
			m.mu.Unlock()
		}
		return // a notification needs no answer
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := mcpVersions[len(mcpVersions)-1]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		m.reply(req.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "amq-remote-link", "version": version},
		}, nil)
	case "ping":
		m.reply(req.ID, map[string]any{}, nil)
	case "tools/list":
		m.toolsList(req.ID)
	case "tools/call":
		ctx, cancel := context.WithTimeout(context.Background(), mcpCallLimit)
		m.mu.Lock()
		m.cancels[string(req.ID)] = cancel
		m.mu.Unlock()
		m.toolsCall(ctx, req.ID, req.Params)
		m.mu.Lock()
		delete(m.cancels, string(req.ID))
		m.mu.Unlock()
		cancel()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.out.Encode(msg)
}

func (m *mcpServer) toolsList(id json.RawMessage) {
	raw, err := linkIPC(m.cl.stateDir, ipc.LinkRequest{Name: m.cl.link, Op: "tools", WaitMS: linkCallWait.Milliseconds()})
	var cat struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Write       bool            `json:"write"`
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
		tools = append(tools, map[string]any{
			"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
			"annotations": map[string]any{"readOnlyHint": !t.Write},
		})
	}
	m.reply(id, map[string]any{"tools": tools}, nil)
}

// toolsCall runs one call. A read is followed to its final state, up to
// mcpCallLimit. A write that waits for the owner returns at once with where
// to decide and how to resume: the owner decides, not the agent.
func (m *mcpServer) toolsCall(ctx context.Context, id json.RawMessage, params json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		m.reply(id, nil, &rpcError{Code: -32602, Message: "tools/call needs a name"})
		return
	}
	key, callID := callKey("")
	reply, err := m.cl.send(ctx, ipc.LinkRequest{Name: m.cl.link, Op: "call", CallID: callID, Tool: p.Name,
		Arguments: p.Arguments, IdempotencyKey: key, WaitMS: linkCallWait.Milliseconds()})
	if err == nil {
		reply, err = m.cl.follow(ctx, reply, true)
	}
	resume := "amq-remote link call resume " + key + " --wait"
	switch {
	case err != nil:
		m.reply(id, mcpText(fmt.Sprintf("%v. Resume with: %s", err, resume), true), nil)
	case reply.Status == "ok":
		m.reply(id, mcpText(string(reply.Result), false), nil)
	case reply.Status == "pending_approval":
		m.reply(id, mcpText(fmt.Sprintf("Waiting for the owner's decision at %s. Resume with: %s", reply.ReviewURL, resume), false), nil)
	case !reply.final():
		m.reply(id, mcpText(fmt.Sprintf("Still %s. Resume with: %s", reply.Status, resume), true), nil)
	case reply.Error != nil:
		m.reply(id, mcpText(fmt.Sprintf("%s: %s", reply.Error.Code, reply.Error.Message), true), nil)
	default:
		m.reply(id, mcpText("The call ended "+reply.Status, true), nil)
	}
}

func mcpText(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isError}
}

// pageRequest serves the local page operations. A register page needs only
// the link's state; a confirm page needs the link's running carrier.
func (ls *linkSet) pageRequest(req ipc.LinkRequest) (json.RawMessage, error) {
	if req.Op == "page_result" {
		return ls.localPageRequest(nil, "", req)
	}
	name := req.Name
	if name == "" {
		mf, err := manifest.Load(ls.manifestFile)
		if err != nil {
			return nil, err
		}
		if len(mf.Links) != 1 {
			return nil, protocol.Refuse(protocol.CodeInvalid, "this root has %d links; name one with --link", len(mf.Links))
		}
		name = mf.Links[0].Name
	}
	if _, err := linkio.LoadDeviceKey(ls.stateDir, name); err != nil {
		return nil, protocol.Refuse(protocol.CodeNotFound, "no link named %q in this root", name)
	}
	ls.mu.Lock()
	run := ls.running[name]
	ls.mu.Unlock()
	return ls.localPageRequest(run, name, req)
}
