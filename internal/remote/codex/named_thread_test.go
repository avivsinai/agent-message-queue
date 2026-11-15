package codex

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Regression: agent-message-queue-4ip, a Codex TUI on the shared
// app-server daemon never got the session name. coop exec now creates the
// thread on the daemon, names it, persists it, and resumes it.
func TestStartNamedThreadCreatesNamesAndPersistsTheThread(t *testing.T) {
	for _, name := range []string{"session1/codex", ""} {
		t.Run(name, func(t *testing.T) {
			d := newFakeNamingDaemon(t)
			id, err := StartThread(context.Background(), d.sock, "/work", name, "0.167.0", ThreadSettings{})
			if err != nil {
				t.Fatal(err)
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if id != "t1" || d.cwd != "/work" || d.name != name || !d.persisted {
				t.Fatalf("id=%q cwd=%q name=%q persisted=%v", id, d.cwd, d.name, d.persisted)
			}
		})
	}
}

// Regression: agent-message-queue-611.63, naming that
// failed after thread/start left a named thread nobody resumes. The thread
// is archived when a later step fails.
func TestStartNamedThreadArchivesAThreadItCouldNotFinish(t *testing.T) {
	d := newFakeNamingDaemon(t)
	d.failInject = true
	if _, err := StartThread(context.Background(), d.sock, "/work", "session1/codex", "", ThreadSettings{}); err == nil {
		t.Fatal("naming succeeded although persisting the thread failed")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.archived {
		t.Fatal("the half-created thread was not archived")
	}
}

// A thread started with a seat's settings gets them in the daemon request
// and is archived when the daemon does not actually hold them.
func TestStartThreadSendsTheSettingsAndRefusesAnAnswerWithoutThem(t *testing.T) {
	settings := ThreadSettings{Sandbox: "read-only", ApprovalPolicy: "on-request", ApprovalsReviewer: "user", ReasoningEffort: "low"}
	d := newFakeNamingDaemon(t)
	id, err := StartThread(context.Background(), d.sock, "/work", "", "0.167.0", settings)
	if err != nil || id != "t1" {
		t.Fatalf("StartThread = %q, %v", id, err)
	}
	d.mu.Lock()
	var start fakeCall
	for _, call := range d.calls {
		if call.method == "thread/start" {
			start = call
		}
	}
	d.mu.Unlock()
	var got map[string]any
	if err := json.Unmarshal(start.params, &got); err != nil || got["sandbox"] != "read-only" || got["approvalPolicy"] != "on-request" || got["approvalsReviewer"] != "user" {
		t.Fatalf("thread/start params = %s, %v", start.params, err)
	}
	config, _ := got["config"].(map[string]any)
	if config["model_reasoning_effort"] != "low" {
		t.Fatalf("thread/start params = %s; want low effort", start.params)
	}
	d = newFakeNamingDaemon(t)
	d.mismatchSandbox = true
	_, err = StartThread(context.Background(), d.sock, "/work", "", "0.167.0", settings)
	if err == nil || !strings.Contains(err.Error(), "does not hold the seat's pins (sandbox: want read-only, daemon has workspaceWrite)") {
		t.Fatalf("mismatch error = %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.archived {
		t.Fatal("the refused thread was not archived")
	}
}

// Pinning on a cold resume sets the thread's settings durably and leaves
// it with no client subscribed before the TUI attaches.
func TestPinThreadResumesWithTheSettingsAndUnsubscribes(t *testing.T) {
	d := newFakeNamingDaemon(t)
	settings := ThreadSettings{Sandbox: "workspace-write", ApprovalPolicy: "on-request", ApprovalsReviewer: "user", ReasoningEffort: "low"}
	if err := PinThread(context.Background(), d.sock, "thread-1", "0.167.0", settings); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var resumed, unsubscribed bool
	for _, call := range d.calls {
		switch call.method {
		case "thread/resume":
			var p map[string]any
			_ = json.Unmarshal(call.params, &p)
			resumed = p["threadId"] == "thread-1" && p["excludeTurns"] == true && p["sandbox"] == "workspace-write" && p["approvalPolicy"] == "on-request" && p["approvalsReviewer"] == "user"
			cfg, _ := p["config"].(map[string]any)
			resumed = resumed && cfg["model_reasoning_effort"] == "low"
		case "thread/unsubscribe":
			unsubscribed = resumed
		}
	}
	if !resumed || !unsubscribed {
		t.Fatalf("calls = %+v; want pinned thread/resume then thread/unsubscribe", d.calls)
	}
}

// fakeNamingDaemon serves app-server connections that start, name, and
// persist one thread, and list and read threads for a resume selection.
type fakeNamingDaemon struct {
	sock      string
	mu        sync.Mutex
	cwd, name string
	persisted bool
	// failInject makes thread/inject_items fail; archived records a
	// thread/archive of the started thread.
	// archived records a refused thread; mismatchSandbox reports workspaceWrite.
	failInject, archived bool
	mismatchSandbox      bool
	// pages answer thread/list in turn; calls records every request.
	pages []string
	calls []fakeCall
}

type fakeCall struct {
	method string
	params json.RawMessage
}

func newFakeNamingDaemon(t *testing.T) *fakeNamingDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "amqcxn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := &fakeNamingDaemon{sock: filepath.Join(dir, "d.sock")}
	l, err := net.Listen("unix", d.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go d.serve(conn)
		}
	}()
	return d
}

func (d *fakeNamingDaemon) serve(conn net.Conn) {
	ws, err := acceptServerWS(conn)
	if err != nil {
		return
	}
	for {
		payload, err := ws.readText()
		if err != nil {
			return
		}
		var msg rpcMessage
		if json.Unmarshal(payload, &msg) != nil || msg.ID == nil {
			continue
		}
		var p struct {
			ThreadID          string            `json:"threadId"`
			Cwd               string            `json:"cwd"`
			Name              string            `json:"name"`
			Items             []json.RawMessage `json:"items"`
			ApprovalsReviewer string            `json:"approvalsReviewer"`
			ApprovalPolicy    string            `json:"approvalPolicy"`
			Sandbox           string            `json:"sandbox"`
			Config            map[string]any    `json:"config"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		var result any = map[string]any{}
		d.mu.Lock()
		d.calls = append(d.calls, fakeCall{msg.Method, msg.Params})
		switch msg.Method {
		case "initialize":
			result = map[string]any{"userAgent": "codex_cli_rs/0.167.0 (Mac OS; arm64)"}
		case "config/read":
			result = map[string]any{"config": map[string]any{"model_provider": nil}, "origins": map[string]any{}, "layers": []any{}}
		case "thread/list":
			result = json.RawMessage(`{"data":[],"nextCursor":null}`)
			if len(d.pages) > 0 {
				result, d.pages = json.RawMessage(d.pages[0]), d.pages[1:]
			}
		case "thread/read":
			// A daemon has not loaded a thread no client opened.
			d.mu.Unlock()
			_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"error":{"code":-32600,"message":"thread not loaded: ` + p.ThreadID + `"}}`))
			continue
		case "thread/start":
			d.cwd = p.Cwd
			sandboxType := sandboxTypes[p.Sandbox]
			if d.mismatchSandbox {
				sandboxType = "workspaceWrite"
			}
			result = map[string]any{"thread": map[string]any{"id": "t1"}, "sandbox": map[string]any{"type": sandboxType},
				"approvalPolicy": p.ApprovalPolicy, "approvalsReviewer": p.ApprovalsReviewer,
				"reasoningEffort": p.Config["model_reasoning_effort"]}
		case "thread/resume":
			result = map[string]any{"thread": map[string]any{"id": p.ThreadID}, "sandbox": map[string]any{"type": sandboxTypes[p.Sandbox]},
				"approvalPolicy": p.ApprovalPolicy, "approvalsReviewer": p.ApprovalsReviewer,
				"reasoningEffort": p.Config["model_reasoning_effort"]}
		case "thread/name/set":
			if p.ThreadID == "t1" {
				d.name = p.Name
			}
		case "thread/inject_items":
			if d.failInject {
				d.mu.Unlock()
				_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"error":{"code":-32603,"message":"busy"}}`))
				continue
			}
			d.persisted = p.ThreadID == "t1" && len(p.Items) == 1
		case "thread/archive":
			d.archived = p.ThreadID == "t1"
		}
		raw, _ := json.Marshal(result)
		d.mu.Unlock()
		_ = ws.writeText([]byte(`{"jsonrpc":"2.0","id":` + string(*msg.ID) + `,"result":` + string(raw) + `}`))
	}
}
