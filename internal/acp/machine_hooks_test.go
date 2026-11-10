package acp

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// hookWatchSession is a live mailbox session the machine agent serves,
// whose binding records a Claude config dir holding the approval hook,
// with the owner's opt-in when optIn is set. It returns the settings file
// and the recorder of the agent's posts.
func hookWatchSession(t *testing.T, optIn bool) (s *Server, settings string, posts func() []string) {
	t.Helper()
	s, _ = machineServer(t)
	home := canonicalTempDir(t)
	t.Setenv("HOME", home)
	b := liveSession(t, "watched")
	b.ConfigDir = filepath.Join(home, "claude")
	if err := binding.WriteNamed(b); err != nil {
		t.Fatal(err)
	}
	activeChannel(t, s, b, machineChanA)
	settings = filepath.Join(b.ConfigDir, "settings.json")
	bin := filepath.Join(home, "Cellar", "1.0", "amq-remote")
	var err error
	if optIn {
		_, err = claude.OptInPermissionHook(home, settings, bin)
	} else {
		_, err = claude.InstallPermissionHookAt(home, settings, bin)
	}
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	saved := postAnswer
	t.Cleanup(func() { postAnswer = saved })
	postAnswer = func(channel, text string, _ time.Duration) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, channel+" "+text)
		return strings.Repeat("f", 64), nil
	}
	return s, settings, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

// dropHook replaces the settings file with a copy that has no AMQ hook, as
// another writer that saves its own older copy does.
func dropHook(t *testing.T, settings string) {
	t.Helper()
	tmp := settings + ".other"
	if err := os.WriteFile(tmp, []byte(`{"model":"other-writer"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, settings); err != nil {
		t.Fatal(err)
	}
}

// The machine agent never writes a user's saved harness settings: with the
// owner's opt-in it put a removed hook back on its own tick. Now it leaves
// the file as the other writer saved it and says once in the session's
// channel that approvals stay in the terminal (audit 2026-11-10).
func TestMachineAgentReportsARemovedApprovalHookAndLeavesTheFile(t *testing.T) {
	s, settings, posts := hookWatchSession(t, true)
	tick(s, time.Now())
	if got := posts(); len(got) != 0 {
		t.Fatalf("posts with the hook in place = %q", got)
	}
	dropHook(t, settings)
	tick(s, time.Now())
	tick(s, time.Now())
	if raw, _ := os.ReadFile(settings); string(raw) != `{"model":"other-writer"}`+"\n" {
		t.Fatalf("settings rewritten by the machine agent:\n%s", raw)
	}
	got := posts()
	if len(got) != 1 || !strings.HasPrefix(got[0], machineChanA+" Approvals of this session stay in the terminal: the AMQ approval hook left "+settings) {
		t.Fatalf("posts = %q; want one warning", got)
	}
}

// Without the owner's opt-in the machine agent leaves the file alone and
// says once that the session's approvals stay in the terminal.
func TestMachineAgentOnlyWarnsWithoutTheOptIn(t *testing.T) {
	s, settings, posts := hookWatchSession(t, false)
	tick(s, time.Now())
	dropHook(t, settings)
	tick(s, time.Now())
	tick(s, time.Now())
	if state, _ := claude.PermissionHookState(settings); state != claude.HookMissing {
		t.Fatalf("approval hook %q; want it left out", state)
	}
	got := posts()
	if len(got) != 1 || !strings.HasPrefix(got[0], machineChanA+" Approvals of this session stay in the terminal: the AMQ approval hook left "+settings) {
		t.Fatalf("posts = %q; want one warning", got)
	}
	// Review of the machine agent's warning: a restart (Buzz Desktop, or
	// another leader) repeated it for the same file.
	s.resign()
	tick(newMachineServer(t, s.cfg.StateDir), time.Now())
	if got := posts(); len(got) != 1 {
		t.Fatalf("posts after a restart = %q; want the one warning only", got)
	}
}

// Review of the machine agent's warning: an owner who removed the hook
// with uninstall-approval-hook was warned about it on every restart.
func TestMachineAgentNeverWarnsAfterTheOwnerRemovedTheHook(t *testing.T) {
	s, settings, posts := hookWatchSession(t, true)
	tick(s, time.Now())
	if err := claude.OptOutHook(settings, claude.HookApproval); err != nil {
		t.Fatal(err)
	}
	tick(s, time.Now())
	if state, _ := claude.PermissionHookState(settings); state != claude.HookMissing || len(posts()) != 0 {
		t.Fatalf("after uninstall: hook %q, posts %q; want neither", state, posts())
	}
}

// Review of the machine agent's upgrade: it rewrote an older-form hook and
// recorded an opt-in the owner never gave. With no opt-in it only says,
// once, that doctor --fix upgrades the hook.
func TestMachineAgentOnlyReportsAnOldFormHookWithoutTheOptIn(t *testing.T) {
	s, settings, posts := hookWatchSession(t, false)
	tick(s, time.Now())
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(raw, []byte(`claude permission-hook"`), []byte(`claude permission-hook --wait 600"`), 1)
	if err := os.WriteFile(settings, old, 0o600); err != nil {
		t.Fatal(err)
	}
	tick(s, time.Now())
	tick(s, time.Now())
	if got, _ := os.ReadFile(settings); !bytes.Equal(got, old) {
		t.Fatalf("settings changed without an opt-in:\n%s", got)
	}
	if in, _ := claude.ReadHookOptIn(settings); in.Approval != nil {
		t.Fatalf("opt-in created: %+v", in)
	}
	if got := posts(); len(got) != 1 || !strings.Contains(got[0], " is an older form ") || !strings.Contains(got[0], "amq-remote doctor --fix") {
		t.Fatalf("posts = %q; want one older-form report", got)
	}
}
