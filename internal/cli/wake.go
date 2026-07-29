package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type wakeConfig struct {
	me                   string
	root                 string
	session              string
	injectCmd            string
	injectVia            string // external command for injection (replaces TIOCSTI)
	injectArgs           []string
	wakeOwner            *wakeOwner
	injectTimeout        time.Duration
	bell                 bool
	debounce             time.Duration
	previewLen           int
	strict               bool
	fallbackWarn         bool
	injectMode           string // auto, raw, paste
	debug                bool
	deferWhileInput      bool
	inputQuietFor        time.Duration
	inputPollInterval    time.Duration
	inputMaxHold         time.Duration
	interrupt            bool
	interruptLabel       string
	interruptPriority    string
	interruptKey         string
	interruptNotice      string
	interruptCooldown    time.Duration
	lastInterrupt        time.Time
	controlStop          <-chan struct{}
	beforeTerminalWrite  func() error
	terminalWrite        func(string) error
	terminalGeneration   string
	terminalTTY          string
	baselineRequested    bool
	baselineInherited    bool
	baselineExisting     map[string]wakeFileIdentity
	rawSubmitPhase       rawSubmitPhase
	rawSubmitPayload     string
	doorbell             wakeDoorbellState
	doorbellNow          func() time.Time
	newDoorbellToken     func() (string, error)
	promptObserved       <-chan string
	suppressAttention    bool
	onBaselineReady      func(map[string]wakeFileIdentity) error
	onPrepared           func(wakeAdmissionWatcher) error
	retainedInbox        wakeInboxReader
	touchPresence        func() error
	maintenanceTicks     <-chan time.Time
	preconditionCheck    func(*wakeConfig) error
	recordNotifierStatus func(status, mode, reason string) error
	recordAttention      func(wakeAttentionEmission) error
	attentionEnv         func(string) string
	attentionIsTTY       func() bool
	attentionWrite       func([]byte) (int, error)
}

type rawSubmitPhase uint8

const (
	rawSubmitIdle rawSubmitPhase = iota
	rawFirstSubmitQueued
	rawRescueSubmitQueued
)

type wakeAdmissionWatcher interface {
	Errors() <-chan error
}

type wakeInboxReader interface {
	ReadDir() ([]os.DirEntry, error)
	ReadHeader(name string) (format.Header, error)
}

const defaultInjectTimeout = 5 * time.Second
const (
	wakeInjectModeAuto  = "auto"
	wakeInjectModeRaw   = "raw"
	wakeInjectModePaste = "paste"
	wakeInjectModeNone  = "none"

	coopWakeDoorbellTokenForTests = "00000000000000000000000000000000"
	coopWakeDoorbellPrefix        = ": AMQ doorbell v1 token="
	coopWakeDoorbellSuffix        = " run amq drain --include-body then act on it"
	coopWakeDoorbell              = coopWakeDoorbellPrefix + coopWakeDoorbellTokenForTests + coopWakeDoorbellSuffix

	rawInjectDrainTimeout      = 2 * time.Second
	rawInjectDrainPollInterval = 10 * time.Millisecond
	// rawInjectCRDrainTimeout bounds the wait for the submit CR itself to be
	// consumed before deciding whether the second rescue CR is safe to send.
	rawInjectCRDrainTimeout = 1 * time.Second
	// Three samples require two follow-up confirmations after the first quiet
	// observation, while adding at most two poll intervals to a real idle
	// transition. Any active sample resets the evidence.
	requiredInputQuietSamples = 3
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
	waitForWakeInputQuiet  = waitForTTYInputQuiet
	rawInjectSleep         = time.Sleep
)

type wakeMsgInfo struct {
	from     string
	subject  string
	priority string
	labels   []string
}

type wakePayloadProvenance string

const (
	wakePayloadSystemFixed  wakePayloadProvenance = "system_fixed"
	wakePayloadPeerHeaders  wakePayloadProvenance = "peer_headers"
	wakePayloadOperatorFlag wakePayloadProvenance = "operator_flag"
)

type wakePayload struct {
	text       string
	provenance wakePayloadProvenance
}

