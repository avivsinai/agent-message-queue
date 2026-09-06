package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
)

type wakeInfoErrorDirEntry struct {
	os.DirEntry
	err error
}

type wakeTestAcceptedProgressError struct {
	err      error
	accepted int
}

func (err *wakeTestAcceptedProgressError) Error() string {
	return err.err.Error()
}

func (err *wakeTestAcceptedProgressError) Unwrap() error {
	return err.err
}

func (err *wakeTestAcceptedProgressError) wakeAcceptedBytes() int {
	return err.accepted
}

func (entry wakeInfoErrorDirEntry) Info() (os.FileInfo, error) {
	info, _ := entry.DirEntry.Info()
	return info, entry.err
}

type wakeStaticInboxReader struct {
	entries []os.DirEntry
	headers map[string]format.Header
}

func (reader wakeStaticInboxReader) ReadDir() ([]os.DirEntry, error) {
	return reader.entries, nil
}

func (reader wakeStaticInboxReader) ReadHeader(name string) (format.Header, error) {
	header, ok := reader.headers[name]
	if !ok {
		return format.Header{}, os.ErrNotExist
	}
	return header, nil
}

func deliverPartialWakeMessageForTest(t *testing.T, root, me, id string) {
	t.Helper()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, me); err != nil {
		t.Fatalf("ensure mailbox: %v", err)
	}
	message := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      id,
			From:    "peer",
			To:      []string{me},
			Thread:  "p2p/peer__" + me,
			Subject: id,
			Created: "2026-07-30T08:00:00Z",
		},
		Body: "body",
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, me, id+".md", data); err != nil {
		t.Fatal(err)
	}
}

func TestBuildNotificationTextRestoresPeerHeadersForOutput(t *testing.T) {
	text := buildNotificationText(
		"collab",
		[]wakeMsgInfo{{from: "codex", subject: "review ready"}},
		48,
	)
	for _, want := range []string{
		"AMQ [collab]",
		"message from codex",
		"review ready",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("notification output %q missing %q", text, want)
		}
	}
}

const injectViaHelperEnv = "AMQ_TEST_INJECT_VIA_HELPER"

func injectViaCaptureConfig(t *testing.T, fixedArgs ...string) (*wakeConfig, string) {
	t.Helper()

	t.Setenv(injectViaHelperEnv, "1")
	outputPath := filepath.Join(secureTempDirForTest(t), "inject output.txt")
	args := []string{"-test.run=^TestInjectViaHelperProcess$", "--", outputPath}
	args = append(args, fixedArgs...)

	return &wakeConfig{
		injectVia:     copyTestBinaryForInjectVia(t),
		injectArgs:    args,
		injectTimeout: 60 * time.Second,
		debug:         false,
	}, outputPath
}

func copyTestBinaryForInjectVia(t *testing.T) string {
	t.Helper()

	path := filepath.Join(secureTempDirForTest(t), "inject-via-helper")
	copyTestAMQ(t, path)
	return path
}

