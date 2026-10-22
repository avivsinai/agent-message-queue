//go:build darwin || linux

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// The Codex approval guard, happy path. The installed Codex commands run
// under /bin/sh and zsh (Codex runs hooks under the user's $SHELL) with a
// fake binary: with AM_ME the open starts it, and with neither AM_ME nor the
// codexidentity store nothing starts. Then the real receiver opens the
// marker from a PermissionRequest, the PostToolUse of another call keeps it,
// and the PostToolUse of the same call removes it. The payloads are the ones
// Codex 0.163 sent: its PermissionRequest input carries
// tool_input.description and its PostToolUse input does not.
func TestCodexDialogHookHoldsUntilTheApprovedCallRuns(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	fake := filepath.Join(dir, "amq")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch \"$RAN\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(fsq.AgentBase(root, "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	shells := []string{"/bin/sh"}
	if zsh, err := exec.LookPath("zsh"); err == nil {
		shells = append(shells, zsh)
	}
	store := filepath.Join(dir, "codex-threads", "current")
	for _, shell := range shells {
		for _, tc := range []struct {
			name  string
			env   []string
			start bool
		}{
			{"participant", []string{"AM_ME=codex", "AM_ROOT=" + root}, true},
			{"no AM_ME and no store", nil, false},
		} {
			for _, hook := range codexDialogHooks(fake, store) {
				if hook.Event != "PermissionRequest" {
					continue
				}
				_ = os.Remove(ran)
				cmd := exec.Command(shell, "-c", hook.Command)
				cmd.Dir = dir
				cmd.Env = append([]string{"PATH=/usr/bin:/bin", "RAN=" + ran}, tc.env...)
				cmd.Stdin = strings.NewReader(`{"hook_event_name":"PermissionRequest"}`)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s %s: %v: %s", shell, tc.name, err, out)
				}
				if _, err := os.Stat(ran); (err == nil) != tc.start {
					t.Errorf("%s %s: started amq = %v, want %v", shell, tc.name, err == nil, tc.start)
				}
			}
		}
	}

	clearSendMailboxTestEnv(t)
	t.Setenv(envMe, "codex")
	t.Setenv(envRoot, root)
	marker := filepath.Join(fsq.AgentBase(root, "codex"), wakeDialogFileName)
	hook := func(verb, input string) {
		t.Helper()
		if err := runWakeDialog([]string{"--harness", "codex", verb}, strings.NewReader(input)); err != nil {
			t.Fatalf("wake dialog %s: %v", verb, err)
		}
	}
	held := func() bool {
		_, err := os.Lstat(marker)
		return err == nil
	}
	hook("open", `{"session_id":"01a165af-5f6d-7270-bb60-671490b46ea1","turn_id":"t-1","hook_event_name":"PermissionRequest",`+
		`"tool_name":"Bash","tool_input":{"command":"touch hello.txt","description":"Allow running touch hello.txt outside the read-only sandbox?"}}`)
	if !held() {
		t.Fatal("PermissionRequest wrote no marker")
	}
	hook("close", `{"turn_id":"t-1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"ls"},"tool_use_id":"exec-1"}`)
	if !held() {
		t.Fatal("the PostToolUse of another call released the approval's hold")
	}
	hook("close", `{"turn_id":"t-1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"touch hello.txt"},"tool_use_id":"exec-2"}`)
	if held() {
		t.Fatal("the approved call's PostToolUse kept the hold")
	}
}
