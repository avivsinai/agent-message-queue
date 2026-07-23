package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

type wakeConfig struct {
	me                string
	root              string
	session           string
	injectCmd         string
	injectVia         string // external command for injection (replaces TIOCSTI)
	injectArgs        []string
	wakeOwner         *wakeOwner
	injectTimeout     time.Duration
	bell              bool
	debounce          time.Duration
	previewLen        int
	strict            bool
	fallbackWarn      bool
	injectMode        string // auto, raw, paste
	debug             bool
	deferWhileInput   bool
	inputQuietFor     time.Duration
	inputPollInterval time.Duration
	inputMaxHold      time.Duration
	interrupt         bool
	interruptLabel    string
	interruptPriority string
	interruptKey      string
	interruptNotice   string
	interruptCooldown time.Duration
	lastInterrupt     time.Time
	controlStop       <-chan struct{}
	baseline          *wakeBaseline
	seen              map[string]struct{}
	readyFile         string
	readyInspection   wakeLockInspection
	baselineRequested bool
	baselineExisting  map[string]wakeFileIdentity
	onPrepared        func() error
}

const defaultInjectTimeout = 5 * time.Second
const (
	wakeInjectModeAuto  = "auto"
	wakeInjectModeRaw   = "raw"
	wakeInjectModePaste = "paste"
	wakeInjectModeNone  = "none"

	rawInjectDrainTimeout      = 2 * time.Second
	rawInjectDrainPollInterval = 10 * time.Millisecond
	// rawInjectCRDrainTimeout bounds the wait for the submit CR itself to be
	// consumed before deciding whether the second rescue CR is safe to send.
	rawInjectCRDrainTimeout = 1 * time.Second
	// codexTUIEnterSuppressWindow mirrors codex-tui's
	// PASTE_ENTER_SUPPRESS_WINDOW (codex-rs/tui/src/bottom_pane/paste_burst.rs,
	// verified at rust-v0.144.1 and main): an Enter arriving within this window
	// after the last rapid-input char is inserted as a pasted newline instead
	// of submitting, and RE-EXTENDS the window by the same amount. Re-pin this
	// value if upstream codex-tui changes.
	codexTUIEnterSuppressWindow = 120 * time.Millisecond
	// rawInjectSettleDelay holds the submit CR after the notification text has
	// drained. A drained queue only proves the TUI read the bytes, not that its
	// paste-burst window expired: fast readers (codex-tui) consume injected
	// bytes within microseconds, and a CR landing inside the suppress window is
	// swallowed. The settle must clear the window with margin for scheduler and
	// timer jitter; the rescue CR uses the same spacing because a swallowed
	// Enter re-extends the window. Claude Code's Ink fork has no timing
	// heuristic (bracketed-paste markers only) and accepts any delay.
	rawInjectSettleDelay = codexTUIEnterSuppressWindow + 30*time.Millisecond
)

var (
	tiocstiInject          = func(text string) error { return tiocsti.Inject(text) }
	waitForRawInputDrained = waitForTTYInputDrain
	rawInjectSleep         = time.Sleep
)

type wakeMsgInfo struct {
	id       string
	filename string
	from     string
	subject  string
	priority string
	labels   []string
}

type ttyInputState struct {
	pendingBytes int
	lastRead     time.Time
	hasLastRead  bool
}

func (s ttyInputState) active(now time.Time, quietFor time.Duration) (bool, string) {
	if s.pendingBytes > 0 {
		return true, "pending terminal input"
	}
	if quietFor <= 0 || !s.hasLastRead {
		return false, ""
	}
	age := now.Sub(s.lastRead)
	if age < 0 || age < quietFor {
		return true, "recent terminal input"
	}
	return false, ""
}

