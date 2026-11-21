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
	return guardedClaudeDoorbellMode(t, true, wakeInjectModeRaw)
}

func guardedClaudeDoorbellMode(t *testing.T, hooked bool, mode string) (h *wakeHoldHarness, read func(string), quietWaits *int) {
	return doorbellOnPrivateTTY(t, hooked, mode, true)
}

func doorbellOnPrivateTTY(t *testing.T, hooked bool, mode string, guard bool) (h *wakeHoldHarness, read func(string), quietWaits *int) {
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
	if hooked {
		if err := claude.InstallCommandHooks(home, dialogguard.ClaudeHooks(bin, []string{"UserPromptSubmit"})); err != nil {
			t.Fatal(err)
		}
	}
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = slave
	t.Cleanup(func() { os.Stdin = oldStdin; _ = master.Close(); _ = slave.Close() })
	var pasteRead, pasteWrite *os.File
	pastePending := 0
	if mode == wakeInjectModePaste {
		pasteRead, pasteWrite, err = os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdin = pasteRead
		t.Cleanup(func() { _ = pasteRead.Close(); _ = pasteWrite.Close() })
	}
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
	start := wakeStart{flags: newWakeStartFlags(), root: h.root, me: "codex", harness: "claude", injectMode: mode,
		flagSettings: defaultWakeSettings(), activeAgentDir: dir,
		requestedOwner: &wakeOwner{PID: host.PID, ProcessStart: host.ProcessStart, BootID: host.BootID},
		currentWake:    wakeLockInspection{Lock: wakeLock{PID: os.Getpid(), TTY: slave.Name(), WakeMode: mode}}}
	writeWakeLockForTest(t, h.root, "codex", start.currentWake.Lock)
	if err := start.loadStartupSettings(); err != nil {
		t.Fatal(err)
	}
	cfg := start.newLoopConfig()
	cfg.injectMode = mode
	cfg.settings.inputGuard = guard
	h.cfg = &cfg
	h.now = time.Now().Add(3 * time.Second)
	cfg.doorbellNow = func() time.Time { return h.now }
	if mode == wakeInjectModePaste {
		cfg.terminalWrite = func(s string) error {
			if _, err := pasteWrite.WriteString(s); err != nil {
				return err
			}
			pastePending += len(s)
			h.injected = append(h.injected, s)
			return nil
		}
		editorRead := read
		cfg.inputGuard.sample = func() (ttyInputState, error) {
			input := make([]byte, pastePending)
			if _, err := io.ReadFull(pasteRead, input); err != nil {
				return ttyInputState{}, err
			}
			pastePending = 0
			editorRead(string(input))
			return sampleTTYInputState(slave.Fd(), slave.Fd())
		}
	} else {
		cfg.terminalWrite = func(s string) error { read(s); h.injected = append(h.injected, s); return nil }
	}
	cfg.attentionIsTTY = func() bool { return false }
	if hooked {
		args := []string{"--host-pid", strconv.Itoa(host.PID), dialogguard.ClaudeHookVerb("UserPromptSubmit")}
		if err := runWakeDialog(args, strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"local"}`)); err != nil {
			t.Fatal(err)
		}
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
				h.advance(30 * time.Second)
			}
			h.send("pending", "normal")
			h.scan(h.cfg)
			if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r", "\r"}) || *waits != 0 {
				t.Fatalf("wrote %q with %d quiet waits", h.injected, *waits)
			}
		})
	}
}

// agent-message-queue-fsmt: a plain seat sent bare Enter into a draft after its own ring.
func TestPlainDoorbellReminderAfterInputIsNotBareEnter(t *testing.T) {
	h, read, _ := guardedClaudeDoorbellMode(t, false, wakeInjectModeRaw)
	h.send("pending", "normal")
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r", "\r"}) {
		t.Fatalf("initial ring = %q", h.injected)
	}
	h.injected = nil
	read("draft")
	h.now = h.deadline()
	notice := captureWakeStderr(t, func() { h.scan(h.cfg) })
	if len(h.injected) != 0 || notice == "" {
		t.Fatalf("input was read: reminder wrote %q, notice %q", h.injected, notice)
	}
	h.advance(30 * time.Second)
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r", "\r"}) {
		t.Fatalf("stale reminder = %q, want full doorbell", h.injected)
	}
}

func TestPlainDoorbellNeedsNoQuietWaitAfterOwnWrite(t *testing.T) {
	for _, mode := range []string{wakeInjectModeRaw, wakeInjectModePaste} {
		t.Run(mode, func(t *testing.T) {
			h, _, waits := guardedClaudeDoorbellMode(t, false, mode)
			if mode == wakeInjectModeRaw {
				// agent-message-queue-fsmt live run on v0.159.0: the reader marked
				// its read after the wake's first empty-queue sample.
				firstEmpty := h.now.Add(-time.Second)
				readAt := firstEmpty
				h.cfg.inputGuard.sample = func() (ttyInputState, error) {
					sample := ttyInputState{hasLastRead: true, lastRead: readAt}
					if len(h.injected) == 3 { // the initial doorbell's rescue Enter
						readAt = firstEmpty.Add(142 * time.Microsecond)
					}
					return sample, nil
				}
			}
			h.send("pending", "normal")
			h.scan(h.cfg)
			h.send("second", "normal")
			h.injected = nil
			*waits = 0
			h.now = h.deadline()
			h.scan(h.cfg)
			want := []string{coopWakeDoorbell, "\r"}
			if mode == wakeInjectModeRaw {
				want = append(want, "\r")
			}
			if !slices.Equal(h.injected, want) || *waits != 0 {
				t.Fatalf("second ring = %q with %d quiet waits, want %q and none", h.injected, *waits, want)
			}
		})
	}
}

// agent-message-queue-fsmt live run on v0.159.0: a held Enter was sent when
// the input became stale and submitted the user's draft alone.
func TestStaleInputAfterDoorbellTextRetriesWithFullDoorbell(t *testing.T) {
	h, read, _ := guardedClaudeDoorbell(t)
	rawInjectSleep = func(delay time.Duration) {
		if delay == rawInjectSettleDelay {
			read("draft")
			rawInjectSleep = func(time.Duration) {}
		}
	}
	h.send("pending", "normal")
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell}) {
		t.Fatalf("input before the draft aged = %q", h.injected)
	}
	h.injected = nil
	h.advance(h.cfg.settings.inputStaleFor + time.Second)
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r", "\r"}) {
		t.Fatalf("input after the draft aged = %q, want the full doorbell and submit", h.injected)
	}
}

// agent-message-queue-fsmt review: a seat whose terminal cannot be sampled
// has no fact about its input, so the Enter of its doorbell is still sent.
func TestDoorbellSubmitsWhenTheTerminalCannotBeSampled(t *testing.T) {
	h, _, _ := guardedClaudeDoorbellMode(t, false, wakeInjectModeRaw)
	h.cfg.inputGuard.sample = func() (ttyInputState, error) { return ttyInputState{}, os.ErrInvalid }
	h.send("pending", "normal")
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r", "\r"}) {
		t.Fatalf("wrote %q, want the doorbell and its Enter", h.injected)
	}
}

// With the input guard off, which is the default, the wake keeps its earlier
// behavior: a reminder for an unread cohort is a bare Enter.
func TestInputGuardOffKeepsTheEarlierReminder(t *testing.T) {
	h, read, _ := doorbellOnPrivateTTY(t, false, wakeInjectModeRaw, false)
	h.send("pending", "normal")
	h.scan(h.cfg)
	read("xyz")
	h.injected = nil
	h.now = h.deadline()
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{"\r", "\r"}) {
		t.Fatalf("reminder with the guard off wrote %q, want the earlier bare Enter", h.injected)
	}
}