type wakeNotification struct {
	input  wakePayload
	output wakePayload
}

func peerWakeNotification(output string) wakeNotification {
	return wakeNotification{
		input: wakePayload{
			text:       coopWakeDoorbell,
			provenance: wakePayloadSystemFixed,
		},
		output: wakePayload{
			text:       output,
			provenance: wakePayloadPeerHeaders,
		},
	}
}

func buildCoopWakeDoorbell(token string) string {
	return coopWakeDoorbellPrefix + token + coopWakeDoorbellSuffix
}

func parseCoopWakeDoorbell(prompt string) (string, bool) {
	if !strings.HasPrefix(prompt, coopWakeDoorbellPrefix) ||
		!strings.HasSuffix(prompt, coopWakeDoorbellSuffix) {
		return "", false
	}
	token := strings.TrimSuffix(strings.TrimPrefix(prompt, coopWakeDoorbellPrefix), coopWakeDoorbellSuffix)
	return token, validWakeDoorbellToken(token)
}

func operatorWakeNotification(text string) wakeNotification {
	payload := wakePayload{
		text:       text,
		provenance: wakePayloadOperatorFlag,
	}
	return wakeNotification{input: payload, output: payload}
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

func waitForInputQuiet(
	sample func() (ttyInputState, error),
	nowFn func() time.Time,
	sleepFn func(time.Duration, ttyInputState, string),
	quietFor, maxHold, pollInterval time.Duration,
) (allowInjection bool, activeReason string, err error) {
	if maxHold <= 0 {
		return true, "", nil
	}

	deadline := nowFn().Add(maxHold)
	quietSamples := 0
	for {
		now := nowFn()
		state, sampleErr := sample()
		if sampleErr != nil {
			return true, "", sampleErr
		}

		active, reason := state.active(now, quietFor)
		if active {
			quietSamples = 0
		} else {
			quietSamples++
			if quietSamples >= requiredInputQuietSamples {
				return true, "", nil
			}
			reason = "quiet confirmation incomplete"
		}
		if !now.Before(deadline) {
			return false, reason, nil
		}

		delay := inputDeferralDelay(state, now, deadline, quietFor, pollInterval)
		if delay <= 0 {
			return false, reason, nil
		}
		sleepFn(delay, state, reason)
	}
}

func shouldDeferBeforeInject(cfg *wakeConfig, deferForInput bool) bool {
	return deferForInput && cfg.deferWhileInput && cfg.injectVia == "" && cfg.injectMode != wakeInjectModeNone
}

func notifyNewMessages(cfg *wakeConfig) error {
	inboxNew := fsq.AgentInboxNew(cfg.root, cfg.me)

	var entries []os.DirEntry
	var err error
	if cfg.retainedInbox != nil {
		entries, err = cfg.retainedInbox.ReadDir()
	} else {
		entries, err = os.ReadDir(inboxNew)
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var messages []wakeMsgInfo
	var interruptMessages []wakeMsgInfo
	interruptCounts := make(map[string]int)
	currentPending := make(map[string]os.FileInfo)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		fileInfo, infoErr := entry.Info()
		if baselineIdentity, ignored := cfg.baselineExisting[name]; ignored {
			if infoErr == nil && matchesWakeFileIdentity(baselineIdentity, fileInfo) {
				continue
			}
			delete(cfg.baselineExisting, name)
		}

		var header format.Header
		if cfg.retainedInbox != nil {
			header, err = cfg.retainedInbox.ReadHeader(name)
		} else {
			header, err = format.ReadHeaderFile(filepath.Join(inboxNew, name))
		}
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			// Count corrupt messages too
			messages = append(messages, wakeMsgInfo{from: "unknown", subject: "(parse error)"})
			if infoErr == nil {
				currentPending[name] = fileInfo
			}
			continue
		}
		if infoErr == nil {
			currentPending[name] = fileInfo
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
			from:     from,
			subject:  subject,
			priority: priority,
			labels:   header.Labels,
		}

		messages = append(messages, info)

		if cfg.interrupt && isInterruptMessage(info, cfg) {
			interruptMessages = append(interruptMessages, info)
			interruptCounts[from]++
		}
	}

	if len(messages) == 0 {
		if usesCoopDoorbell(cfg) {
			if _, err := cfg.doorbell.plan(
				cfg.wakeDoorbellNow(),
				currentPending,
				cfg.wakeDoorbellToken,
			); err != nil {
				return err
			}
			if err := reconcileRawSubmitAfterInboxDrain(cfg); err != nil {
				return err
			}
		}
		return nil
	}

	if cfg.interrupt && len(interruptMessages) > 0 {
		interruptText := buildInterruptText(cfg.session, interruptMessages, interruptCounts, cfg.previewLen, cfg.interruptNotice)
		notice := peerWakeNotification(interruptText)
		if cfg.interruptNotice != "" {
			notice = operatorWakeNotification(interruptText)
		}
		if cfg.injectMode == wakeInjectModeNone {
			return deliverNewMessageNotification(cfg, notice, false, currentPending)
		}
		now := time.Now()
		if !usesCoopDoorbell(cfg) &&
			cfg.interruptKey != "" &&
			shouldInterruptNow(cfg, now) {
			if cfg.injectVia != "" {
				allowed, guardErr := authorizeTerminalWrite(cfg)
				if guardErr != nil {
					return guardErr
				}
				if allowed {
					// Exit zero is the strongest acceptance evidence the v0
					// external transport exposes. It cannot prove the provider
					// delivered the key, but advancing the cooldown bounds
					// duplicate control-key injection. A false-success provider
					// may therefore suppress one genuine interrupt until the
					// next window; stronger evidence needs a structured protocol.
					if err := injectVia(cfg, cfg.interruptKey); err == nil {
						cfg.lastInterrupt = now
						time.Sleep(50 * time.Millisecond)
					}
				}
			} else {
				wrote, writeErr := writeTerminalChunk(cfg, cfg.interruptKey)
				if writeErr != nil {
					var authorityErr *wakeTerminalAuthorityError
					if errors.As(writeErr, &authorityErr) {
						return writeErr
					}
				}
				// The raw path can require positive terminal-write evidence
				// instead of inferring acceptance from a process exit status.
				if writeErr == nil && wrote {
					cfg.lastInterrupt = now
					time.Sleep(50 * time.Millisecond)
				}
			}
		}
		return deliverNewMessageNotification(cfg, notice, false, currentPending)
	}

	var notice wakeNotification
	if cfg.injectCmd != "" {
		// Power user mode: inject the operator-authored command.
		text := "\n" + sanitizeForTTY(cfg.injectCmd) + "\n"
		notice = operatorWakeNotification(text)
	} else {
		notice = peerWakeNotification(
			buildNotificationText(cfg.session, messages, cfg.previewLen),
		)
	}

	return deliverNewMessageNotification(cfg, notice, true, currentPending)
}