func inputDeferralDelay(state ttyInputState, now, deadline time.Time, quietFor, pollInterval time.Duration) time.Duration {
	delay := pollInterval
	if delay <= 0 {
		delay = 200 * time.Millisecond
	}

	if state.pendingBytes == 0 && state.hasLastRead && quietFor > 0 {
		untilQuiet := state.lastRead.Add(quietFor).Sub(now)
		if untilQuiet > 0 && untilQuiet < delay {
			delay = untilQuiet
		}
	}

	if remaining := deadline.Sub(now); remaining > 0 && remaining < delay {
		delay = remaining
	}
	if delay <= 0 {
		return 0
	}
	return delay
}

func shouldDeferBeforeInject(cfg *wakeConfig, deferForInput bool) bool {
	return deferForInput && cfg.deferWhileInput && cfg.injectVia == "" && cfg.injectMode != wakeInjectModeNone
}

func notifyNewMessages(cfg *wakeConfig) error {
	inboxNew := fsq.AgentInboxNew(cfg.root, cfg.me)

	entries, err := os.ReadDir(inboxNew)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var normalMessages []wakeMsgInfo
	normalCounts := make(map[string]int)
	var interruptMessages []wakeMsgInfo
	interruptCounts := make(map[string]int)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		if wakeMessageSeen(cfg, "", name) {
			continue
		}
		if baselineIdentity, ignored := cfg.baselineExisting[name]; ignored {
			info, infoErr := entry.Info()
			if infoErr == nil && matchesWakeFileIdentity(baselineIdentity, info) {
				continue
			}
			delete(cfg.baselineExisting, name)
		}

		path := filepath.Join(inboxNew, name)
		header, err := format.ReadHeaderFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			// Count corrupt messages too
			normalMessages = append(normalMessages, wakeMsgInfo{filename: name, from: "unknown", subject: "(parse error)"})
			normalCounts["unknown"]++
			continue
		}
		id := strings.TrimSpace(header.ID)
		if wakeMessageSeen(cfg, id, name) || hasValidDrainedReceipt(cfg.root, cfg.me, id) {
			continue
		}

		from := strings.TrimSpace(header.From)
		if from == "" {
			from = "unknown"
		}
		subject := strings.TrimSpace(header.Subject)
		subject = sanitizeForTTY(subject)
		from = sanitizeForTTY(from)
		priority := strings.TrimSpace(header.Priority)

		info := wakeMsgInfo{
			id:       id,
			filename: name,
			from:     from,
			subject:  subject,
			priority: priority,
			labels:   header.Labels,
		}

		if cfg.interrupt && isInterruptMessage(info, cfg) {
			interruptMessages = append(interruptMessages, info)
			interruptCounts[from]++
		} else {
			normalMessages = append(normalMessages, info)
			normalCounts[from]++
		}
	}

	if len(normalMessages) == 0 && len(interruptMessages) == 0 {
		return nil
	}

	if cfg.interrupt && len(interruptMessages) > 0 {
		interruptText := buildInterruptText(cfg.session, interruptMessages, interruptCounts, cfg.previewLen, cfg.interruptNotice)
		if cfg.injectMode == wakeInjectModeNone {
			writeWakeOutput(interruptText, true)
			markWakeMessagesSeen(cfg, interruptMessages)
		} else {
			now := time.Now()
			if cfg.interruptKey != "" && shouldInterruptNow(cfg, now) {
				if cfg.injectVia != "" {
					if err := injectVia(cfg, cfg.interruptKey); err == nil {
						cfg.lastInterrupt = now
						time.Sleep(50 * time.Millisecond)
					}
				} else if err := tiocsti.Inject(cfg.interruptKey); err == nil {
					cfg.lastInterrupt = now
					time.Sleep(50 * time.Millisecond)
				}
			}
			if err := injectNotification(cfg, interruptText, false); err != nil {
				return err
			}
			markWakeMessagesSeen(cfg, interruptMessages)
		}
	}

	if len(normalMessages) == 0 {
		return nil
	}

	// Build notification text
	var text string
	if cfg.injectCmd != "" {
		// Power user mode: inject actual command
		text = "\n" + cfg.injectCmd + "\n"
	} else {
		// Default: informational notice
		text = buildNotificationText(cfg.session, normalMessages, normalCounts, cfg.previewLen)
	}

	if err := injectNotification(cfg, text, true); err != nil {
		return err
	}
	markWakeMessagesSeen(cfg, normalMessages)
	return nil
}