// TestDeliverWithNotificationLedgerDoesNotBlockOnWriteFailure pins the single
// most important invariant of the notification-attempt ledger: a wake delivers
// the notification EVEN WHEN the ledger write fails. The ledger must never
// block a doorbell. This is the wake-layer assertion that was lost when the
// obsolete TestNotificationAttemptLoggingIsBestEffort was removed (it called
// the deleted attemptNotification symbol); the package-level Writer test
// (TestPrepareWriteFailureDoesNotBlockAndResultIsRecordable) proves the Writer
// degrades gracefully, but only this test exercises the wake wiring
// (deliverWithNotificationLedger -> deliverNewMessageNotification) and can
// catch a future refactor that returns early on prepareErr before delivery.
//
// We force the ledger Prepare to fail by setting cfg.me to a handle
// ValidateHandle rejects (contains ".."), then call deliverWithNotificationLedger
// directly and assert: (1) the external notifier still produced its output
// (delivery happened), and (2) the returned error is the delivery error (nil
// here, since the external notifier succeeds), NOT the ledger prepareErr — a
// regression that leaked the ledger error through the return would surface as
// a non-nil error mentioning "notification attempt agent".
func TestDeliverWithNotificationLedgerDoesNotBlockOnWriteFailure(t *testing.T) {
	cfg, outputPath := injectViaCaptureConfig(t, "doorbell")
	// A root must exist so EnsureAgentDirs can create the receipts dir; the
	// ledger failure comes from the invalid agent handle, not a missing root.
	cfg.root = t.TempDir()
	// Force Prepare to fail: ValidateHandle rejects handles containing ".." or
	// "/", so the Writer's Prepare returns a non-nil prepareErr before any
	// record is persisted. Delivery must proceed anyway.
	cfg.me = "../invalid-ledger-owner"
	// inject-via delivery requires a non-none inject mode; raw is the simplest.
	cfg.injectMode = wakeInjectModeRaw

	notice := peerWakeNotification("doorbell")
	messageIDs := []string{"msg-best-effort"}
	// deliverNewMessageNotification gates on doorbell.plan(now, currentPending).attempt:
	// an empty map returns attempt=false and the function returns before delivery.
	// A non-empty map with a zero-value doorbell state arms and attempts. The
	// FileInfo may be nil (snapshotWakeFileIdentities accepts nil), so a bare
	// key is enough to make delivery proceed.
	currentPending := map[string]os.FileInfo{"msg-best-effort.md": nil}

	deliverErr := deliverWithNotificationLedger(cfg, messageIDs, notice, false, currentPending)

	// (1) Delivery happened despite the ledger write failure: the external
	// notifier wrote its payload to the capture file. If the ledger error had
	// blocked delivery, this file would not exist (or be empty).
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("delivery did not happen on ledger write failure — notifier output not written: %v", err)
	}
	if !strings.Contains(string(got), "doorbell") {
		t.Fatalf("notifier output = %q, want it to contain %q (delivery happened but payload wrong)", got, "doorbell")
	}

	// (2) The return is the DELIVERY error, not the ledger prepareErr. The
	// external notifier succeeded, so deliverErr must be nil. If a future
	// refactor leaked prepareErr through the return, deliverErr would be
	// non-nil and mention "notification attempt agent" (the ValidateHandle
	// error string from Prepare).
	if deliverErr != nil {
		t.Fatalf("deliverWithNotificationLedger returned the ledger error instead of the delivery error: got %v (delivery succeeded, so this should be nil)", deliverErr)
	}

	// (3) No journal file should exist — Prepare failed before any write. This
	// confirms the failure was the intended ledger-write path, not a silent
	// skip, and that trace would surface StateWriteFailed rather than a bogus
	// "written" record.
	journalPath := filepath.Join(cfg.root, "agents", cfg.me, "receipts", notificationattempt.LogFilename)
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("ledger journal should not exist after Prepare failure, got err: %v", err)
	}
}

func TestInjectNotificationNoneWritesOutputWithoutTIOCSTI(t *testing.T) {
	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		injected = append(injected, text)
		return errors.New("none mode must not inject")
	})

	stderr := captureWakeStderr(t, func() {
		cfg := &wakeConfig{
			injectMode:     wakeInjectModeNone,
			bell:           true,
			attentionEnv:   func(string) string { return "" },
			attentionIsTTY: func() bool { return true },
		}
		if err := injectNotification(cfg, "safe notice", true); err != nil {
			t.Fatalf("injectNotification: %v", err)
		}
	})

	if len(injected) != 0 {
		t.Fatalf("none mode injected terminal input: %q", injected)
	}
	if stderr != "\x1b]0;AMQ attention\a\asafe notice\n" {
		t.Fatalf("stderr = %q, want title + bell + notice", stderr)
	}
}