func deliverNewMessageNotification(
	cfg *wakeConfig,
	notice wakeNotification,
	deferForInput bool,
	currentPending map[string]os.FileInfo,
) error {
	ownerBound := usesCoopDoorbell(cfg)
	if ownerBound && cfg.injectMode != wakeInjectModeNone {
		now := cfg.wakeDoorbellNow()
		plan, err := cfg.doorbell.plan(now, currentPending, cfg.wakeDoorbellToken)
		if err != nil {
			return err
		}
		if !plan.attempt {
			return nil
		}
		notice.input = wakePayload{
			text:       plan.prompt,
			provenance: wakePayloadSystemFixed,
		}
		cfg.suppressAttention = plan.retry
		deliveryErr := deliverWakeNotification(cfg, notice, deferForInput)
		cfg.suppressAttention = false
		if deliveryErr == nil || !isWakeTerminalForegroundPGRPChanged(deliveryErr) {
			cfg.doorbell.recordAttempt(now)
		}
		return deliveryErr
	}

	return deliverWakeNotification(cfg, notice, deferForInput)
}

func (cfg *wakeConfig) wakeDoorbellNow() time.Time {
	if cfg.doorbellNow != nil {
		return cfg.doorbellNow()
	}
	return time.Now()
}

func (cfg *wakeConfig) wakeDoorbellToken() (string, error) {
	if cfg.newDoorbellToken != nil {
		return cfg.newDoorbellToken()
	}
	// Internal direct-call tests do not carry a published wake generation.
	// Production wake instances always do and receive a fresh random token.
	if cfg.terminalGeneration == "" {
		return coopWakeDoorbellTokenForTests, nil
	}
	return generateWakeDoorbellToken()
}