func wakeMessageSeen(cfg *wakeConfig, id, filename string) bool {
	if sameWakeBaselineMessage(cfg.baseline, id, filename) {
		return true
	}
	if cfg.seen == nil {
		return false
	}
	if id != "" {
		if _, ok := cfg.seen["id:"+id]; ok {
			return true
		}
	}
	_, ok := cfg.seen["file:"+filename]
	return ok
}

func markWakeMessagesSeen(cfg *wakeConfig, messages []wakeMsgInfo) {
	if cfg.seen == nil {
		cfg.seen = make(map[string]struct{})
	}
	for _, message := range messages {
		if message.id != "" {
			cfg.seen["id:"+message.id] = struct{}{}
		}
		if message.filename != "" {
			cfg.seen["file:"+message.filename] = struct{}{}
		}
	}
}

func hasValidDrainedReceipt(root, me, id string) bool {
	if id == "" {
		return false
	}
	receipts, err := receipt.List(root, me, receipt.ListFilter{MsgID: id, Consumer: me, Stage: receipt.StageDrained})
	if err != nil {
		return false
	}
	for _, item := range receipts {
		if item.Schema == format.CurrentSchema && item.MsgID == id && item.Consumer == me && item.Stage == receipt.StageDrained {
			return true
		}
	}
	return false
}

func buildNotificationText(session string, messages []wakeMsgInfo, senderCounts map[string]int, previewLen int) string {
	count := len(messages)
	prefix := notificationPrefix("AMQ", session)

	if count == 1 {
		// Single message: show from + truncated subject
		msg := messages[0]
		subject := msg.subject
		if subject == "" {
			subject = "(no subject)"
		}
		subject = truncateSubject(subject, previewLen)
		return fmt.Sprintf("%s: message from %s - %s. Drain with: amq drain --include-body — then act on it", prefix, msg.from, subject)
	}

	// Multiple messages: show counts by sender
	var parts []string
	senders := make([]string, 0, len(senderCounts))
	for s := range senderCounts {
		senders = append(senders, s)
	}
	sort.Strings(senders)

	for _, sender := range senders {
		c := senderCounts[sender]
		parts = append(parts, fmt.Sprintf("%d from %s", c, sender))
	}

	return fmt.Sprintf("%s: %d messages - %s. Drain with: amq drain --include-body — then act on it",
		prefix, count, strings.Join(parts, ", "))
}

func buildInterruptText(session string, messages []wakeMsgInfo, senderCounts map[string]int, previewLen int, custom string) string {
	if custom != "" {
		return custom
	}

	count := len(messages)
	prefix := notificationPrefix("AMQ interrupt", session)

	if count == 1 {
		msg := messages[0]
		subject := msg.subject
		if subject == "" {
			subject = "(no subject)"
		}
		subject = truncateSubject(subject, previewLen)
		return fmt.Sprintf("%s: urgent message from %s - %s. Drain with: amq drain --include-body — then act on it",
			prefix, msg.from, subject)
	}

	var parts []string
	senders := make([]string, 0, len(senderCounts))
	for s := range senderCounts {
		senders = append(senders, s)
	}
	sort.Strings(senders)
	for _, sender := range senders {
		c := senderCounts[sender]
		parts = append(parts, fmt.Sprintf("%d from %s", c, sender))
	}
	return fmt.Sprintf("%s: %d urgent messages - %s. Drain with: amq drain --include-body — then act on it",
		prefix, count, strings.Join(parts, ", "))
}

// notificationPrefix builds "AMQ [session]" or just "AMQ" when session is empty.
func notificationPrefix(base, session string) string {
	if session == "" {
		return base
	}
	return fmt.Sprintf("%s [%s]", base, session)
}