func TestOwnerBoundNotificationUsesFixedDoorbellAndGuardsEveryChunk(t *testing.T) {
	oldWait := waitForRawInputDrained
	oldSleep := rawInjectSleep
	waitForRawInputDrained = func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	}
	rawInjectSleep = func(time.Duration) {}
	t.Cleanup(func() {
		waitForRawInputDrained = oldWait
		rawInjectSleep = oldSleep
	})

	tests := []struct {
		name string
		me   string
		mode string
		want []string
	}{
		{
			name: "codex-raw",
			me:   "codex",
			mode: wakeInjectModeRaw,
			want: []string{coopWakeDoorbell, "\n", "\r", "\r"},
		},
		{
			name: "claude-raw",
			me:   "claude",
			mode: wakeInjectModeRaw,
			want: []string{coopWakeDoorbell, "\r", "\r"},
		},
		{
			name: "grok-paste",
			me:   "grok",
			mode: wakeInjectModePaste,
			want: []string{coopWakeDoorbell, "\r"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stop := make(chan struct{})
			var writes []string
			guardCalls := 0
			cfg := &wakeConfig{
				me:          tc.me,
				injectMode:  tc.mode,
				controlStop: stop,
				bell:        true,
				beforeTerminalWrite: func() error {
					guardCalls++
					return nil
				},
				terminalWrite: func(chunk string) error {
					writes = append(writes, chunk)
					return nil
				},
			}

			dynamic := "session=$(touch /tmp/pwned) subject=\x1b[31m body=untrusted"
			if err := injectNotification(cfg, dynamic, false); err != nil {
				t.Fatal(err)
			}
			if strings.Join(writes, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("owner-bound chunks = %#v, want %#v", writes, tc.want)
			}
			if guardCalls != len(tc.want) {
				t.Fatalf("guard calls = %d, want %d", guardCalls, len(tc.want))
			}
		})
	}
}

func captureWakeStderr(t *testing.T, fn func()) string {
	t.Helper()

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = oldStderr
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close stderr reader: %v", err)
	}
	return string(out)
}

func stubTIOCSTIInject(t *testing.T, fn func(string) error) {
	t.Helper()
	old := tiocstiInject
	tiocstiInject = fn
	t.Cleanup(func() {
		tiocstiInject = old
	})
}

func stubRawInputDrained(t *testing.T, fn func(time.Duration, time.Duration) (time.Duration, bool, error)) {
	t.Helper()
	old := waitForRawInputDrained
	waitForRawInputDrained = fn
	t.Cleanup(func() {
		waitForRawInputDrained = old
	})
}

func stubRawInjectSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	old := rawInjectSleep
	rawInjectSleep = func(d time.Duration) {
		slept = append(slept, d)
	}
	t.Cleanup(func() {
		rawInjectSleep = old
	})
	return &slept
}

func TestInjectNotificationRawInjectsLFPreludeForCodex(t *testing.T) {
	// codex targets get a lone LF between the drained text and the settle: it
	// routes through codex-tui's Ctrl-J binding, which flushes and clears
	// paste-burst state before the \r submit. In the reproduced Ghostty +
	// kitty-enhanced path a bare \r did not submit without it.
	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		injected = append(injected, text)
		return nil
	})
	stubRawInputDrained(t, func(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
		return time.Millisecond, true, nil
	})
	slept := stubRawInjectSleep(t)

	cfg := &wakeConfig{injectMode: "raw", me: "codex"}
	if err := injectNotification(cfg, "AMQ wake", true); err != nil {
		t.Fatalf("injectNotification: %v", err)
	}

	if got := strings.Join(injected, "|"); got != "AMQ wake|\n|\r|\r" {
		t.Fatalf("raw injection sequence = %q, want text, LF prelude, CR, rescue CR", got)
	}
	if len(*slept) != 2 {
		t.Fatalf("settle sleeps = %v, want two settle delays", *slept)
	}
}