func snapshotWakeFileIdentities(current map[string]os.FileInfo) map[string]wakeFileIdentity {
	snapshot := make(map[string]wakeFileIdentity, len(current))
	for name, info := range current {
		if identity, ok := captureWakeFileIdentity(info); ok {
			snapshot[name] = identity
		}
	}
	return snapshot
}

func buildNotificationText(session string, messages []wakeMsgInfo, previewLen int) string {
	count := len(messages)
	prefix := notificationPrefix("AMQ", session)
	if count == 1 {
		msg := messages[0]
		subject := msg.subject
		if subject == "" {
			subject = "(no subject)"
		}
		return fmt.Sprintf(
			"%s: message from %s - %s. Drain with: amq drain --include-body — then act on it",
			prefix,
			msg.from,
			truncateSubject(subject, previewLen),
		)
	}

	senderCounts := make(map[string]int)
	for _, msg := range messages {
		senderCounts[msg.from]++
	}
	senders := make([]string, 0, len(senderCounts))
	for sender := range senderCounts {
		senders = append(senders, sender)
	}
	sort.Strings(senders)
	parts := make([]string, 0, len(senders))
	for _, sender := range senders {
		parts = append(parts, fmt.Sprintf("%d from %s", senderCounts[sender], sender))
	}
	return fmt.Sprintf(
		"%s: %d messages - %s. Drain with: amq drain --include-body — then act on it",
		prefix,
		count,
		strings.Join(parts, ", "),
	)
}

