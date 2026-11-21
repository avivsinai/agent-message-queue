package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	return doorbellOnPrivateTTY(t, hooked, mode, true, "claude")
}

func doorbellOnPrivateTTY(t *testing.T, hooked bool, mode string, guard bool, hookHarness ...string) (h *wakeHoldHarness, read func(string), quietWaits *int) {
	t.Helper()
	seatHarness := "claude"
	if len(hookHarness) > 0 {
		seatHarness = hookHarness[0]
	}
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
		if seatHarness == "cursor" {
			if err := os.Mkdir(filepath.Join(home, ".cursor"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := installCursorDialogHook(bin, false); err != nil {
				t.Fatal(err)
			}
		} else if err := claude.InstallCommandHooks(home, dialogguard.ClaudeHooks(bin, []string{"UserPromptSubmit"})); err != nil {
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
	start := wakeStart{flags: newWakeStartFlags(), root: h.root, me: "codex", harness: seatHarness, injectMode: mode,
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
		args := []string{"--host-pid", strconv.Itoa(host.PID)}
		input := ""
		if seatHarness == "cursor" {
			args = append(args, "--harness", "cursor", cursorDialogHookVerb("beforeSubmitPrompt"))
			input = `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"cursor-local"}`
		} else {
			args = append(args, dialogguard.ClaudeHookVerb("UserPromptSubmit"))
			input = `{"hook_event_name":"UserPromptSubmit","session_id":"local"}`
		}
		if err := runWakeDialog(args, strings.NewReader(input)); err != nil {
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

func TestWakeCursorSubmitFactTypesPromptlyAndHoldsDraft(t *testing.T) {
	// agent-message-queue-nkky: Cursor's beforeSubmitPrompt arrives with
	// conversation_id, in the paste-mode seat, and provides the same submit
	// fact the already covered raw Claude Code hook does.
	h, read, waits := doorbellOnPrivateTTY(t, true, wakeInjectModePaste, true, "cursor")
	if got := h.cfg.inputGuard.status(h.cfg.settings); got != properInputGuard("cursor") {
		t.Fatalf("Cursor installation status = %q, want proper (cursor)", got)
	}
	h.send("pending", "normal")
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r"}) || *waits != 0 {
		t.Fatalf("after Cursor submit: wrote %q with %d quiet waits", h.injected, *waits)
	}
	h.cfg.inputDelivery.reset()
	h.injected = nil
	read("partial draft")
	h.send("draft-pending", "normal")
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q after text was entered since the Cursor submit", h.injected)
	}
}

// agent-message-queue-fsmt: an unchanged cohort sent bare Enter into a draft edited after the first ring.
func TestNotifyNewMessagesReminderAfterInputSincePresentation(t *testing.T) {
	h, read, _ := guardedClaudeDoorbell(t)
	h.send("pending", "normal")
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r"}) {
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
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r"}) {
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
			if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r"}) || *waits != 0 {
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
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r"}) {
		t.Fatalf("initial ring = %q", h.injected)
	}
	h.injected = nil
	read("draft")
	h.now = h.deadline()
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("input was read: reminder wrote %q", h.injected)
	}
	h.advance(30 * time.Second)
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r"}) {
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
					if len(h.injected) == 2 { // the initial doorbell's Enter
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
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r"}) {
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

// agent-message-queue-fsmt review: with a paste still queued in the terminal,
// the wake sent its Enter anyway.
func TestQueuedPasteHoldsTheEnter(t *testing.T) {
	h, _, _ := guardedClaudeDoorbellMode(t, false, wakeInjectModePaste)
	h.cfg.inputGuard.sample = func() (ttyInputState, error) {
		return ttyInputState{pendingBytes: 1, hasLastRead: true, lastRead: h.now.Add(-time.Minute)}, nil
	}
	h.send("pending", "normal")
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell}) {
		t.Fatalf("wrote %q, want the doorbell text and no Enter", h.injected)
	}
}

// agent-message-queue-h7o7, real-pty runs: keys read 0.4 to 1.3 ms after the
// wake's Enter count as the wake's own write. The rescue Enter 150 ms later
// submitted them alone, and without it the bare reminder did 5 s later.
func TestKeysReadInsideTheSettleIntervalAreNotSubmittedAlone(t *testing.T) {
	h, read, _ := guardedClaudeDoorbellMode(t, false, wakeInjectModeRaw)
	settle := rawInjectSleep
	typed := false
	rawInjectSleep = func(delay time.Duration) {
		// The person's keys are read inside the settle interval after the
		// Enter, so the wake's own read time covers them (1.3 ms in the run).
		if delay == rawInjectDrainPollInterval && len(h.injected) == 2 && !typed {
			typed = true
			read("xyz")
		}
	}
	t.Cleanup(func() { rawInjectSleep = settle })
	h.send("pending", "normal")
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r"}) {
		t.Fatalf("wrote %q, want the doorbell and one Enter", h.injected)
	}
	h.injected = nil
	h.now = h.deadline()
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("reminder for the unread mail wrote %q, want nothing before the stale time", h.injected)
	}
	h.advance(h.cfg.settings.inputStaleFor)
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r"}) {
		t.Fatalf("after the stale time wrote %q, want the full doorbell", h.injected)
	}
	// Review of agent-message-queue-h7o7: a busy agent got each further
	// reminder as one more message.
	h.injected = nil
	h.advance(10 * h.cfg.settings.inputStaleFor)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("a second reminder wrote %q, want one reminder for a cohort", h.injected)
	}
}

// Review of agent-message-queue-h7o7: a held reminder kept the deadline of
// the input_stale_for value it started with.
func TestHeldReminderFollowsAChangedStaleTime(t *testing.T) {
	h, _, _ := guardedClaudeDoorbellMode(t, false, wakeInjectModeRaw)
	h.send("pending", "normal")
	h.scan(h.cfg)
	h.injected = nil
	h.now = h.deadline()
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("reminder inside the stale time wrote %q", h.injected)
	}
	h.cfg.settings.inputStaleFor = time.Second
	h.advance(6 * time.Second)
	h.scan(h.cfg)
	if !slices.Equal(h.injected, []string{coopWakeDoorbell, "\r"}) {
		t.Fatalf("after the stale time became 1s wrote %q, want the full doorbell", h.injected)
	}
}
