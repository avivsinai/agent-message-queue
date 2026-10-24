//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

// Bead agent-message-queue-vd4.3 (surface audit, codex-cli 0.163.0): a
// doorbell at Codex's "Trust this folder?" dialog quit Codex. coop exec now
// writes a Startup marker before exec, and the wake holds until Codex
// records the decision in config.toml, with no hook installed.
func TestCodexTrustDialogHoldsTheDoorbellUntilTrustIsRecorded(t *testing.T) {
	h, config, real, marker := newCodexTrustDialogHarness(t)
	// #1180 review: a directory saved as untrusted still opens on Codex's
	// restricted trust dialog, so it gets the marker too.
	untrusted := "[projects.\"" + real + "\"]\ntrust_level = \"untrusted\"\n"
	if err := os.WriteFile(config, []byte(untrusted), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStartupDialogMarker(h.root, "codex", "/usr/local/bin/codex", []string{"-s", "read-only"})
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatalf("no Startup marker for an untrusted checkout: %v", err)
	}

	h.send("a", format.PriorityUrgent)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into the trust dialog", h.injected)
	}

	// #1180 review: a half-written config.toml proves nothing, so the hold
	// stays.
	if err := os.WriteFile(config, []byte("[projects.\""), 0o600); err != nil {
		t.Fatal(err)
	}
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q after the trust file became unreadable", h.injected)
	}
	// PR #1204 Codex review: another terminal drains during the dialog.
	// That drain follows no doorbell typed after the release, so it does
	// not prove that Codex accepts input.
	h.drain()
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into the trust dialog after another terminal drained", h.injected)
	}
	h.send("late", format.PriorityUrgent)

	trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(config, []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells after trust was recorded = %d, want 1", got)
	}
	// Bead agent-message-queue-vd4.3, live on 0.107.0 without the Codex
	// hook: the doorbell typed on the trust release reached a Codex whose
	// composer was not ready yet and was lost; the retries were a bare
	// Enter into an empty composer. The retry rings the full doorbell.
	h.advance(wakeDoorbellRetryBase)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells on the retry after a trust release = %d, want 1", got)
	}
	// The next launch in the trusted checkout removes the leftover marker.
	clearStaleStartupDialogMarker(h.root, "codex", "/usr/local/bin/codex", nil)
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("Startup marker left after a launch with no dialog: %v", err)
	}

	// #1180 ruling: a directory that stays untrusted has no readiness
	// signal without the Codex SessionStart hook; after 10 minutes the
	// existing aging applies: input still held, attention output instead.
	if err := os.WriteFile(config, []byte(untrusted), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStartupDialogMarker(h.root, "codex", "/usr/local/bin/codex", nil)
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatal(err)
	}
	rang := len(h.injected)
	h.send("b", format.PriorityUrgent)
	h.advance(wakeDialogQuietHold)
	h.scan(h.cfg)
	if len(h.injected) != rang || h.cfg.dialogHold != wakeDialogUnknown {
		t.Fatalf("aged untrusted hold: injected %q, dialog state %v; want held with attention", h.injected[rang:], h.cfg.dialogHold)
	}
}