func buildInterruptText(session string, messages []wakeMsgInfo, senderCounts map[string]int, previewLen int, custom string) string {
	if custom != "" {
		return sanitizeForTTY(custom)
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

// injectNotification preserves the legacy test/internal string helper. New
// production routing uses deliverWakeNotification so input and output sources
// remain explicit.
func injectNotification(cfg *wakeConfig, text string, deferForInput bool) error {
	payload := wakePayload{
		text:       text,
		provenance: wakePayloadSystemFixed,
	}
	return deliverWakeNotification(
		cfg,
		wakeNotification{input: payload, output: payload},
		deferForInput,
	)
}

func deliverWakeNotification(cfg *wakeConfig, notice wakeNotification, deferForInput bool) error {
	ownerBound := usesCoopDoorbell(cfg)
	if ownerBound {
		if _, valid := parseCoopWakeDoorbell(notice.input.text); valid {
			// Generation-token prompts are system-created before delivery.
		} else {
			notice.input = wakePayload{
				text:       coopWakeDoorbell,
				provenance: wakePayloadSystemFixed,
			}
		}
	}
	if cfg.injectMode == wakeInjectModeNone {
		emitWakeAttention(cfg, notice.output)
		return nil
	}

	inputText := notice.input.text
	plainText := inputText
	if cfg.bell && !ownerBound {
		plainText = "\a" + plainText
	}

	if shouldDeferBeforeInject(cfg, deferForInput) {
		if !waitForWakeInputQuiet(cfg) {
			emitWakeAttention(cfg, notice.output)
			return nil
		}
	}

	// External injection: delegate to user-specified command instead of TIOCSTI.
	// The command receives the notification text as its last argument.
	if cfg.injectVia != "" {
		allowed, guardErr := authorizeTerminalWrite(cfg)
		if guardErr != nil {
			return guardErr
		}
		if !allowed {
			return nil
		}
		if err := injectVia(cfg, plainText); err != nil {
			if cfg.fallbackWarn {
				_ = writeStderr("amq wake: --inject-via failed: %v\n", err)
				_ = writeStderr("amq wake: falling back to stderr notification\n")
				cfg.fallbackWarn = false
			}
			emitWakeAttention(cfg, notice.output)
			return nil
		}
		return nil
	}

	mode := effectiveInjectMode(cfg)
	if cfg.debug {
		_ = writeStderr("amq wake [debug]: mode=%s text_len=%d\n", mode, len(inputText))
	}
	var injectErr error
	switch mode {
	case wakeInjectModeRaw:
		// Raw mode: inject text and CR separately to avoid paste detection.
		// Ink treats multi-char input as paste, not keypresses. Sending text+CR
		// as one chunk makes Ink see pasted text, not an Enter keypress.
		injectedText := inputText
		if cfg.bell && !ownerBound {
			injectedText = "\a" + injectedText
		}
		_, injectErr = injectRawNotification(cfg, injectedText)

	case wakeInjectModePaste:
		// Paste mode: bracketed paste with delayed CR
		// Works with crossterm/ratatui apps
		// Send paste content first, then CR after short delay to avoid coalescing
		pasteText := inputText
		if !ownerBound {
			pasteText = "\x1b[200~" + inputText + "\x1b[201~"
		}
		if cfg.bell && !ownerBound {
			pasteText = "\a" + pasteText
		}
		wrote, err := writeTerminalChunk(cfg, pasteText)
		if err != nil {
			injectErr = err
		} else if wrote {
			// Small delay to ensure CR lands in separate read cycle
			time.Sleep(25 * time.Millisecond)
			_, err := writeTerminalChunk(cfg, "\r")
			injectErr = err
		}

	default:
		// Unknown mode, fall back to raw
		if ownerBound {
			wrote, err := writeTerminalChunk(cfg, inputText)
			if err != nil {
				injectErr = err
			} else if wrote {
				_, err := writeTerminalChunk(cfg, "\r")
				injectErr = err
			}
		} else {
			injectedText := inputText + "\r"
			if cfg.bell {
				injectedText = "\a" + injectedText
			}
			_, injectErr = writeTerminalChunk(cfg, injectedText)
		}
	}

	if injectErr != nil {
		var unsupported *wakeInjectorUnsupportedError
		if errors.As(injectErr, &unsupported) {
			mode := effectiveInjectMode(cfg)
			reason := wakeInjectorUnsupportedReason(mode, injectErr)
			if cfg.recordNotifierStatus != nil {
				if err := cfg.recordNotifierStatus(
					wakeInjectorUnsupportedStatus,
					mode,
					reason,
				); err != nil {
					_ = writeStderr("amq wake: record unsupported injector status: %v\n", err)
				}
			}
			cfg.injectMode = wakeInjectModeNone
			_ = writeStderr("amq wake: warning: %s\n", reason)
			cfg.fallbackWarn = false
			emitWakeAttention(cfg, notice.output)
			return nil
		}
		var authorityErr *wakeTerminalAuthorityError
		if errors.As(injectErr, &authorityErr) {
			return injectErr
		}
		if cfg.fallbackWarn {
			_ = writeStderr("amq wake: TIOCSTI injection failed: %v\n", injectErr)
			_ = writeStderr("amq wake: falling back to stderr notification\n")
			cfg.fallbackWarn = false
		}
		// Fallback: use the output-only attention tier; never retry input.
		emitWakeAttention(cfg, notice.output)
		return nil
	}

	return nil
}

func usesCoopDoorbell(cfg *wakeConfig) bool {
	if cfg.wakeOwner != nil {
		return true
	}
	if cfg.controlStop == nil {
		return false
	}
	// Direct guarded-write tests and injected terminal authorities do not need
	// to synthesize a complete process owner merely to exercise the doorbell.
	// A normal ownerless repair has a rooted control channel but no retained
	// terminal writer and must keep its legacy notification payload.
	return cfg.root == "" ||
		cfg.beforeTerminalWrite != nil ||
		cfg.terminalWrite != nil
}

type wakeTerminalAuthorityError struct {
	err error
}

func (err *wakeTerminalAuthorityError) Error() string {
	return err.err.Error()
}

func (err *wakeTerminalAuthorityError) Unwrap() error {
	return err.err
}

func authorizeTerminalWrite(cfg *wakeConfig) (bool, error) {
	if cfg.controlStop != nil {
		select {
		case <-cfg.controlStop:
			return false, nil
		default:
		}
	}
	if cfg.controlStop != nil &&
		cfg.beforeTerminalWrite == nil &&
		cfg.terminalWrite == nil &&
		cfg.root != "" &&
		cfg.me != "" {
		if !authorizeTerminalWritePlatform(cfg) {
			return false, nil
		}
	}
	if cfg.beforeTerminalWrite != nil {
		if err := cfg.beforeTerminalWrite(); err != nil {
			if isWakeTerminalControlStopped(err) {
				return false, nil
			}
			return false, &wakeTerminalAuthorityError{err: err}
		}
	}
	return true, nil
}

func writeTerminalChunk(cfg *wakeConfig, chunk string) (bool, error) {
	allowed, err := authorizeTerminalWrite(cfg)
	if err != nil || !allowed {
		return false, err
	}
	if cfg.terminalWrite != nil {
		if err := cfg.terminalWrite(chunk); err != nil {
			if isWakeTerminalControlStopped(err) {
				return false, nil
			}
			var unsupported *wakeInjectorUnsupportedError
			if errors.As(err, &unsupported) {
				return true, err
			}
			return true, &wakeTerminalAuthorityError{err: err}
		}
		return true, nil
	}
	return true, tiocstiInject(chunk)
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

func clearRawSubmitState(cfg *wakeConfig) {
	cfg.rawSubmitPhase = rawSubmitIdle
	cfg.rawSubmitPayload = ""
}

func reconcileRawSubmitAfterInboxDrain(cfg *wakeConfig) error {
	if cfg.rawSubmitPhase == rawSubmitIdle {
		return nil
	}
	waited, drained, err := waitForRawInputDrained(
		rawInjectCRDrainTimeout,
		rawInjectDrainPollInterval,
	)
	if cfg.debug {
		switch {
		case err != nil:
			_ = writeStderr("amq wake [debug]: empty-inbox submit drain check failed after %s: %v\n", waited, err)
		case drained:
			_ = writeStderr("amq wake [debug]: empty-inbox submit queue drained after %s\n", waited)
		default:
			_ = writeStderr("amq wake [debug]: empty-inbox submit key still queued after %s\n", waited)
		}
	}
	if err != nil || !drained {
		return nil
	}
	if cfg.rawSubmitPhase == rawFirstSubmitQueued {
		_, err := injectRawRescueSubmit(cfg)
		return err
	}
	clearRawSubmitState(cfg)
	return nil
}

func injectRawNotification(cfg *wakeConfig, injectedText string) (bool, error) {
	if cfg.rawSubmitPhase != rawSubmitIdle {
		// A previous submit key is still in the terminal input queue. Never
		// retype the notification: wait until the reader resumes, then send only
		// the one permitted rescue Enter after the first submit. A drained
		// rescue is final; it must not generate a third Enter.
		pendingPhase := cfg.rawSubmitPhase
		pendingPayload := cfg.rawSubmitPayload
		waited, drained, err := waitForRawInputDrained(rawInjectCRDrainTimeout, rawInjectDrainPollInterval)
		if err != nil {
			if cfg.debug {
				_ = writeStderr("amq wake [debug]: pending submit drain check failed after %s: %v\n", waited, err)
			}
			return false, nil
		}
		if !drained {
			if cfg.debug {
				_ = writeStderr("amq wake [debug]: submit key still queued after %s; waiting for terminal reader\n", waited)
			}
			return false, nil
		}
		clearRawSubmitState(cfg)
		if pendingPhase == rawFirstSubmitQueued {
			cfg.rawSubmitPhase = rawFirstSubmitQueued
			cfg.rawSubmitPayload = pendingPayload
			if _, err := injectRawRescueSubmit(cfg); err != nil {
				return false, err
			}
			if cfg.rawSubmitPhase != rawSubmitIdle {
				return false, nil
			}
		}
		if pendingPayload != "" && pendingPayload == injectedText {
			return true, nil
		}
		// The old submit sequence is fully drained. It is now safe to type the
		// current generation without stacking bytes behind stale terminal input.
	}

	if cfg.debug {
		_ = writeStderr("amq wake [debug]: injecting %d bytes of text\n", len(injectedText))
	}
	wrote, err := writeTerminalChunk(cfg, injectedText)
	if err != nil {
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: text inject failed: %v\n", err)
		}
		return false, err
	}
	if !wrote {
		return false, nil
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
		wrote, err := writeTerminalChunk(cfg, prelude)
		if err != nil {
			if cfg.debug {
				_ = writeStderr("amq wake [debug]: prelude inject failed: %v\n", err)
			}
			return false, err
		}
		if !wrote {
			return false, nil
		}
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: prelude injected OK (%q)\n", prelude)
		}
	}

	// Hold the submit CR past the TUI's paste-burst window (see
	// rawInjectSettleDelay) so it is classified as a real Enter keypress, not a
	// pasted newline.
	rawInjectSleep(rawInjectSettleDelay)

	wrote, err = writeTerminalChunk(cfg, "\r")
	if err != nil {
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: submit key inject failed: %v\n", err)
		}
		return false, err
	}
	if !wrote {
		return false, nil
	}
	if cfg.debug {
		_ = writeStderr("amq wake [debug]: submit key injected OK\n")
	}
	cfg.rawSubmitPayload = injectedText

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
		cfg.rawSubmitPhase = rawFirstSubmitQueued
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: submit key still queued after %s; skipping rescue submit\n", crWaited)
		}
		return false, nil
	}
	if crErr != nil {
		// Without queue visibility, preserve the historical best-effort path:
		// one spaced rescue Enter and no unbounded retries.
		rawInjectSleep(rawInjectSettleDelay)
		wrote, err = writeTerminalChunk(cfg, "\r")
		if err != nil {
			var authorityErr *wakeTerminalAuthorityError
			if errors.As(err, &authorityErr) {
				clearRawSubmitState(cfg)
				return true, err
			}
			if cfg.debug {
				_ = writeStderr("amq wake [debug]: rescue submit inject failed: %v\n", err)
			}
			clearRawSubmitState(cfg)
			return true, nil
		}
		if !wrote {
			clearRawSubmitState(cfg)
			return true, nil
		}
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: rescue submit injected OK\n")
		}
		clearRawSubmitState(cfg)
		return true, nil
	}

	cfg.rawSubmitPhase = rawFirstSubmitQueued
	return injectRawRescueSubmit(cfg)
}

