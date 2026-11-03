package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/dialog"
)

// boundHookEnv binds the AMQ handle the hook's environment names and
// returns the binding's dialogs directory.
func boundHookEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	// The binding store refuses a symlinked path, and the macOS temp dir is
	// under the /var symlink (macOS CI failed here).
	store, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(binding.EnvPath, filepath.Join(store, "binding.json"))
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
	if req.Text != "$ rm -rf node_modules\n(Remove node_modules)" || len(req.Options) != 3 || req.Options[1].Label != "Allow this session: Bash(rm -rf node_modules)" || !req.Options[2].Deny {
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

// After /clear the Claude process that attach bound has a new session id: the
// hook finds the binding by that process's whole identity and opens the
// dialog there, leaving the binding as attach wrote it.
func TestPermissionHookUsesTheBindingOfItsProcessAfterASessionChange(t *testing.T) {
	boundHookEnv(t)
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_ME", "")
	ancestors := launch.Ancestors()
	if len(ancestors) == 0 {
		t.Skip("no process ancestry on this platform")
	}
	config, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	raw, _ := json.Marshal(map[string]any{"pid": ancestors[0].PID, "sessionId": "s-new", "cwd": "/work/myproj"})
	if err := os.MkdirAll(filepath.Join(config, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "sessions", strconv.Itoa(ancestors[0].PID)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	attached := binding.Binding{Name: "claude-myproj", Root: t.TempDir(), Target: "claude:" + strconv.Itoa(ancestors[0].PID),
		NativeSession: "s-old", Holder: ancestors[0], BoundAt: "2026-11-03T00:00:00Z"}
	if err := binding.WriteNamed(attached); err != nil {
		t.Fatal(err)
	}
	dir, err := dialog.DirFor("claude-myproj")
	if err != nil {
		t.Fatal(err)
	}
	wait := runHook(t, `{"session_id":"s-new","tool_name":"WebFetch","tool_input":{"url":"https://example.com"}}`)
	req := openRequest(t, dir)
	if _, err := dialog.WriteAnswer(dir, dialog.Answer{ID: req.ID, Option: 1, By: dialog.ByBuzz}); err != nil {
		t.Fatal(err)
	}
	if d := decisionOf(t, wait()); d["behavior"] != "allow" {
		t.Fatalf("decision = %v, want allow", d)
	}
	if b, err := binding.ReadNamed("claude-myproj"); err != nil || b.NativeSession != "s-old" || b.Holder != attached.Holder {
		t.Fatalf("binding = %+v, %v; want it as attach wrote it", b, err)
	}
}

// "Allow this session" names its scope with each path as Claude gives it,
// the home directory as ~ where a path starts with it.
func TestSessionScopeLabels(t *testing.T) {
	t.Setenv("HOME", "/Users/me")
	for raw, want := range map[string]string{
		`{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"rm -rf /Users/me/other/node_modules"}]}`: "Allow this session: Bash(rm -rf ~/other/node_modules)",
		`{"type":"addRules","rules":[{"toolName":"Read","ruleContent":"//Users/me/notes/**"}]}`:                 "Allow this session: Read(~/notes/**)",
		`{"type":"addDirectories","directories":["/Users/me/proj"]}`:                                            "Allow edits in ~/proj this session",
		`{"type":"removeDirectories","directories":["/Volumes/x/Users/me/p"]}`:                                  "Stop allowing edits in /Volumes/x/Users/me/p this session",
		`{"type":"addDirectories","directories":["/Users/me/Users/me/x"]}`:                                      "Allow edits in ~/Users/me/x this session",
		`{"type":"setMode","mode":"acceptEdits"}`:                                                               "Accept edits this session",
	} {
		if got := sessionScope([]json.RawMessage{json.RawMessage(raw)}); got != want {
			t.Fatalf("sessionScope(%s) = %q, want %q", raw, got, want)
		}
	}
}