func TestInjectNotificationUnsupportedDegradesOnceAndPersistsReason(t *testing.T) {
	writes := 0
	var statuses []struct {
		status string
		mode   string
		reason string
	}
	cfg := &wakeConfig{
		injectMode:   wakeInjectModeRaw,
		fallbackWarn: true,
		terminalWrite: func(string) error {
			writes++
			return newWakeInjectorUnsupportedError(syscall.EIO)
		},
		recordNotifierStatus: func(status, mode, reason string) error {
			statuses = append(statuses, struct {
				status string
				mode   string
				reason string
			}{status: status, mode: mode, reason: reason})
			return nil
		},
	}

	first := captureWakeStderr(t, func() {
		if err := injectNotification(cfg, "first doorbell", true); err != nil {
			t.Fatalf("first injectNotification: %v", err)
		}
	})
	if cfg.injectMode != wakeInjectModeNone {
		t.Fatalf("inject mode = %q, want degraded non-input", cfg.injectMode)
	}
	if writes != 1 {
		t.Fatalf("terminal writes = %d, want 1", writes)
	}
	if len(statuses) != 1 ||
		statuses[0].status != wakeInjectorUnsupportedStatus ||
		statuses[0].mode != wakeInjectModeRaw ||
		!strings.Contains(statuses[0].reason, "--inject-via") {
		t.Fatalf("recorded statuses = %#v", statuses)
	}
	if count := strings.Count(first, "warning:"); count != 1 {
		t.Fatalf("first warning count = %d, want 1:\n%s", count, first)
	}
	if !strings.Contains(first, "first doorbell") {
		t.Fatalf("first fallback missing doorbell:\n%s", first)
	}

	second := captureWakeStderr(t, func() {
		if err := injectNotification(cfg, "second doorbell", true); err != nil {
			t.Fatalf("second injectNotification: %v", err)
		}
	})
	if writes != 1 {
		t.Fatalf("degraded notifier retried terminal injection: writes=%d", writes)
	}
	if strings.Contains(second, "warning:") || !strings.Contains(second, "second doorbell") {
		t.Fatalf("second non-input output = %q", second)
	}
}

func TestInjectNotificationRawNeverInjectsEscapeBytes(t *testing.T) {
	// TIOCSTI delivers one byte per ioctl, so a multi-byte escape sequence can
	// be split by reader scheduling: a reader that sees a lone ESC parses the
	// Escape key, which cancels an active codex turn. Raw-mode injection must
	// therefore never contain ESC for any target.
	for _, me := range []string{"", "claude", "codex", "codex-test"} {
		var injected []string
		stubTIOCSTIInject(t, func(text string) error {
			injected = append(injected, text)
			return nil
		})
		stubRawInputDrained(t, func(timeout time.Duration, pollInterval time.Duration) (time.Duration, bool, error) {
			return time.Millisecond, true, nil
		})
		stubRawInjectSleep(t)

		cfg := &wakeConfig{injectMode: "raw", me: me}
		if err := injectNotification(cfg, "AMQ wake", true); err != nil {
			t.Fatalf("injectNotification(me=%q): %v", me, err)
		}
		for _, chunk := range injected {
			if strings.Contains(chunk, "\x1b") {
				t.Fatalf("me=%q injected chunk %q contains ESC", me, chunk)
			}
		}
	}
}

func TestNotifyNewMessagesNormalSuccessUsesFixedInputOnly(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-normal-input",
			From:    "untrusted-sender",
			To:      []string{"alice"},
			Thread:  "p2p/alice__untrusted-sender",
			Subject: "untrusted-subject",
			Created: "2026-07-27T05:10:00Z",
		},
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, root, "alice", "msg-normal-input.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		injected = append(injected, text)
		return nil
	})
	cfg := &wakeConfig{
		me:         "alice",
		root:       root,
		injectMode: wakeInjectModeRaw,
		previewLen: 48,
	}
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notifyNewMessages: %v", err)
	}

	got := strings.Join(injected, "|")
	if !strings.Contains(got, coopWakeDoorbell) {
		t.Fatalf("injected bytes = %q, missing fixed doorbell", got)
	}
	for _, forbidden := range []string{"untrusted-sender", "untrusted-subject"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("peer header %q entered terminal input: %q", forbidden, got)
		}
	}
}