// PR #1204 Codex review: after a trust release, only a drain that follows
// a doorbell actually typed into Codex proves that Codex accepts input.
// Each row ends at a retry that must still type the full doorbell.
func TestCodexTrustReleaseRetypesTheDoorbellUntilInputIsProven(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, h *wakeHoldHarness)
	}{
		{
			// An inbox file replaced in place is a new message, not a drain.
			name: "replacement after the release",
			run: func(t *testing.T, h *wakeHoldHarness) {
				h.send("a", format.PriorityUrgent)
				h.scan(h.cfg)
				if got := h.rings(); got != 1 {
					t.Fatalf("full doorbells on the trust release = %d, want 1", got)
				}
				h.drain()
				h.send("a", format.PriorityUrgent)
				h.scan(h.cfg)
				if got := h.rings(); got != 1 {
					t.Fatalf("full doorbells for the replaced message = %d, want 1", got)
				}
			},
		},
		{
			// A deferred attempt types nothing, so a drain after it proves
			// nothing.
			name: "deferred attempt then a drain",
			run: func(t *testing.T, h *wakeHoldHarness) {
				h.cfg.injectVia = "test-hook"
				h.cfg.injectViaHook = func(string) (string, error) {
					return "AMQ_INJECT_PROGRESS=deferred\n", errors.New("exit status 1")
				}
				h.send("a", format.PriorityUrgent)
				if err := notifyNewMessages(h.cfg); !isWakeInjectorDeferred(err) {
					t.Fatalf("release attempt error = %v, want deferred", err)
				}
				h.cfg.injectVia, h.cfg.injectViaHook = "", nil
				h.drain()
				h.scan(h.cfg)
				h.send("b", format.PriorityUrgent)
				h.scan(h.cfg)
				if got := h.rings(); got != 1 {
					t.Fatalf("full doorbells for new mail after the drain = %d, want 1", got)
				}
			},
		},
		{
			// A busy-input deferral types nothing, also when its attention
			// output is throttled.
			name: "busy-input deferral with attention throttled then a drain",
			run: func(t *testing.T, h *wakeHoldHarness) {
				old := waitForWakeInputQuiet
				t.Cleanup(func() { waitForWakeInputQuiet = old })
				waitForWakeInputQuiet = func(*wakeConfig) bool { return false }
				h.cfg.settings.deferWhileInput = true
				h.cfg.doorbell.recordTransientAttentionAttempt(h.now)
				h.send("a", format.PriorityUrgent)
				h.scan(h.cfg)
				if len(h.injected) != 0 {
					t.Fatalf("typed %q while the input was busy", h.injected)
				}
				waitForWakeInputQuiet = old
				h.cfg.settings.deferWhileInput = false
				h.drain()
				h.scan(h.cfg)
				h.send("b", format.PriorityUrgent)
				h.scan(h.cfg)
				if got := h.rings(); got != 1 {
					t.Fatalf("full doorbells for new mail after the drain = %d, want 1", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, config, real, marker := newCodexTrustDialogHarness(t)
			writeStartupDialogMarker(h.root, "codex", "/usr/local/bin/codex", nil)
			if _, err := os.Lstat(marker); err != nil {
				t.Fatalf("no Startup marker for an unrecorded checkout: %v", err)
			}
			// Codex records trust, but no SessionStart hook removes the
			// marker: a trust release.
			trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
			if err := os.WriteFile(config, []byte(trusted), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.run(t, h)
			h.advance(wakeDoorbellRetryBase)
			h.scan(h.cfg)
			if got := h.rings(); got != 1 {
				t.Fatalf("full doorbells on the retry = %d, want 1", got)
			}
		})
	}
}

// newCodexTrustDialogHarness returns a wake harness for a Codex agent in a
// fresh git checkout, with the dialog sources reading the agent's marker,
// plus the Codex config.toml path, the checkout's real path, and the
// marker path.
func newCodexTrustDialogHarness(t *testing.T) (h *wakeHoldHarness, config, real, marker string) {
	t.Helper()
	h = newWakeHoldHarness(t, wakeHoldPolicy{})
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	project := secureTempDirForTest(t)
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir)
	h.cfg.startupTrustRelease = wakeStartupTrustReleaseInDir(agentDir)
	return h, filepath.Join(codexHome, "config.toml"), real, filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
}

// #1180 review: coop exec wrote or removed the Startup marker before
// prepareCoopWakeLock refused a launch for another live owner, so a refused
// launch from a trusted directory cleared the running owner's trust hold.
func TestRefusedCoopLaunchKeepsTheLiveOwnersStartupMarker(t *testing.T) {
	clearSendMailboxTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	project := secureTempDirForTest(t)
	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	trusted := `{"projects":{"` + real + `":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := secureTempDirForTest(t)
	// The test process is running and is no amq wake: a live owner coop
	// exec must not touch.
	writeWakeLockExactForTest(t, root, "claude", wakeLock{PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: "claude"})
	marker := filepath.Join(fsq.AgentBase(root, "claude"), wakeDialogFileName)
	owners := []byte(`{"schema":1,"event":"Startup","harness":"claude","cwd":"/elsewhere","opened_at":"x"}` + "\n")
	if err := os.WriteFile(marker, owners, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = stubCoopExecSentinel(t)

	if err := runCoopExec([]string{"--root", root, "--me", "claude", "-y", "claude"}); err == nil {
		t.Fatal("coop exec started beside a live wake owner")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != string(owners) {
		t.Fatalf("live owner's Startup marker = %q, %v; want it unchanged", got, err)
	}
}

// #1180 review: coop exec wrote the Startup marker only after its wake was
// ready, so a message that arrived in between was typed into the terminal,
// waited there, and answered the trust dialog when the harness started.
// The wake now admits no input until coop exec releases its startup hold,
// which it does after the marker is in place.
func TestCoopWakeTypesNothingBeforeTheStartupHoldIsReleased(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	childEnd, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Close() })
	fd, err := unix.Dup(int(childEnd.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = childEnd.Close()
	t.Cleanup(func() { wakeStartupHoldFile = nil })
	t.Setenv(envWakeStartupHold, strconv.Itoa(fd))
	released, err := wakeStartupHoldFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	h.cfg.dialogSource = withWakeStartupHold(wakeDialogSourceInDir(agentDir), released)

	h.send("a", format.PriorityUrgent)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q before coop exec released the startup hold", h.injected)
	}
	_ = hold.Close()
	<-released
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells after the startup hold was released = %d, want 1", got)
	}
}

// Lead ruling on #1180: a trust hold that never closes (for example a Codex
// directory kept untrusted, without the Codex hook) must be visible:
// amq wake check reports it, its age, and what releases it.
func TestWakeCheckReportsAnOpenTrustHold(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fsq.AgentBase(root, "codex"), wakeDialogFileName)
	raw := `{"schema":1,"event":"Startup","harness":"codex","cwd":"/work","opened_at":"x"}` + "\n"
	if err := os.WriteFile(marker, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(marker, opened, opened); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return runWake([]string{"check", "--me", "codex", "--root", root, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		StartupHold *wakeStartupHoldStatus `json:"startup_hold"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("wake check JSON %q: %v", out, err)
	}
	hold := result.StartupHold
	if hold == nil || hold.Harness != "codex" || hold.Dir != "/work" || hold.AgeSeconds < 600 ||
		!strings.Contains(hold.Release, "install-dialog-hook --harness codex") {
		t.Fatalf("startup hold = %#v", hold)
	}

	// #1180 review: wake check reported a marker as held after Codex had
	// recorded trust and the wake had released input.
	project := secureTempDirForTest(t)
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	accepted := `{"schema":1,"event":"Startup","harness":"codex","cwd":"` + real + `","opened_at":"x"}` + "\n"
	if err := os.WriteFile(marker, []byte(accepted), 0o600); err != nil {
		t.Fatal(err)
	}
	if hold := inspectWakeStartupHold(root, "codex", time.Now()); hold != nil {
		t.Fatalf("startup hold after trust was recorded = %#v, want none", hold)
	}
}