func truncateSubject(subject string, previewLen int) string {
	if previewLen <= 0 {
		return ""
	}
	runes := []rune(subject)
	if len(runes) <= previewLen {
		return subject
	}
	if previewLen <= 3 {
		return string(runes[:previewLen])
	}
	return string(runes[:previewLen-3]) + "..."
}

func isInterruptMessage(info wakeMsgInfo, cfg *wakeConfig) bool {
	if !cfg.interrupt {
		return false
	}
	if cfg.interruptPriority != "" && info.priority != cfg.interruptPriority {
		return false
	}
	if cfg.interruptLabel == "" {
		return false
	}
	for _, label := range info.labels {
		if strings.TrimSpace(label) == cfg.interruptLabel {
			return true
		}
	}
	return false
}

func shouldInterruptNow(cfg *wakeConfig, now time.Time) bool {
	if cfg.interruptCooldown <= 0 {
		return true
	}
	return now.Sub(cfg.lastInterrupt) >= cfg.interruptCooldown
}

func effectiveInjectMode(cfg *wakeConfig) string {
	mode := cfg.injectMode
	if mode == "" || mode == wakeInjectModeAuto {
		// Auto-detect: use raw mode for Claude Code and Codex to avoid bracketed-paste
		// Enter swallowing in some CLIs. Paste mode remains available via flag.
		// Claude Code's Ink framework has buggy bracketed paste handling where CR gets
		// coalesced with the paste-end sequence and swallowed by the input parser.
		meLower := strings.ToLower(cfg.me)
		if strings.Contains(meLower, "claude") || strings.Contains(meLower, "codex") {
			mode = wakeInjectModeRaw
		} else {
			mode = wakeInjectModePaste
		}
	}
	return mode
}

func normalizeWakeInjectMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		mode = wakeInjectModeAuto
	}
	switch mode {
	case wakeInjectModeAuto, wakeInjectModeRaw, wakeInjectModePaste, wakeInjectModeNone:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid injection mode %q (supported: auto, raw, paste, none)", raw)
	}
}

func writeWakeOutput(text string, bell bool) {
	if bell {
		text = "\a" + text
	}
	_, _ = fmt.Fprint(os.Stderr, text+"\n")
}

func injectNotification(cfg *wakeConfig, text string, deferForInput bool) error {
	if cfg.injectMode == wakeInjectModeNone {
		writeWakeOutput(text, cfg.bell)
		return nil
	}

	// Keep plain text for stderr fallback
	plainText := text
	if cfg.bell {
		plainText = "\a" + plainText
	}

	if shouldDeferBeforeInject(cfg, deferForInput) {
		waitForTTYInputQuiet(cfg)
	}

	// External injection: delegate to user-specified command instead of TIOCSTI.
	// The command receives the notification text as its last argument.
	if cfg.injectVia != "" {
		if err := injectVia(cfg, plainText); err != nil {
			if cfg.fallbackWarn {
				_ = writeStderr("amq wake: --inject-via failed: %v\n", err)
				_ = writeStderr("amq wake: falling back to stderr notification\n")
				cfg.fallbackWarn = false
			}
			_, _ = fmt.Fprint(os.Stderr, plainText+"\n")
			// An external injector is commonly the only path back into a TUI.
			// Stderr may be detached or hidden, so it is a diagnostic fallback,
			// not a delivery acknowledgement. Keep the message retryable.
			return err
		}
		return nil
	}

	mode := effectiveInjectMode(cfg)
	if cfg.debug {
		_ = writeStderr("amq wake [debug]: mode=%s text_len=%d\n", mode, len(text))
	}
	var injectErr error
	switch mode {
	case wakeInjectModeRaw:
		// Raw mode: inject text and CR separately to avoid paste detection.
		// Ink treats multi-char input as paste, not keypresses. Sending text+CR
		// as one chunk makes Ink see pasted text, not an Enter keypress.
		injectedText := text
		if cfg.bell {
			injectedText = "\a" + injectedText
		}
		injectErr = injectRawNotification(cfg, injectedText)

	case wakeInjectModePaste:
		// Paste mode: bracketed paste with delayed CR
		// Works with crossterm/ratatui apps
		// Send paste content first, then CR after short delay to avoid coalescing
		pasteText := "\x1b[200~" + text + "\x1b[201~"
		if cfg.bell {
			pasteText = "\a" + pasteText
		}
		if err := tiocstiInject(pasteText); err != nil {
			injectErr = err
		} else {
			// Small delay to ensure CR lands in separate read cycle
			time.Sleep(25 * time.Millisecond)
			injectErr = tiocstiInject("\r")
		}

	default:
		// Unknown mode, fall back to raw
		injectedText := text + "\r"
		if cfg.bell {
			injectedText = "\a" + injectedText
		}
		injectErr = tiocstiInject(injectedText)
	}

	if injectErr != nil {
		if cfg.fallbackWarn {
			_ = writeStderr("amq wake: TIOCSTI injection failed: %v\n", injectErr)
			_ = writeStderr("amq wake: falling back to stderr notification\n")
			cfg.fallbackWarn = false
		}
		// Fallback: print plain text to stderr (no escape sequences)
		_, _ = fmt.Fprint(os.Stderr, plainText+"\n")
		return nil
	}

	return nil
}

