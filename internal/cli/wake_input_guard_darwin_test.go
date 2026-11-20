package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/dialogguard"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/creack/pty"
	"golang.org/x/term"
)

// Run the installed submit receiver and use a private tty for both the
// simulated editor and the wake. The clock advances past the quiet window
// without sleeping or touching the user's terminal.
func guardedClaudeDoorbell(t *testing.T) (h *wakeHoldHarness, read func(string), quietWaits *int) {
	t.Helper()
	h = newWakeHoldHarness(t, wakeHoldPolicy{})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AM_ME", "codex")
	t.Setenv("AM_ROOT", h.root)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := claude.InstallCommandHooks(home, dialogguard.ClaudeHooks(bin, []string{"UserPromptSubmit"})); err != nil {
		t.Fatal(err)
	}
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = slave
	t.Cleanup(func() { os.Stdin = oldStdin; _ = master.Close(); _ = slave.Close() })
	if _, err := term.MakeRaw(int(slave.Fd())); err != nil {
		t.Fatal(err)
	}
	read = func(input string) {
		t.Helper()
		if _, err := io.WriteString(master, input); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(input))
		if _, err := io.ReadFull(slave, buf); err != nil || string(buf) != input {
			t.Fatalf("private tty read = %q, %v", buf, err)
		}
	}
	read("x")
	parents := launch.Ancestors()
	if len(parents) == 0 {
		t.Fatal("missing hook host")
	}
	host := parents[0]
	dir, err := openExistingWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	start := wakeStart{flags: newWakeStartFlags(), root: h.root, me: "codex", harness: "claude", injectMode: wakeInjectModeRaw,
		flagSettings: defaultWakeSettings(), activeAgentDir: dir,
		requestedOwner: &wakeOwner{PID: host.PID, ProcessStart: host.ProcessStart, BootID: host.BootID},
		currentWake:    wakeLockInspection{Lock: wakeLock{PID: os.Getpid(), TTY: slave.Name(), WakeMode: wakeInjectModeRaw}}}
	writeWakeLockForTest(t, h.root, "codex", start.currentWake.Lock)
	if err := start.loadStartupSettings(); err != nil {
		t.Fatal(err)
	}
	cfg := start.newLoopConfig()
	h.cfg = &cfg
	h.now = time.Now().Add(3 * time.Second)
	cfg.doorbellNow = func() time.Time { return h.now }
	cfg.terminalWrite = func(s string) error { read(s); h.injected = append(h.injected, s); return nil }
	cfg.attentionIsTTY = func() bool { return false }
	args := []string{"--host-pid", strconv.Itoa(host.PID), dialogguard.ClaudeHookVerb("UserPromptSubmit")}
	if err := runWakeDialog(args, strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"local"}`)); err != nil {
		t.Fatal(err)
	}

	quietWaits = new(int)
	oldQuiet := waitForWakeInputQuiet
	t.Cleanup(func() { waitForWakeInputQuiet = oldQuiet })
	waitForWakeInputQuiet = func(cfg *wakeConfig) bool {
		now := h.now
		ok, _, err := waitForInputQuiet(func() (ttyInputState, error) { return sampleTTYInputState(slave.Fd(), slave.Fd()) },
			func() time.Time { return now }, func(delay time.Duration, _ ttyInputState, _ string) { *quietWaits++; now = now.Add(delay) },
			cfg.settings.inputQuietFor, cfg.settings.inputMaxHold, cfg.settings.inputPollInterval)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	return h, read, quietWaits
}

// agent-message-queue-fsmt: a pause in a draft let the old quiet timer type a doorbell into it.
func TestWakeHoldsDoorbellAfterInputSinceSubmit(t *testing.T) {
	h, read, _ := guardedClaudeDoorbell(t)
	read("draft")
	h.send("pending", "normal")
	notice := captureWakeStderr(t, func() { h.scan(h.cfg) })
	if len(h.injected) != 0 || notice == "" || !h.cfg.lastAttemptTransientAttention {
		t.Fatalf("after draft: wrote %q, transient notice %q", h.injected, notice)
	}
}

// agent-message-queue-fsmt: an unchanged cohort sent bare Enter into a draft edited after the first ring.
func TestNotifyNewMessagesReminderAfterInputSincePresentation(t *testing.T) {
	h, read, _ := guardedClaudeDoorbell(t)
	h.send("pending", "normal")
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r", "\r"}) {
		t.Fatalf("initial ring = %q", h.injected)
	}
	h.injected = nil
	read("draft")
	h.now = h.deadline()
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("reminder wrote %q into the edited prompt", h.injected)
	}
	h.advance(5 * time.Minute)
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r", "\r"}) {
		t.Fatalf("stale reminder = %q, want full doorbell", h.injected)
	}
}

func TestWakeTypesCleanAndStaleDoorbells(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			h, read, waits := guardedClaudeDoorbell(t)
			if stale {
				read("draft")
				h.advance(5 * time.Minute)
			}
			h.send("pending", "normal")
			h.scan(h.cfg)
			if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r", "\r"}) || *waits != 0 {
				t.Fatalf("wrote %q with %d quiet waits", h.injected, *waits)
			}
		})
	}
}