func injectRawRescueSubmit(cfg *wakeConfig) (bool, error) {
	rawInjectSleep(rawInjectSettleDelay)
	wrote, err := writeTerminalChunk(cfg, "\r")
	if err != nil {
		// The text and first submit key were already delivered; the rescue is
		// best-effort unless exact terminal authority was lost.
		var authorityErr *wakeTerminalAuthorityError
		if errors.As(err, &authorityErr) {
			return false, err
		}
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: rescue submit inject failed: %v\n", err)
		}
		return false, nil
	}
	if !wrote {
		return false, nil
	}
	if cfg.debug {
		_ = writeStderr("amq wake [debug]: rescue submit injected OK\n")
	}
	waited, drained, drainErr := waitForRawInputDrained(rawInjectCRDrainTimeout, rawInjectDrainPollInterval)
	if drainErr != nil {
		// Queue inspection is best-effort. A successful terminal write remains
		// the strongest evidence available when the platform cannot inspect it.
		clearRawSubmitState(cfg)
		return true, nil
	}
	if !drained {
		cfg.rawSubmitPhase = rawRescueSubmitQueued
		if cfg.debug {
			_ = writeStderr("amq wake [debug]: rescue submit still queued after %s; waiting for terminal reader\n", waited)
		}
		return false, nil
	}
	clearRawSubmitState(cfg)
	return true, nil
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