func TestNotifyNewMessagesCoalescesAdditionsUntilRetryOrProgress(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	writeMessage := func(filename, id, subject string) {
		t.Helper()
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      id,
				From:    "peer",
				To:      []string{"codex"},
				Thread:  "p2p/codex__peer",
				Subject: subject,
				Created: "2026-07-28T15:00:00Z",
			},
			Body: "body",
		}
		data, err := msg.Marshal()
		if err != nil {
			t.Fatalf("marshal %s: %v", id, err)
		}
		if _, err := deliverToInboxForTest(t, root, "codex", filename, data); err != nil {
			t.Fatalf("deliver %s: %v", id, err)
		}
	}

	stubRawInputDrained(t, func(time.Duration, time.Duration) (time.Duration, bool, error) {
		return 0, true, nil
	})
	stubRawInjectSleep(t)
	var injected []string
	now := time.Unix(1_800_000_000, 0)
	cfg := &wakeConfig{
		me:             "codex",
		root:           root,
		session:        "session3",
		wakeOwner:      &wakeOwner{},
		injectMode:     wakeInjectModeRaw,
		doorbellNow:    func() time.Time { return now },
		attentionIsTTY: func() bool { return false },
		terminalWrite: func(text string) error {
			injected = append(injected, text)
			return nil
		},
	}

	writeMessage("first.md", "first", "first review")
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify first message: %v", err)
	}
	if got := strings.Join(injected, "|"); got != coopWakeDoorbell+"|\n|\r|\r" {
		t.Fatalf("first raw injection = %q, want one fixed doorbell submission", got)
	}
	injected = nil

	writeMessage("second.md", "second", "second review")
	stderr := captureWakeStderr(t, func() {
		if err := notifyNewMessages(cfg); err != nil {
			t.Fatalf("notify second message: %v", err)
		}
	})
	if len(injected) != 0 {
		t.Fatalf("second pending message injected another user turn: %q", injected)
	}
	if stderr != "" {
		t.Fatalf("pending generation emitted output-only reminder: %q", stderr)
	}

	now = now.Add(wakeDoorbellRetryBase)
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify at consolidated retry deadline: %v", err)
	}
	if got := strings.Join(injected, "|"); got != coopWakeDoorbell+"|\n|\r|\r" {
		t.Fatalf("consolidated raw injection = %q, want one fixed doorbell", got)
	}
	injected = nil

	if drained := runDrainJSON(t, root, "codex", 1, false); drained.Count != 1 {
		t.Fatalf("drained count = %d, want 1", drained.Count)
	}
	writeMessage("third.md", "third", "third review")
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify after drain: %v", err)
	}
	if got := strings.Join(injected, "|"); got != coopWakeDoorbell+"|\n|\r|\r" {
		t.Fatalf("post-drain raw injection = %q, want a fresh fixed doorbell submission", got)
	}
	injected = nil

	writeMessage("fourth.md", "fourth", "fourth review")
	stderr = captureWakeStderr(t, func() {
		if err := notifyNewMessages(cfg); err != nil {
			t.Fatalf("notify after re-armed doorbell: %v", err)
		}
	})
	if len(injected) != 0 {
		t.Fatalf("fourth pending message injected another user turn: %q", injected)
	}
	if stderr != "" {
		t.Fatalf("re-armed generation emitted output-only reminder: %q", stderr)
	}
}

