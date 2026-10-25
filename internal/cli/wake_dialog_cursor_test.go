//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// The Cursor approval guard, happy path: the installer writes a hooks file
// Cursor loads (version 1, one entry per event) and doctor reports it
// installed; the receiver opens the marker from the preToolUse Cursor
// 2026.10.22 sent before "Run this command?", and the turn's stop removes it.
func TestCursorDialogHookHoldsUntilTheTurnEnds(t *testing.T) {
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
	hook("open", `{"conversation_id":"3b3d8b96-c4e1-4179-8039-66951ada578c","generation_id":"7f8505fe-c99f-4299-94c8-4dd6f89a3414",`+
		`"tool_name":"Shell","tool_input":{"command":"touch guarded.txt","cwd":"","timeout":30000},"tool_use_id":"df9fada2-3518-48d3-9e09-da6f21911f48",`+
		`"cwd":"","session_id":"3b3d8b96-c4e1-4179-8039-66951ada578c","hook_event_name":"preToolUse"}`)
	if !held() {
		t.Fatal("preToolUse wrote no marker")
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
	if err := installCursorDialogHook("/opt/amq//bin/amq", false); err != nil {
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
	// The escaped entries decode to the command written: doctor reads the
	// binary back.
	if check, _ := checkCursorDialogHook(); !strings.Contains(check.Message, "missing amq binary /opt/amq//bin/amq") {
		t.Errorf("doctor = %+v", check)
	}
}
