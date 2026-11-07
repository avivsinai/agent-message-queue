package acp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// hookWatchSession is a live mailbox session the machine agent serves,
// whose binding records a Claude config dir holding the approval hook,
// with the owner's opt-in when optIn is set. The hook was installed from an
// amq-remote path an upgrade has since removed; the installed one is now
// at stable. It returns the settings file and the recorder of the agent's
// posts.
func hookWatchSession(t *testing.T, optIn bool) (s *Server, settings, stable string, posts func() []string) {
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
	stable = filepath.Join(home, "bin", "amq-remote")
	if err := os.MkdirAll(filepath.Dir(stable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stable, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	savedExe := remoteExecutable
	t.Cleanup(func() { remoteExecutable = savedExe })
	remoteExecutable = func() (string, error) { return stable, nil }
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
	return s, settings, stable, func() []string {
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

// Regression: on 2026-11-05 another writer removed the AMQ approval hook
// from settings.json and every approval stayed in the terminal for hours.
// With the owner's opt-in the machine agent puts the hook back within one
// tick and says so once in the session's channel. The restored hook runs
// the amq-remote installed now, never the recorded path an upgrade removed.
func TestMachineAgentRestoresARemovedApprovalHook(t *testing.T) {
	s, settings, stable, posts := hookWatchSession(t, true)
	tick(s, time.Now())
	if got := posts(); len(got) != 0 {
		t.Fatalf("posts with the hook in place = %q", got)
	}
	dropHook(t, settings)
	tick(s, time.Now())
	tick(s, time.Now())
	if cmd, _ := claude.PermissionHookCommand(settings); !strings.Contains(cmd, "'"+stable+"' claude permission-hook") {
		t.Fatalf("restored hook %q; want it to run %s", cmd, stable)
	}
	got := posts()
	if len(got) != 1 || !strings.HasPrefix(got[0], machineChanA+" The AMQ approval hook was missing from "+settings) || !strings.HasSuffix(got[0], "; restored.") {
		t.Fatalf("posts = %q; want one restored line", got)
	}
}

// Without the owner's opt-in the machine agent leaves the file alone and
// says once that the session's approvals stay in the terminal.
func TestMachineAgentOnlyWarnsWithoutTheOptIn(t *testing.T) {
	s, settings, _, posts := hookWatchSession(t, false)
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
	s, settings, _, posts := hookWatchSession(t, true)
	tick(s, time.Now())
	if err := claude.OptOutHook(settings, claude.HookApproval); err != nil {
		t.Fatal(err)
	}
	tick(s, time.Now())
	if state, _ := claude.PermissionHookState(settings); state != claude.HookMissing || len(posts()) != 0 {
		t.Fatalf("after uninstall: hook %q, posts %q; want neither", state, posts())
	}
}

// A writer that holds the settings file's lock never stalls the machine
// agent: the tick returns at once, writes and posts nothing, and the file
// is checked again on a later tick.
func TestMachineAgentSkipsASettingsFileWhoseLockIsHeld(t *testing.T) {
	s, settings, _, posts := hookWatchSession(t, true)
	tick(s, time.Now())
	dropHook(t, settings)
	home, _ := os.UserHomeDir()
	sum := sha256.Sum256([]byte(settings))
	release, ok, err := lock.TryExclusive(filepath.Join(home, ".amq", "locks", "settings-"+hex.EncodeToString(sum[:12])+".lock"))
	if err != nil || !ok {
		t.Fatalf("hold the lock: %v %v", ok, err)
	}
	done := make(chan struct{})
	go func() {
		tick(s, time.Now())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		release()
		<-done
		t.Fatal("the tick waited on a held settings lock")
	}
	if state, _ := claude.PermissionHookState(settings); state != claude.HookMissing || len(posts()) != 0 {
		t.Fatalf("with the lock held: hook %q, posts %q; want nothing", state, posts())
	}
	release()
	tick(s, time.Now())
	if state, _ := claude.PermissionHookState(settings); state != claude.HookPresent || len(posts()) != 1 {
		t.Fatalf("after the lock: hook %q, posts %q; want restored once", state, posts())
	}
}

// Review of the machine agent: a failed restore posted the plain warning
// and dropped why the restore failed.
func TestMachineAgentPostsWhyARestoreFailed(t *testing.T) {
	s, settings, _, posts := hookWatchSession(t, true)
	remoteExecutable = func() (string, error) { return "amq-remote", nil }
	tick(s, time.Now())
	dropHook(t, settings)
	tick(s, time.Now())
	got := posts()
	if len(got) != 1 || !strings.Contains(got[0], "the AMQ approval hook left "+settings) || !strings.Contains(got[0], `The restore failed: the hook binary "amq-remote" is not an installed file`) {
		t.Fatalf("posts = %q; want the warning with the restore's error", got)
	}
}