func TestNotifyNewMessagesReminderUsesSubmitOnlyAfterConfirmedPresentation(t *testing.T) {
	testCases := []struct {
		name                   string
		mode                   string
		queueFirstSubmit       bool
		wantConfirmedInitially bool
		wantInjected           string
	}{
		{
			name:                   "raw-confirmed",
			mode:                   wakeInjectModeRaw,
			wantConfirmedInitially: true,
			wantInjected:           coopWakeDoorbell + "\n\r\r\r\r",
		},
		{
			name:                   "paste-confirmed",
			mode:                   wakeInjectModePaste,
			wantConfirmedInitially: true,
			wantInjected:           coopWakeDoorbell + "\r\r",
		},
		{
			name:             "raw-first-submit-queued",
			mode:             wakeInjectModeRaw,
			queueFirstSubmit: true,
			wantInjected:     coopWakeDoorbell + "\n\r\r",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			if err := fsq.EnsureRootDirs(root); err != nil {
				t.Fatal(err)
			}
			if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
				t.Fatal(err)
			}
			msg := format.Message{
				Header: format.Header{
					Schema:  1,
					ID:      "unchanged-reminder-" + tc.name,
					From:    "peer",
					To:      []string{"codex"},
					Thread:  "p2p/codex__peer",
					Subject: "unchanged reminder",
					Created: "2026-08-05T19:00:00Z",
				},
			}
			data, err := msg.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := deliverToInboxForTest(t, root, "codex", "unchanged.md", data); err != nil {
				t.Fatal(err)
			}

			drainCall := 0
			stubRawInputDrained(t, func(timeout time.Duration, _ time.Duration) (time.Duration, bool, error) {
				drainCall++
				if tc.queueFirstSubmit && drainCall == 2 {
					return timeout, false, nil
				}
				return 0, true, nil
			})
			stubRawInjectSleep(t)
			var injected strings.Builder
			now := time.Unix(1_800_000_000, 0)
			cfg := &wakeConfig{
				me:          "codex",
				root:        root,
				session:     "session1",
				wakeOwner:   &wakeOwner{},
				injectMode:  tc.mode,
				doorbellNow: func() time.Time { return now },
				terminalWrite: func(text string) error {
					injected.WriteString(text)
					return nil
				},
			}

			if err := notifyNewMessages(cfg); err != nil {
				t.Fatalf("initial presentation: %v", err)
			}
			if got := cfg.doorbell.presentationConfirmed; got != tc.wantConfirmedInitially {
				t.Fatalf("initial presentation confirmed = %t, want %t", got, tc.wantConfirmedInitially)
			}
			now = cfg.doorbell.nextAttempt
			if err := notifyNewMessages(cfg); err != nil {
				t.Fatalf("next attempt: %v", err)
			}
			if cfg.inputRecoveryRequired {
				t.Fatal("ordinary queued-suffix resume entered input recovery")
			}
			if !cfg.doorbell.presentationConfirmed {
				t.Fatal("completed full presentation was not marked confirmed")
			}

			got := injected.String()
			if count := strings.Count(got, coopWakeDoorbell); count != 1 {
				t.Fatalf("payload occurrences = %d, want exactly one across presentation and reminder: %q", count, got)
			}
			if got != tc.wantInjected {
				t.Fatalf("presentation sequence = %q, want %q", got, tc.wantInjected)
			}
		})
	}
}