// #1180 review: a wake restart or self-upgrade during the trust dialog
// exec'd a successor without the startup hold, which then typed into the
// dialog. The restart passes the hold on; the successor holds until coop
// exec releases it.
func TestWakeRestartKeepsAPendingStartupHold(t *testing.T) {
	childEnd, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Close() })
	fd, err := unix.Dup(int(childEnd.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = childEnd.Close()
	t.Cleanup(func() { wakeStartupHoldFile = nil })
	t.Setenv(envWakeStartupHold, strconv.Itoa(fd))
	if _, err := wakeStartupHoldFromEnv(); err != nil {
		t.Fatal(err)
	}
	successorEnv := carryWakeStartupHold(nil)
	var value string
	for _, entry := range successorEnv {
		if v, found := strings.CutPrefix(entry, envWakeStartupHold+"="); found {
			value = v
		}
	}
	if value == "" {
		t.Fatalf("restart env %q carries no startup hold", successorEnv)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC != 0 {
		t.Fatalf("startup hold fd flags = %#x, %v; it would close at the restart exec", flags, err)
	}
	// The successor adopts the same descriptor after exec.
	t.Setenv(envWakeStartupHold, value)
	released, err := wakeStartupHoldFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-released:
		t.Fatal("successor released the hold before coop exec did")
	case <-time.After(50 * time.Millisecond):
	}
	_ = hold.Close()
	<-released
}

// #1190 review: a remover read the marker, compared it, then unlinked
// whatever was there, so a writer that replaced the marker in between lost
// its hold. Replacement and conditional removal now share one lock.
func TestMarkerReplacedDuringARemovalSurvives(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "agy"); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "agy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	marker := filepath.Join(fsq.AgentBase(root, "agy"), wakeDialogFileName)
	if err := os.WriteFile(marker, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newer := []byte(`{"schema":1,"event":"PermissionRequest","opened_at":"x"}` + "\n")
	replaced := make(chan struct{})
	err = agentDir.withFD(func(dirfd int) error {
		return removeWakeDialogMarkerIf(dirfd, agentDir, func(current []byte) bool {
			// Another writer replaces the marker between the compare and
			// the unlink.
			go func() {
				defer close(replaced)
				_ = agentDir.withFD(func(dirfd int) error {
					return replaceWakeDialogMarkerAt(dirfd, agentDir, newer)
				})
			}()
			select {
			case <-replaced:
			case <-time.After(200 * time.Millisecond):
			}
			return string(current) == "mine\n"
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	<-replaced
	if got, err := os.ReadFile(marker); err != nil || string(got) != string(newer) {
		t.Fatalf("marker after a replacement during removal = %q, %v; want the newer marker", got, err)
	}
}