// rawSubmitPrelude returns the bytes injected between the drained notification
// text and the settle delay. codex targets get a single LF: codex-tui maps a
// raw 0x0A to Ctrl-J, whose editor binding routes through handle_input_basic,
// which flushes and clears any active paste-burst state before inserting a
// newline (trailing whitespace is trimmed from the submitted payload). In the
// reproduced Ghostty + kitty-enhanced codex-tui wake path a raw \r alone did
// not submit at any tested delay; the LF prelude unlocks the later \r submit.
//
// Everything injected here must stay single-byte control characters. TIOCSTI
// delivers one byte per ioctl, so a multi-byte escape sequence (e.g. the kitty
// CSI-u Enter ESC[13u) can be split by reader scheduling — and a reader that
// sees a lone ESC parses the Escape key, which cancels an active codex turn
// and leaves the sequence tail as literal composer text.
func rawSubmitPrelude(me string) string {
	if strings.Contains(strings.ToLower(me), "codex") {
		return "\n"
	}
	return ""
}

func injectRawNotification(cfg *wakeConfig, injectedText string) error {
	if cfg.debug {
		_ = writeStderr("amq wake [debug]: injecting %d bytes of text\n", len(injectedText))
	}
	if err := tiocstiInject(injectedText); err != nil {
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: text inject failed: %v\n", err)
		}
		return err
	}
	prelude := rawSubmitPrelude(cfg.me)

	// The submit key must arrive in its own read() chunk; otherwise the TUI can
	// treat text+Enter as pasted input instead of a keypress. Waiting for the
	// text bytes to drain keeps the submit key out of a paste-shaped chunk even
	// when the reader stalls (#208).
	waited, drained, err := waitForRawInputDrained(rawInjectDrainTimeout, rawInjectDrainPollInterval)
	if cfg.debug {
		switch {
		case err != nil:
			_ = writeStderr("amq wake [debug]: input drain wait unavailable after %s: %v; continuing on timing alone\n", waited, err)
		case drained:
			_ = writeStderr("amq wake [debug]: input queue drained after %s\n", waited)
		default:
			_ = writeStderr("amq wake [debug]: input drain timeout after %s; injecting submit key anyway\n", waited)
		}
	}

	// Prelude (codex: a lone LF) clears the TUI's paste-burst state while the
	// injected text is fresh; its newline is trimmed from the submitted payload.
	if prelude != "" {
		if err := tiocstiInject(prelude); err != nil {
			if cfg.debug {
				_ = writeStderr("amq wake [debug]: prelude inject failed: %v\n", err)
			}
			return err
		}
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: prelude injected OK (%q)\n", prelude)
		}
	}

	// Hold the submit CR past the TUI's paste-burst window (see
	// rawInjectSettleDelay) so it is classified as a real Enter keypress, not a
	// pasted newline.
	rawInjectSleep(rawInjectSettleDelay)

	if err := tiocstiInject("\r"); err != nil {
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: submit key inject failed: %v\n", err)
		}
		return err
	}
	if cfg.debug {
		_ = writeStderr("amq wake [debug]: submit key injected OK\n")
	}

	// Rescue submit: if the first Enter was swallowed anyway (input buffer
	// flush or a burst-window race), a repeat Enter submits the composer; if
	// the first already submitted, Enter on an empty composer is a no-op. The
	// rescue must be spaced a full settle delay after the first: a swallowed
	// Enter re-extends codex-tui's 120ms suppress window, so a faster rescue
	// would be swallowed too. Skip the rescue only when the first submit key is
	// provably still queued — a second would coalesce with it into one
	// paste-shaped chunk and both would be swallowed.
	crWaited, crDrained, crErr := waitForRawInputDrained(rawInjectCRDrainTimeout, rawInjectDrainPollInterval)
	if crErr == nil && !crDrained {
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: submit key still queued after %s; skipping rescue submit\n", crWaited)
		}
		return nil
	}
	rawInjectSleep(rawInjectSettleDelay)
	if err := tiocstiInject("\r"); err != nil {
		// The text and first submit key were already delivered; the rescue is
		// best-effort.
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: rescue submit inject failed: %v\n", err)
		}
		return nil
	}
	if cfg.debug {
		_ = writeStderr("amq wake [debug]: rescue submit injected OK\n")
	}
	return nil
}