func TestNotifyNewMessagesInjectedPolicyAcknowledgesSuccessfulInjectVia(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	writeMessage := func(filename, id string) {
		t.Helper()
		msg := format.Message{Header: format.Header{
			Schema: 1, ID: id, From: "peer", To: []string{"codex"},
			Thread: "p2p/codex__peer", Subject: id,
			Created: "2026-08-03T08:00:00Z",
		}}
		data, err := msg.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := deliverToInboxForTest(t, root, "codex", filename, data); err != nil {
			t.Fatal(err)
		}
	}

	working, outputPath := injectViaCaptureConfig(t)
	now := time.Unix(1_800_000_000, 0)
	cfg := &wakeConfig{
		me: "codex", root: root, session: "session1", wakeOwner: &wakeOwner{},
		injectVia: working.injectVia, injectArgs: working.injectArgs,
		injectTimeout: working.injectTimeout, retryUntil: wakeRetryUntilInjected,
		doorbellNow: func() time.Time { return now },
	}

	writeMessage("first.md", "first")
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("first notify: %v", err)
	}
	if got, err := os.ReadFile(outputPath); err != nil || string(got) != coopWakeDoorbell {
		t.Fatalf("first injection = %q, err=%v", got, err)
	}
	if err := os.WriteFile(outputPath, []byte("not-called-again"), 0o600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * wakeDoorbellRetryMax)
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("unchanged notify: %v", err)
	}
	if got, err := os.ReadFile(outputPath); err != nil || string(got) != "not-called-again" {
		t.Fatalf("unchanged cohort reinjected = %q, err=%v", got, err)
	}

	writeMessage("second.md", "second")
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("expanded notify: %v", err)
	}
	if got, err := os.ReadFile(outputPath); err != nil || string(got) != coopWakeDoorbell {
		t.Fatalf("expanded cohort injection = %q, err=%v", got, err)
	}
}

func TestNotifyNewMessagesCustomInterruptUsesSanitizedOperatorPayloadForInputAndFallback(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	msg := format.Message{
		Header: format.Header{
			Schema:   1,
			ID:       "msg-custom-interrupt",
			From:     "peer",
			To:       []string{"alice"},
			Thread:   "p2p/alice__peer",
			Subject:  "peer subject",
			Created:  "2026-07-27T05:20:00Z",
			Priority: "urgent",
			Labels:   []string{"interrupt"},
		},
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, root, "alice", "msg-custom-interrupt.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	fail := false
	var injected []string
	stubTIOCSTIInject(t, func(text string) error {
		if fail {
			return newWakeInjectorUnsupportedError(errors.New("injection unavailable"))
		}
		injected = append(injected, text)
		return nil
	})
	var emission wakeAttentionEmission
	cfg := &wakeConfig{
		me:                "alice",
		root:              root,
		injectMode:        wakeInjectModeRaw,
		interrupt:         true,
		interruptPriority: "urgent",
		interruptLabel:    "interrupt",
		interruptNotice:   "operator\x1b[31m\nnotice",
		attentionIsTTY:    func() bool { return false },
		recordAttention: func(got wakeAttentionEmission) error {
			emission = got
			return nil
		},
	}

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("successful notifyNewMessages: %v", err)
	}
	safeNotice := sanitizeForTTY(cfg.interruptNotice)
	if got := strings.Join(injected, "|"); !strings.Contains(got, safeNotice) {
		t.Fatalf("injected bytes = %q, want sanitized operator notice %q", got, safeNotice)
	}
	fail = true
	path := filepath.Join(fsq.AgentInboxNew(root, "alice"), "msg-custom-interrupt.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stderr := captureWakeStderr(t, func() {
		if err := notifyNewMessages(cfg); err != nil {
			t.Fatalf("fallback notifyNewMessages: %v", err)
		}
	})
	if !strings.Contains(stderr, safeNotice) || strings.Contains(stderr, "peer subject") {
		t.Fatalf("fallback output = %q, want only sanitized operator notice", stderr)
	}
	if emission.OutputProvenance != wakePayloadOperatorFlag {
		t.Fatalf("fallback provenance = %q, want operator", emission.OutputProvenance)
	}
}

