//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// The Cursor approval guard, happy path: the installer writes a hooks file
// Cursor loads (version 1, one entry per event) and doctor reports it
// installed; the receiver opens the marker from the preToolUse Cursor
// 2026.10.22 sent before "Run this command?", and the turn's stop removes it.
func TestCursorDialogHookHoldsStartupAndTheTurn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runWakeInstallDialogHook([]string{"--harness", "cursor"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Version int                                   `json:"version"`
		Hooks   map[string][]struct{ Command string } `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("hooks file: %v", err)
	}
	if file.Version != 1 {
		t.Errorf("version = %d, want 1", file.Version)
	}
	for _, event := range cursorDialogHookEvents {
		if len(file.Hooks[event]) != 1 || !strings.Contains(file.Hooks[event][0].Command, " wake dialog --harness cursor ") {
			t.Errorf("%s entries = %+v", event, file.Hooks[event])
		}
	}
	if check, ok := checkCursorDialogHook(); !ok || check.Status != "ok" {
		t.Errorf("doctor = %+v, %v", check, ok)
	}

	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(fsq.AgentBase(root, "cur"), 0o700); err != nil {
		t.Fatal(err)
	}
	clearSendMailboxTestEnv(t)
	t.Setenv(envMe, "cur")
	t.Setenv(envRoot, root)
	marker := filepath.Join(fsq.AgentBase(root, "cur"), wakeDialogFileName)
	hook := func(verb, input string) {
		t.Helper()
		if err := runWakeDialog([]string{"--harness", "cursor", verb}, strings.NewReader(input)); err != nil {
			t.Fatalf("wake dialog %s: %v", verb, err)
		}
	}
	held := func() bool {
		_, err := os.Lstat(marker)
		return err == nil
	}
	// PR #1233 review: Cursor can show trust, MCP and sandbox prompts
	// before sessionStart; resume omits that hook and needs a human turn.
	for _, ready := range []string{"sessionStart", "beforeSubmitPrompt"} {
		if err := syncStartupDialogMarker(root, "cur", "/opt/bin/cursor-agent", nil); err != nil {
			t.Fatal(err)
		}
		if !held() {
			t.Fatal("Cursor launch wrote no startup hold")
		}
		hook("close", `{}`)
		if !held() {
			t.Fatal("unknown input released startup")
		}
		hook("close", `{"hook_event_name":"`+ready+`","session_id":"ready"}`)
		if held() {
			t.Fatalf("%s kept startup hold", ready)
		}
	}
	hook("open", `{"conversation_id":"3b3d8b96-c4e1-4179-8039-66951ada578c","generation_id":"7f8505fe-c99f-4299-94c8-4dd6f89a3414",`+
		`"tool_name":"Shell","tool_input":{"command":"touch guarded.txt","cwd":"","timeout":30000},"tool_use_id":"df9fada2-3518-48d3-9e09-da6f21911f48",`+
		`"cwd":"","session_id":"3b3d8b96-c4e1-4179-8039-66951ada578c","hook_event_name":"preToolUse"}`)
	if !held() {
		t.Fatal("preToolUse wrote no marker")
	}
	// PR #1233 review: a submitted follow-up is queued before the active
	// turn ends, while an abort/error stop can arrive after the next turn.
	for _, event := range []string{"beforeSubmitPrompt", "UserPromptSubmit"} {
		if err := runWakeDialog([]string{"close"}, strings.NewReader(`{"hook_event_name":"`+event+`"}`)); err != nil {
			t.Fatal(err)
		}
		if !held() {
			t.Fatalf("%s released an active approval", event)
		}
	}
	for _, generation := range []string{"old-generation", ""} {
		hook("close", `{"hook_event_name":"stop","status":"aborted","generation_id":"`+generation+`"}`)
		if !held() {
			t.Fatal("unmatched background stop released approval")
		}
	}
	hook("close", `{"conversation_id":"3b3d8b96-c4e1-4179-8039-66951ada578c","status":"completed","loop_count":0,`+
		`"session_id":"3b3d8b96-c4e1-4179-8039-66951ada578c","hook_event_name":"stop"}`)
	if held() {
		t.Fatal("the turn's stop kept the hold")
	}
}

// Regression: observed in the vd4.7 live run on Cursor 2026.10.22. Cursor
// deletes "//" to the end of a line and "/*" through the next "*/" before it
// parses a hooks file, inside strings too. The close gate holds "*/./*", so
// the unescaped file reached Cursor spliced: the stop event ran another
// event's command and beforeSubmitPrompt and sessionStart ran none.
func TestCursorDialogHookFileSurvivesCursorCommentStripping(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Dir(exe) + "//" + filepath.Base(exe)
	if err := installCursorDialogHook(bin, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []string{"//", "/*", "*/"} {
		if bytes.Contains(raw, []byte(seq)) {
			t.Errorf("the Cursor hooks file holds %q, which Cursor strips as a comment", seq)
		}
	}
	if check, _ := checkCursorDialogHook(); check.Status != "ok" {
		t.Fatalf("doctor = %+v", check)
	}
	// PR #1233 review: decoded equality hid old, unescaped bytes from
	// reinstall, even though Cursor corrupted them during comment stripping.
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	old, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if check, _ := checkCursorDialogHook(); check.Status != "warn" {
		t.Fatalf("unsafe old bytes accepted: %+v", check)
	}
	if err := installCursorDialogHook(bin, false); err != nil {
		t.Fatal(err)
	}
	if check, _ := checkCursorDialogHook(); check.Status != "ok" {
		t.Fatalf("reinstall did not repair bytes: %+v", check)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["version"] = json.RawMessage(`"1"`)
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if check, _ := checkCursorDialogHook(); check.Status != "warn" || !strings.Contains(check.Message, "numeric positive integer") {
		t.Fatalf("string version accepted: %+v", check)
	}
}

// Regression: observed in the vd4.7 live run on Cursor 2026.10.22. Cursor
// ran each AMQ hook and then failed it with empty_stdout, because it
// requires JSON on stdout. Every command must print exactly {}, on the
// gated path without AM_ME and on the path that runs amq.
func TestCursorDialogHookCommandsPrintJSON(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(fsq.AgentBase(root, "cur"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "missing-amq")
	path := "PATH=" + os.Getenv("PATH")
	envs := [][]string{{path}, {path, envMe + "=cur", envRoot + "=" + root}}
	for _, hook := range cursorDialogHooks(bin) {
		for _, env := range envs {
			cmd := exec.Command("sh", "-c", hook.Command)
			cmd.Env = env
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"` + hook.Event + `"}`)
			out, _ := cmd.Output()
			if string(out) != "{}" {
				t.Errorf("%s with %v printed %q, want {}", hook.Event, env[1:], out)
			}
		}
	}
}