func waitForInputQueueDrain(
	samplePending func() (int, error),
	now func() time.Time,
	sleep func(time.Duration),
	timeout time.Duration,
	pollInterval time.Duration,
) (time.Duration, bool, error) {
	if pollInterval <= 0 {
		pollInterval = rawInjectDrainPollInterval
	}

	start := now()
	deadline := start.Add(timeout)
	for {
		pending, err := samplePending()
		current := now()
		elapsed := current.Sub(start)
		if err != nil {
			return elapsed, false, err
		}
		if pending <= 0 {
			return elapsed, true, nil
		}
		if timeout <= 0 || !current.Before(deadline) {
			return elapsed, false, nil
		}

		delay := pollInterval
		if remaining := deadline.Sub(current); remaining > 0 && remaining < delay {
			delay = remaining
		}
		if delay <= 0 {
			return elapsed, false, nil
		}
		sleep(delay)
	}
}

func injectVia(cfg *wakeConfig, text string) error {
	if cfg.debug {
		_ = writeStderr("amq wake [debug]: inject-via mode, running: %s %s <text>\n", cfg.injectVia, strings.Join(cfg.injectArgs, " "))
	}

	executable := strings.TrimSpace(cfg.injectVia)
	if executable == "" {
		return fmt.Errorf("inject-via command is blank")
	}
	if err := validateResolvedWakeInjectViaPath(executable); err != nil {
		return err
	}

	timeout := cfg.injectTimeout
	if timeout <= 0 {
		timeout = defaultInjectTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := append([]string{}, cfg.injectArgs...)
	args = append(args, text)
	cmd := exec.CommandContext(ctx, executable, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			if cfg.debug {
				_ = writeStderr("amq wake [debug]: inject-via timed out after %s (%s)\n", timeout, string(out))
			}
			return fmt.Errorf("inject-via timed out after %s", timeout)
		}
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: inject-via failed: %v (%s)\n", err, string(out))
		}
		return err
	}

	return nil
}

func sanitizeForTTY(s string) string {
	return strings.Map(func(r rune) rune {
		// Filter ASCII controls (0x00-0x1F), DEL (0x7F), and C1 controls (0x80-0x9F)
		// C1 range includes 0x9B which some terminals interpret as CSI
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
}