func TestNotifyNewMessages_InjectViaInjectCmdPayload(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	logPath := filepath.Join(root, "inject.log")
	scriptPath := filepath.Join(root, "inject.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nprintf '%s' \"$1\" > "+logPath+"\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	msg := format.Message{
		Header: format.Header{
			Schema:  1,
			ID:      "msg-normal",
			From:    "codex",
			To:      []string{"alice"},
			Thread:  "p2p/alice__codex",
			Subject: "normal",
			Created: "2026-04-25T10:00:00Z",
		},
		Body: "normal body",
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, root, "alice", "msg-normal.md", data); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	cfg := &wakeConfig{
		me:         "alice",
		root:       root,
		injectVia:  scriptPath,
		injectCmd:  "amq drain\x1b[31m\n--include-body",
		previewLen: 48,
	}

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notifyNewMessages: %v", err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	expected := "\namq drain [31m --include-body\n"
	if string(got) != expected {
		t.Fatalf("expected inject-cmd payload %q, got %q", expected, string(got))
	}
}

func TestNotifyNewMessagesSkipsBaselineWithoutDraining(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	writeMessage := func(filename, id, subject string) {
		t.Helper()
		msg := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      id,
				From:    "codex",
				To:      []string{"alice"},
				Thread:  "p2p/alice__codex",
				Subject: subject,
				Created: "2026-07-22T00:00:00Z",
			},
			Body: "body",
		}
		data, err := msg.Marshal()
		if err != nil {
			t.Fatalf("marshal %s: %v", id, err)
		}
		if _, err := deliverToInboxForTest(t, root, "alice", filename, data); err != nil {
			t.Fatalf("deliver %s: %v", id, err)
		}
	}

	writeMessage("stale.md", "stale", "stale subject")
	cfg, outputPath := injectViaCaptureConfig(t)
	cfg.me = "alice"
	cfg.root = root
	cfg.previewLen = 48
	staleInfo, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "stale.md"))
	if err != nil {
		t.Fatalf("stat stale baseline message: %v", err)
	}
	staleIdentity, ok := captureWakeFileIdentity(staleInfo)
	if !ok {
		t.Fatal("capture stale baseline identity")
	}
	cfg.baselineExisting = map[string]wakeFileIdentity{"stale.md": staleIdentity}

	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify stale baseline: %v", err)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("baseline message triggered injection; output stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), "stale.md")); err != nil {
		t.Fatalf("baseline message was moved or removed: %v", err)
	}
	receipts, err := os.ReadDir(fsq.AgentReceipts(root, "alice"))
	if err != nil {
		t.Fatalf("read receipts: %v", err)
	}
	if len(receipts) != 0 {
		t.Fatalf("wake created receipts for an unread baseline: %v", receipts)
	}

	writeMessage("fresh.md", "fresh", "fresh subject")
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify fresh message: %v", err)
	}
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read injection output: %v", err)
	}
	if string(got) != coopWakeDoorbell {
		t.Fatalf("injected payload = %q, want fixed doorbell %q", string(got), coopWakeDoorbell)
	}
}

func TestWaitForInputQuietDemotesWhenActiveThroughMaxHold(t *testing.T) {
	now := time.Date(2026, 7, 26, 18, 0, 0, 0, time.UTC)
	var sleeps []time.Duration
	sampleCalls := 0

	allowInjection, reason, err := waitForInputQuiet(
		func() (ttyInputState, error) {
			sampleCalls++
			return ttyInputState{pendingBytes: 1}, nil
		},
		func() time.Time {
			return now
		},
		func(delay time.Duration, _ ttyInputState, _ string) {
			sleeps = append(sleeps, delay)
			now = now.Add(delay)
		},
		time.Second,
		25*time.Millisecond,
		10*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	if allowInjection || reason != "pending terminal input" {
		t.Fatalf("max-hold result allow=%v reason=%q", allowInjection, reason)
	}
	if sampleCalls != 4 {
		t.Fatalf("sample calls = %d, want 4", sampleCalls)
	}
	if len(sleeps) != 3 ||
		sleeps[0] != 10*time.Millisecond ||
		sleeps[1] != 10*time.Millisecond ||
		sleeps[2] != 5*time.Millisecond {
		t.Fatalf("sleeps = %v, want [10ms 10ms 5ms]", sleeps)
	}
}
