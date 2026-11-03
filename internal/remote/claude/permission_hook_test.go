package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// boundHookEnv binds the AMQ handle the hook's environment names and
// returns the binding's dialogs directory.
func boundHookEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(binding.EnvPath, filepath.Join(t.TempDir(), "binding.json"))
	root := t.TempDir()
	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_ME", "agent")
	if err := binding.WriteNamed(binding.Binding{Name: "proj", Carrier: binding.CarrierMailbox, Root: root, Handle: "agent", BoundAt: "2026-11-02T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	dir, err := dialog.DirFor("proj")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// runHook runs the hook on stdin in the background; the returned function
// waits for it and returns what it printed.
func runHook(t *testing.T, stdin string) func() string {
	t.Helper()
	h := permissionHook{now: time.Now, wait: time.Minute, poll: 5 * time.Millisecond}
	var out bytes.Buffer
	ended := make(chan struct{})
	go func() {
		h.run(strings.NewReader(stdin), &out, make(chan struct{}))
		close(ended)
	}()
	return func() string {
		select {
		case <-ended:
		case <-time.After(3 * time.Second):
			t.Fatal("the hook did not end")
		}
		return out.String()
	}
}

// openRequest waits for the hook's one open request.
func openRequest(t *testing.T, dir string) dialog.Request {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if open, err := dialog.Requests(dir); err == nil && len(open) == 1 {
			return open[0]
		}
	}
	t.Fatal("the hook opened no request")
	return dialog.Request{}
}

func decisionOf(t *testing.T, out string) map[string]any {
	t.Helper()
	var got struct {
		HookSpecificOutput struct {
			HookEventName string         `json:"hookEventName"`
			Decision      map[string]any `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.HookSpecificOutput.HookEventName != "PermissionRequest" {
		t.Fatalf("hook output %q is not a PermissionRequest decision: %v", out, err)
	}
	return got.HookSpecificOutput.Decision
}

func TestPermissionHookAppliesBuzzOption(t *testing.T) {
	dir := boundHookEnv(t)
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, _ := json.Marshal(map[string]any{
		"session_id": "s-1", "transcript_path": transcript, "hook_event_name": "PermissionRequest",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf node_modules", "description": "Remove node_modules"},
		"permission_suggestions": []any{map[string]any{"type": "addRules", "behavior": "allow", "destination": "localSettings",
			"rules": []any{map[string]any{"toolName": "Bash", "ruleContent": "rm -rf node_modules"}}}},
	})

	// The owner answers 2 in Buzz: allow, with the suggestion kept to this session.
	wait := runHook(t, string(stdin))
	req := openRequest(t, dir)
	if req.Text != "$ rm -rf node_modules\n(Remove node_modules)" || len(req.Options) != 3 || !strings.HasPrefix(req.Options[1].Label, "Allow this session") || !req.Options[2].Deny {
		t.Fatalf("request = %+v", req)
	}
	if _, err := dialog.WriteAnswer(dir, dialog.Answer{ID: req.ID, Option: 2, By: dialog.ByBuzz}); err != nil {
		t.Fatal(err)
	}
	d := decisionOf(t, wait())
	perms, _ := d["updatedPermissions"].([]any)
	if d["behavior"] != "allow" || len(perms) != 1 || perms[0].(map[string]any)["destination"] != "session" || perms[0].(map[string]any)["type"] != "addRules" {
		t.Fatalf("decision = %v, want allow with the suggestion scoped to the session", d)
	}
	if r, ok, _ := dialog.ReadResolved(dir, req.ID); !ok || r.Outcome != dialog.OutcomeAllowed || r.By != dialog.ByBuzz {
		t.Fatalf("resolved = %+v, %v", r, ok)
	}

	// The terminal approves the next call: its tool_result alone closes the
	// dialog, and the hook prints nothing.
	wait = runHook(t, string(stdin))
	req = openRequest(t, dir)
	lines := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"description":"Remove node_modules","command":"rm -rf node_modules"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := wait(); out != "" {
		t.Fatalf("a terminal answer printed %q", out)
	}
	if r, ok, _ := dialog.ReadResolved(dir, req.ID); !ok || r.Outcome != dialog.OutcomeTerminal || r.By != dialog.ByTerminal {
		t.Fatalf("resolved = %+v, %v; want answered in the terminal", r, ok)
	}
}

// A Claude session with no AM_* (the desktop app, an IDE) reaches its own
// channel when a native binding names its session id, and the machine's
// dialogs otherwise, labelled with its working directory.
func TestPermissionHookRoutesUnboundSessionsToTheMachine(t *testing.T) {
	boundHookEnv(t)
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_ME", "")
	if err := binding.WriteNamed(binding.Binding{Name: "native", Root: t.TempDir(), Target: "claude:native", NativeSession: "s-native", BoundAt: "2026-11-02T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	for session, want := range map[string]string{"s-native": "native", "s-desktop": dialog.MachineBinding} {
		dir, err := dialog.DirFor(want)
		if err != nil {
			t.Fatal(err)
		}
		wait := runHook(t, `{"session_id":"`+session+`","cwd":"/work/myproj","tool_name":"WebFetch","tool_input":{"url":"https://example.com","prompt":"summarize"}}`)
		req := openRequest(t, dir)
		if req.Session != session || req.Label != "claude · myproj" || req.Text != "https://example.com\nsummarize" {
			t.Fatalf("%s request in %s = %+v", session, want, req)
		}
		if _, err := dialog.WriteAnswer(dir, dialog.Answer{ID: req.ID, Option: len(req.Options), By: dialog.ByBuzz}); err != nil {
			t.Fatal(err)
		}
		if d := decisionOf(t, wait()); d["behavior"] != "deny" || d["message"] != "Denied from Buzz" {
			t.Fatalf("decision = %v, want deny", d)
		}
	}
}
