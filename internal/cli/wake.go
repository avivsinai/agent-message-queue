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
	me                            string
	root                          string
	session                       string
	injectCmd                     string
	injectVia                     string // external command for injection (replaces TIOCSTI)
	injectArgs                    []string
	wakeOwner                     *wakeOwner
	injectTimeout                 time.Duration
	bell                          bool
	debounce                      time.Duration
	previewLen                    int
	strict                        bool
	fallbackWarn                  bool
	injectMode                    string // auto, raw, paste
	requestedInjectMode           string
	retryUntil                    string
	legacyTIOCSTIDemoted          bool
	debug                         bool
	deferWhileInput               bool
	inputQuietFor                 time.Duration
	inputPollInterval             time.Duration
	inputMaxHold                  time.Duration
	interrupt                     bool
	interruptLabel                string
	interruptPriority             string
	interruptKey                  string
	interruptNotice               string
	interruptCooldown             time.Duration
	lastInterrupt                 time.Time
	controlStop                   <-chan struct{}
	beforeTerminalWrite           func() error
	terminalWrite                 func(string) error
	inspectTerminalGeneration     func() wakeLockInspection
	terminalGeneration            string
	terminalTTY                   string
	baselineRequested             bool
	baselineInherited             bool
	baselineExisting              map[string]wakeFileIdentity
	inputDelivery                 wakeInputDeliveryState
	inputRecoveryRequired         bool
	doorbell                      wakeDoorbellState
	doorbellNow                   func() time.Time
	lastAttemptAttention          bool
	lastAttemptTransientAttention bool
	onBaselineReady               func(map[string]wakeFileIdentity) error
	onPrepared                    func(wakeAdmissionWatcher) error
	retainedAgent                 wakeRetainedAgent
	retainedInbox                 wakeInboxReader
	touchPresence                 func() error
	maintenanceTicks              <-chan time.Time
	maintenanceOutputs            []*os.File
	preconditionCheck             func(*wakeConfig) error
	onPendingNotify               func()
	recordNotifierStatus          func(status, mode, reason string) error
	pendingNotifierStatus         *wakeNotifierStatus
	recordDoorbellStatus          func(parked bool, attempts uint) error
	pendingDoorbellStatus         *wakeDoorbellStatus
	reportedDoorbellStatus        wakeDoorbellStatus
	recordAttention               func(wakeAttentionEmission) error
	attentionEnv                  func(string) string
	attentionIsTTY                func() bool
	attentionWrite                func([]byte) (int, error)
	diagnosticIsTTY               func() bool
}

type wakeNotifierStatus struct {
	status string
	mode   string
	reason string
}

type wakeDoorbellStatus struct {
	parked   bool
	attempts uint
}

const maxWakeNotificationSenderClauses = 8

type wakeInputDeliveryPhase uint8

const (
	wakeInputDeliveryIdle wakeInputDeliveryPhase = iota
	wakeInputPayloadPending
	wakeInputRawPreludePending
	wakeInputPrimarySubmitPending
	wakeInputRawFirstSubmitQueued
	wakeInputRawRescuePending
	wakeInputRawRescueQueued
)

type wakeInputDeliveryState struct {
	phase               wakeInputDeliveryPhase
	mode                string
	payload             string
	acceptedBytes       int
	acceptanceUncertain bool
}

func (state *wakeInputDeliveryState) reset() {
	*state = wakeInputDeliveryState{}
}

func (state wakeInputDeliveryState) pending() bool {
	return state.phase != wakeInputDeliveryIdle
}

func (state wakeInputDeliveryState) confirmedInput() bool {
	return state.acceptedBytes > 0 ||
		(state.phase != wakeInputDeliveryIdle &&
			state.phase != wakeInputPayloadPending)
}

func (state wakeInputDeliveryState) blocksDemotion() bool {
	return state.confirmedInput() || state.acceptanceUncertain
}

type wakeAdmissionWatcher interface {
	Errors() <-chan error
}

type wakeRetainedAgent interface {
	isWakeRetainedAgent()
}

type wakeInboxReader interface {
	ReadDir() ([]os.DirEntry, error)
	ReadHeader(name string) (format.Header, error)
}

type wakeInboxFileInfoReader interface {
	FileInfo(name string) (os.FileInfo, error)
}

type wakeInboxScanError struct {
	err error
}

func (err *wakeInboxScanError) Error() string {
	return err.err.Error()
}

func (err *wakeInboxScanError) Unwrap() error {
	return err.err
}

type wakeTerminalPartialProgressError struct {
	err error
}

func (err *wakeTerminalPartialProgressError) Error() string {
	return err.err.Error()
}

func (err *wakeTerminalPartialProgressError) Unwrap() error {
	return err.err
}

func isWakeTerminalPartialProgress(err error) bool {
	var partial *wakeTerminalPartialProgressError
	return errors.As(err, &partial)
}

type wakeInputDemotionBlockedError struct {
	err error
}

func (err *wakeInputDemotionBlockedError) Error() string {
	return "terminal input has confirmed or uncertain progress; refusing to discard its exact completion debt: " + err.err.Error()
}

func (err *wakeInputDemotionBlockedError) Unwrap() error {
	return err.err
}

func isWakeInputDemotionBlocked(err error) bool {
	var blocked *wakeInputDemotionBlockedError
	return errors.As(err, &blocked)
}

type wakeTerminalProgressUncertainError struct {
	err error
}

func (err *wakeTerminalProgressUncertainError) Error() string {
	return "terminal input acceptance is uncertain; refusing to replay: " + err.err.Error()
}

func (err *wakeTerminalProgressUncertainError) Unwrap() error {
	return err.err
}

func isWakeTerminalProgressUncertain(err error) bool {
	var uncertain *wakeTerminalProgressUncertainError
	return errors.As(err, &uncertain)
}

type wakeTerminalAcceptedProgress interface {
	wakeAcceptedBytes() int
}

const defaultInjectTimeout = 5 * time.Second
const (
	wakeInjectModeAuto     = "auto"
	wakeInjectModeRaw      = "raw"
	wakeInjectModePaste    = "paste"
	wakeInjectModeNone     = "none"
	wakeRetryUntilDrained  = "drained"
	wakeRetryUntilInjected = "injected"

	coopWakeDoorbell = ": AMQ doorbell run amq drain --include-body then act on it"

	rawInjectDrainTimeout      = 2 * time.Second
	rawInjectDrainPollInterval = 10 * time.Millisecond
	// rawInjectCRDrainTimeout bounds the wait for the submit CR itself to be
	// consumed before deciding whether the second rescue CR is safe to send.
	rawInjectCRDrainTimeout = 1 * time.Second
	// Three samples require two follow-up confirmations after the first quiet
	// observation, while adding at most two poll intervals to a real idle
	// transition. Any active sample resets the evidence.
	requiredInputQuietSamples = 3

	wakeInputRecoveryNotice = "AMQ terminal input delivery is incomplete; inspect the composer, run amq drain --include-body, then restart this agent session"
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

func normalizeWakeRetryUntil(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return wakeRetryUntilDrained, nil
	}
	switch value {
	case wakeRetryUntilDrained, wakeRetryUntilInjected:
		return value, nil
	default:
		return "", fmt.Errorf(
			"invalid retry-until %q (supported: drained, injected)",
			raw,
		)
	}
}

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

func validCoopWakeDoorbell(prompt string) bool {
	return prompt == coopWakeDoorbell
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
		return &wakeInboxScanError{
			err: fmt.Errorf("read wake inbox %s: %w", inboxNew, err),
		}
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
		var fileInfo os.FileInfo
		var infoErr error
		if retained, ok := cfg.retainedInbox.(wakeInboxFileInfoReader); ok {
			fileInfo, infoErr = retained.FileInfo(name)
		} else {
			fileInfo, infoErr = entry.Info()
		}
		pendingInfo := fileInfo
		if infoErr != nil {
			pendingInfo = nil
		}
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
		if err != nil && os.IsNotExist(err) {
			continue
		}
		currentPending[name] = pendingInfo
		if err != nil {
			// Count corrupt messages too
			messages = append(messages, wakeMsgInfo{from: "unknown", subject: "(parse error)"})
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

	if cfg.inputRecoveryRequired &&
		dischargeWakeInputRecoveryAfterProgress(cfg, currentPending) &&
		len(messages) == 0 {
		return nil
	}
	if len(messages) == 0 {
		if cfg.inputRecoveryRequired {
			return deliverWakeInputRecoveryAttention(
				cfg,
				currentPending,
			)
		}
		cfg.doorbell.reset()
		persistWakeDoorbellStatusBestEffort(cfg)
		return reconcileWakeInputAfterInboxDrain(cfg)
	}

	if cfg.interrupt && len(interruptMessages) > 0 {
		interruptText := buildInterruptText(cfg.session, interruptMessages, interruptCounts, cfg.previewLen, cfg.interruptNotice)
		notice := peerWakeNotification(interruptText)
		if cfg.interruptNotice != "" {
			notice = operatorWakeNotification(interruptText)
		}
		if cfg.inputRecoveryRequired {
			return deliverNewMessageNotification(cfg, notice, false, currentPending)
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
	if cfg.inputRecoveryRequired {
		if !dischargeWakeInputRecoveryAfterProgress(cfg, currentPending) {
			return deliverWakeInputRecoveryAttention(cfg, currentPending)
		}
		if len(currentPending) == 0 {
			return nil
		}
	}
	if cfg.injectMode == wakeInjectModeNone {
		if cfg.inputDelivery.blocksDemotion() {
			return &wakeInputDemotionBlockedError{
				err: errors.New("input mode became none before retained terminal input completed"),
			}
		}
		clearWakeInputState(cfg)
	}
	now := cfg.wakeDoorbellNow()
	plan := cfg.doorbell.plan(now, currentPending)
	persistWakeDoorbellStatusBestEffort(cfg)
	if !plan.attempt {
		return nil
	}
	if plan.progress && cfg.inputDelivery.pending() {
		if err := reconcileWakeInputAfterInboxDrain(cfg); err != nil {
			return err
		}
		if cfg.inputDelivery.pending() {
			return nil
		}
	}
	ownerBound := usesCoopDoorbell(cfg)
	if ownerBound && cfg.injectMode != wakeInjectModeNone {
		notice.input = wakePayload{
			text:       plan.prompt,
			provenance: wakePayloadSystemFixed,
		}
		deliveryErr := deliverWakeNotification(cfg, notice, deferForInput)
		if isWakeInputDemotionBlocked(deliveryErr) {
			return enterWakeInputRecovery(cfg, currentPending, deliveryErr)
		}
		if cfg.injectMode == wakeInjectModeNone {
			clearWakeInputState(cfg)
			if shouldRecordWakeAttempt(deliveryErr) {
				recordWakeAttempt(
					cfg,
					cfg.wakeDoorbellNow(),
					currentPending,
					deliveryErr,
				)
			}
			return deliveryErr
		}
		if shouldRecordWakeAttempt(deliveryErr) {
			recordWakeAttempt(
				cfg,
				cfg.wakeDoorbellNow(),
				currentPending,
				deliveryErr,
			)
		}
		return deliveryErr
	}

	deliveryErr := deliverWakeNotification(cfg, notice, deferForInput)
	if isWakeInputDemotionBlocked(deliveryErr) {
		return enterWakeInputRecovery(cfg, currentPending, deliveryErr)
	}
	if shouldRecordWakeAttempt(deliveryErr) {
		recordWakeAttempt(
			cfg,
			cfg.wakeDoorbellNow(),
			currentPending,
			deliveryErr,
		)
	}
	return deliveryErr
}

func recordWakeAttempt(
	cfg *wakeConfig,
	now time.Time,
	currentPending map[string]os.FileInfo,
	deliveryErr error,
) {
	if len(cfg.doorbell.cohort) == 0 {
		cfg.doorbell.arm(currentPending)
	}
	if cfg.retryUntil == wakeRetryUntilInjected &&
		deliveryErr == nil &&
		!cfg.lastAttemptAttention &&
		!cfg.lastAttemptTransientAttention {
		cfg.doorbell.recordInjected(currentPending)
		return
	}
	var attentionErr *wakeAttentionDeliveryError
	if cfg.lastAttemptTransientAttention {
		// Input deferral emitted attention but made no synthetic-input attempt,
		// so it advances cadence without consuming the finite injection budget.
		cfg.doorbell.recordDeferredInputAttempt(now)
	} else if cfg.lastAttemptAttention || errors.As(deliveryErr, &attentionErr) {
		cfg.doorbell.recordAttentionAttempt(now)
	} else {
		cfg.doorbell.recordAttempt(now)
	}
	persistWakeDoorbellStatusBestEffort(cfg)
}

func shouldRecordWakeAttempt(err error) bool {
	if err == nil {
		return true
	}
	return !isWakeTerminalAuthorityLoss(err) &&
		!isWakeTerminalPartialProgress(err) &&
		!isWakeInputDemotionBlocked(err) &&
		!isWakeTerminalProgressUncertain(err)
}

func deliverWakeInputRecoveryAttention(
	cfg *wakeConfig,
	currentPending map[string]os.FileInfo,
) error {
	now := cfg.wakeDoorbellNow()
	if !cfg.doorbell.planRecoveryAttention(now, currentPending) {
		return nil
	}
	cfg.doorbell.recordRecoveryRequired(now, currentPending)
	persistWakeDoorbellStatusBestEffort(cfg)
	if err := emitWakeAttention(cfg, wakePayload{
		text:       wakeInputRecoveryNotice,
		provenance: wakePayloadSystemFixed,
	}); err != nil {
		return err
	}
	cfg.doorbell.recordRecoveryAttentionDelivered()
	if len(currentPending) == 0 {
		cfg.doorbell.noteRecoveryInboxEmpty()
	}
	return nil
}

func enterWakeInputRecovery(
	cfg *wakeConfig,
	currentPending map[string]os.FileInfo,
	cause error,
) error {
	markWakeInputRecoveryRequired(cfg, cause)
	if err := deliverWakeInputRecoveryAttention(cfg, currentPending); err != nil {
		return errors.Join(cause, err)
	}
	return nil
}

func markWakeInputRecoveryRequired(cfg *wakeConfig, cause error) {
	cfg.inputRecoveryRequired = true
	mode := effectiveInjectMode(cfg)
	reason := wakeInputRecoveryNotice
	if cause != nil {
		reason += ": " + cause.Error()
	}
	if err := persistWakeNotifierStatus(
		cfg,
		wakeInputRecoveryRequiredStatus,
		mode,
		reason,
	); err != nil {
		_ = writeWakeDiagnostic(cfg, "amq wake: record input recovery-required status: %v\n", err)
	}
}

func dischargeWakeInputRecoveryAfterProgress(
	cfg *wakeConfig,
	currentPending map[string]os.FileInfo,
) bool {
	if cfg == nil || !cfg.inputRecoveryRequired {
		return false
	}
	progressed := len(currentPending) == 0
	if !progressed && cfg.doorbell.phase == wakeDoorbellRecoveryRequired {
		progressed = wakeCohortProgressed(cfg.doorbell.cohort, currentPending)
	}
	if !progressed {
		return false
	}

	// Durable consumer progress proves that the composer state changed after
	// the uncertain write. Never resume the old suffix: discard that debt and
	// let any remaining cohort start with one fresh, complete doorbell. This
	// accepts bounded composer noise over a permanently undriven session, while
	// preserving the no-stacking rule for the old partial payload.
	cfg.inputRecoveryRequired = false
	clearWakeInputState(cfg)
	cfg.doorbell.reset()
	persistWakeDoorbellStatusBestEffort(cfg)
	if err := persistWakeNotifierStatus(cfg, "", effectiveInjectMode(cfg), ""); err != nil {
		_ = writeWakeDiagnostic(cfg, "amq wake: clear input recovery-required status: %v\n", err)
	}
	return true
}

func persistWakeNotifierStatus(cfg *wakeConfig, status, mode, reason string) error {
	if cfg == nil || cfg.recordNotifierStatus == nil {
		return nil
	}
	desired := &wakeNotifierStatus{status: status, mode: mode, reason: reason}
	if err := cfg.recordNotifierStatus(status, mode, reason); err != nil {
		cfg.pendingNotifierStatus = desired
		return err
	}
	cfg.pendingNotifierStatus = nil
	return nil
}

func retryPendingWakeNotifierStatus(cfg *wakeConfig) error {
	if cfg == nil || cfg.pendingNotifierStatus == nil {
		return nil
	}
	pending := cfg.pendingNotifierStatus
	return persistWakeNotifierStatus(cfg, pending.status, pending.mode, pending.reason)
}

func persistWakeDoorbellStatusBestEffort(cfg *wakeConfig) {
	if err := persistWakeDoorbellStatus(cfg); err != nil {
		_ = writeWakeDiagnostic(cfg, "amq wake: persist doorbell parked status: %v; retrying\n", err)
	}
}

func persistWakeDoorbellStatus(cfg *wakeConfig) error {
	if cfg == nil || cfg.recordDoorbellStatus == nil {
		return nil
	}
	attempts, parked := cfg.doorbell.parkedReminderAttempts()
	desired := wakeDoorbellStatus{parked: parked, attempts: attempts}
	if !parked {
		desired.attempts = 0
	}
	if cfg.pendingDoorbellStatus == nil && desired == cfg.reportedDoorbellStatus {
		return nil
	}
	if err := cfg.recordDoorbellStatus(desired.parked, desired.attempts); err != nil {
		cfg.pendingDoorbellStatus = &desired
		return err
	}
	cfg.reportedDoorbellStatus = desired
	cfg.pendingDoorbellStatus = nil
	return nil
}

func retryPendingWakeDoorbellStatus(cfg *wakeConfig) error {
	if cfg == nil || cfg.pendingDoorbellStatus == nil || cfg.recordDoorbellStatus == nil {
		return nil
	}
	pending := *cfg.pendingDoorbellStatus
	if err := cfg.recordDoorbellStatus(pending.parked, pending.attempts); err != nil {
		return err
	}
	cfg.reportedDoorbellStatus = pending
	cfg.pendingDoorbellStatus = nil
	return nil
}

func (cfg *wakeConfig) wakeDoorbellNow() time.Time {
	if cfg.doorbellNow != nil {
		return cfg.doorbellNow()
	}
	return time.Now()
}

func snapshotWakeFileIdentities(current map[string]os.FileInfo) map[string]*wakeFileIdentity {
	snapshot := make(map[string]*wakeFileIdentity, len(current))
	for name, info := range current {
		if info == nil {
			snapshot[name] = nil
			continue
		}
		if identity, ok := captureWakeFileIdentity(info); ok {
			snapshot[name] = &identity
			continue
		}
		snapshot[name] = nil
	}
	return snapshot
}

func sameKnownWakeCohort(
	previous map[string]*wakeFileIdentity,
	current map[string]os.FileInfo,
) bool {
	if len(previous) == 0 || len(previous) != len(current) {
		return false
	}
	for name, previousIdentity := range previous {
		info, exists := current[name]
		if !exists || previousIdentity == nil || info == nil {
			return false
		}
		currentIdentity, known := captureWakeFileIdentity(info)
		if !known || currentIdentity != *previousIdentity {
			return false
		}
	}
	return true
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
	return fmt.Sprintf(
		"%s: %d messages - %s. Drain with: amq drain --include-body — then act on it",
		prefix,
		count,
		strings.Join(buildWakeSenderClauses(senders, senderCounts), ", "),
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

	senders := make([]string, 0, len(senderCounts))
	for s := range senderCounts {
		senders = append(senders, s)
	}
	sort.Strings(senders)
	parts := buildWakeSenderClauses(senders, senderCounts)
	return fmt.Sprintf("%s: %d urgent messages - %s. Drain with: amq drain --include-body — then act on it",
		prefix, count, strings.Join(parts, ", "))
}

func buildWakeSenderClauses(senders []string, senderCounts map[string]int) []string {
	limit := min(len(senders), maxWakeNotificationSenderClauses)
	parts := make([]string, 0, limit+1)
	for _, sender := range senders[:limit] {
		parts = append(parts, fmt.Sprintf("%d from %s", senderCounts[sender], sender))
	}
	if omitted := len(senders) - limit; omitted > 0 {
		label := "senders"
		if omitted == 1 {
			label = "sender"
		}
		parts = append(parts, fmt.Sprintf("%d other %s", omitted, label))
	}
	return parts
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
		if wakeUsesAlternateScreenTUI(cfg.me) {
			mode = wakeInjectModeRaw
		} else {
			mode = wakeInjectModePaste
		}
	}
	return mode
}

func wakeUsesAlternateScreenTUI(me string) bool {
	meLower := strings.ToLower(me)
	return strings.Contains(meLower, "claude") || strings.Contains(meLower, "codex")
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
	cfg.lastAttemptAttention = false
	cfg.lastAttemptTransientAttention = false
	ownerBound := usesCoopDoorbell(cfg)
	if ownerBound {
		if validCoopWakeDoorbell(notice.input.text) {
			// The fixed doorbell is system-created before delivery.
		} else {
			notice.input = wakePayload{
				text:       coopWakeDoorbell,
				provenance: wakePayloadSystemFixed,
			}
		}
	}
	if cfg.injectMode == wakeInjectModeNone {
		return deliverWakeAttentionOnly(cfg, notice.output)
	}

	inputText := notice.input.text
	plainText := inputText
	if cfg.bell && !ownerBound {
		plainText = "\a" + plainText
	}

	if shouldDeferBeforeInject(cfg, deferForInput) {
		if !waitForWakeInputQuiet(cfg) {
			return deliverWakeTransientAttention(cfg, notice.output, nil)
		}
	}

	// External injection: delegate to user-specified command instead of TIOCSTI.
	// The command receives the notification text as its last argument.
	if cfg.injectVia != "" {
		allowed, guardErr := authorizeTerminalWrite(cfg)
		if guardErr != nil {
			return deliverWakeAttentionAfterInputRefusal(
				cfg,
				notice.output,
				guardErr,
			)
		}
		if !allowed {
			return deliverWakeAttentionOnly(cfg, notice.output)
		}
		if err := injectVia(cfg, plainText); err != nil {
			if cfg.fallbackWarn {
				_ = writeWakeDiagnostic(cfg, "amq wake: --inject-via failed: %v\n", err)
				_ = writeWakeDiagnostic(cfg, "amq wake: falling back to stderr notification\n")
				cfg.fallbackWarn = false
			}
			return deliverWakeAttentionOnly(cfg, notice.output)
		}
		return nil
	}

	mode := effectiveInjectMode(cfg)
	if cfg.debug {
		_ = writeWakeDiagnostic(cfg, "amq wake [debug]: mode=%s text_len=%d\n", mode, len(inputText))
	}
	var injectErr error
	inputAccepted := false
	switch mode {
	case wakeInjectModeRaw:
		// Raw mode: inject text and CR separately to avoid paste detection.
		// Ink treats multi-char input as paste, not keypresses. Sending text+CR
		// as one chunk makes Ink see pasted text, not an Enter keypress.
		injectedText := inputText
		if cfg.bell && !ownerBound {
			injectedText = "\a" + injectedText
		}
		inputAccepted, injectErr = injectRawNotification(cfg, injectedText)
		inputAccepted = inputAccepted || cfg.inputDelivery.confirmedInput()

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
		inputAccepted, injectErr = injectTerminalNotification(
			cfg,
			wakeInjectModePaste,
			pasteText,
		)
		inputAccepted = inputAccepted || cfg.inputDelivery.confirmedInput()

	default:
		// Unknown mode, fall back to raw
		if ownerBound {
			wrote, err := writeTerminalChunk(cfg, inputText)
			inputAccepted = wrote
			if err != nil {
				injectErr = err
			} else if wrote {
				submitted, err := writeTerminalChunk(cfg, "\r")
				inputAccepted = inputAccepted || submitted
				injectErr = err
			}
		} else {
			injectedText := inputText + "\r"
			if cfg.bell {
				injectedText = "\a" + injectedText
			}
			inputAccepted, injectErr = writeTerminalChunk(cfg, injectedText)
		}
	}

	if injectErr != nil {
		if isWakeTerminalPartialProgress(injectErr) {
			return injectErr
		}
		var uncertain *wakeTerminalProgressUncertainError
		if errors.As(injectErr, &uncertain) {
			cfg.inputDelivery.acceptanceUncertain = true
			_ = writeWakeDiagnostic(cfg, "amq wake: warning: %v\n", uncertain)
			cfg.fallbackWarn = false
			return &wakeInputDemotionBlockedError{err: uncertain}
		}
		if isWakeInputDemotionBlocked(injectErr) {
			return injectErr
		}
		var unsupported *wakeInjectorUnsupportedError
		if errors.As(injectErr, &unsupported) {
			mode := effectiveInjectMode(cfg)
			reason := wakeInjectorUnsupportedReason(mode, injectErr)
			if err := persistWakeNotifierStatus(
				cfg,
				wakeInjectorUnsupportedStatus,
				mode,
				reason,
			); err != nil {
				_ = writeWakeDiagnostic(cfg, "amq wake: record unsupported injector status: %v\n", err)
			}
			if err := disableWakeInput(cfg, injectErr); err != nil {
				return err
			}
			_ = writeWakeDiagnostic(cfg, "amq wake: warning: %s\n", reason)
			cfg.fallbackWarn = false
			return deliverWakeAttentionOnly(cfg, notice.output)
		}
		var authorityErr *wakeTerminalAuthorityError
		if errors.As(injectErr, &authorityErr) {
			if !cfg.inputDelivery.confirmedInput() {
				return deliverWakeAttentionAfterInputRefusal(
					cfg,
					notice.output,
					injectErr,
				)
			}
			return injectErr
		}
		if cfg.fallbackWarn {
			_ = writeWakeDiagnostic(cfg, "amq wake: TIOCSTI injection failed: %v\n", injectErr)
			_ = writeWakeDiagnostic(cfg, "amq wake: falling back to stderr notification\n")
			cfg.fallbackWarn = false
		}
		// Fallback: use the output-only attention tier; never retry input.
		return deliverWakeAttentionOnly(cfg, notice.output)
	}
	if !inputAccepted {
		return deliverWakeAttentionOnly(cfg, notice.output)
	}

	return nil
}

func deliverWakeAttentionAfterInputRefusal(
	cfg *wakeConfig,
	payload wakePayload,
	cause error,
) error {
	if isWakeTerminalForegroundPGRPChanged(cause) {
		return deliverWakeTransientAttention(cfg, payload, cause)
	}
	if isWakeTerminalAuthorityLoss(cause) {
		// A replacement wake owns both terminal input and peer-rich attention.
		// Let an inconclusive authority check continue narrating, but never let
		// a conclusively stale driver speak alongside its replacement.
		return cause
	}
	if err := deliverWakeAttentionOnly(cfg, payload); err != nil {
		return errors.Join(cause, err)
	}
	return nil
}

func deliverWakeTransientAttention(
	cfg *wakeConfig,
	payload wakePayload,
	cause error,
) error {
	if err := authorizePeerWakeAttention(cfg, payload); err != nil {
		return err
	}
	now := cfg.wakeDoorbellNow()
	if !cfg.doorbell.transientAttentionDue(now) {
		return cause
	}
	cfg.lastAttemptTransientAttention = true
	cfg.doorbell.recordTransientAttentionAttempt(now)
	if err := emitWakeAttention(cfg, payload); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func deliverWakeAttentionOnly(cfg *wakeConfig, payload wakePayload) error {
	if err := authorizePeerWakeAttention(cfg, payload); err != nil {
		return err
	}
	if err := emitWakeAttention(cfg, payload); err != nil {
		return err
	}
	cfg.lastAttemptAttention = true
	return nil
}

func authorizePeerWakeAttention(cfg *wakeConfig, payload wakePayload) error {
	if cfg == nil ||
		payload.provenance == wakePayloadSystemFixed ||
		cfg.root == "" ||
		cfg.me == "" ||
		strings.TrimSpace(cfg.terminalGeneration) == "" {
		return nil
	}
	// Fence notifications fired by mailbox-cohort state, including
	// operator-authored interrupt text. System-fixed notices describe this
	// process's own state and remain available for diagnostics.
	// The platform layer owns the single generation classifier. Its typed
	// error is reserved for a missing or readable-different retained
	// generation; false with no error parks input but keeps attention alive.
	_, _, err := classifyWakeGenerationPlatformState(cfg)
	return err
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
		allowed, err := authorizeTerminalWritePlatformState(cfg)
		if err != nil {
			return false, &wakeTerminalAuthorityError{err: err}
		}
		if !allowed {
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

func writePendingWakeInputChunk(
	cfg *wakeConfig,
	state *wakeInputDeliveryState,
	chunk string,
) (bool, error) {
	if state.acceptanceUncertain {
		return false, &wakeInputDemotionBlockedError{
			err: errors.New("a prior terminal write has uncertain acceptance"),
		}
	}
	if state.acceptedBytes < 0 || state.acceptedBytes > len(chunk) {
		state.acceptanceUncertain = true
		return false, &wakeTerminalProgressUncertainError{
			err: fmt.Errorf(
				"invalid retained input offset %d for %d-byte chunk",
				state.acceptedBytes,
				len(chunk),
			),
		}
	}
	remaining := chunk[state.acceptedBytes:]
	wrote, err := writeTerminalChunk(cfg, remaining)
	if err == nil {
		if wrote {
			state.acceptedBytes = 0
		} else if state.confirmedInput() {
			return false, &wakeTerminalPartialProgressError{
				err: errors.New("terminal write authorization is temporarily unavailable"),
			}
		}
		return wrote, nil
	}

	var unsupported *wakeInjectorUnsupportedError
	if errors.As(err, &unsupported) {
		if state.blocksDemotion() {
			return wrote, &wakeInputDemotionBlockedError{err: err}
		}
		return wrote, err
	}
	if !wrote {
		if state.confirmedInput() {
			return false, &wakeTerminalPartialProgressError{err: err}
		}
		return false, err
	}

	var progress wakeTerminalAcceptedProgress
	if !errors.As(err, &progress) {
		if isWakeTerminalAuthorityLoss(err) {
			if state.confirmedInput() {
				return true, &wakeTerminalPartialProgressError{err: err}
			}
			return true, err
		}
		state.acceptanceUncertain = true
		return true, &wakeTerminalProgressUncertainError{err: err}
	}
	accepted := progress.wakeAcceptedBytes()
	if accepted < 0 || accepted >= len(remaining) {
		state.acceptanceUncertain = true
		return true, &wakeTerminalProgressUncertainError{
			err: fmt.Errorf(
				"invalid terminal progress %d for %d-byte suffix: %w",
				accepted,
				len(remaining),
				err,
			),
		}
	}
	state.acceptedBytes += accepted
	if state.confirmedInput() {
		return true, &wakeTerminalPartialProgressError{err: err}
	}
	return true, err
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

func clearWakeInputState(cfg *wakeConfig) {
	cfg.inputDelivery.reset()
}

func retireWakeInputState(cfg *wakeConfig, cause error) error {
	if cfg.inputDelivery.blocksDemotion() {
		if cause == nil {
			cause = errors.New("input retirement requested before terminal completion")
		}
		return &wakeInputDemotionBlockedError{err: cause}
	}
	cfg.doorbell.reset()
	persistWakeDoorbellStatusBestEffort(cfg)
	clearWakeInputState(cfg)
	return nil
}

func disableWakeInput(cfg *wakeConfig, cause error) error {
	rememberRequestedWakeInputMode(cfg)
	if err := retireWakeInputState(cfg, cause); err != nil {
		return err
	}
	cfg.injectMode = wakeInjectModeNone
	cfg.legacyTIOCSTIDemoted = false
	return nil
}

func disableWakeInputForLegacyTIOCSTI(cfg *wakeConfig, cause error) error {
	rememberRequestedWakeInputMode(cfg)
	if err := retireWakeInputState(cfg, cause); err != nil {
		return err
	}
	cfg.injectMode = wakeInjectModeNone
	cfg.legacyTIOCSTIDemoted = true
	return nil
}

func rememberRequestedWakeInputMode(cfg *wakeConfig) {
	if cfg.requestedInjectMode != "" ||
		cfg.injectMode == "" ||
		cfg.injectMode == wakeInjectModeNone {
		return
	}
	cfg.requestedInjectMode = cfg.injectMode
}

func reconcileWakeInputAfterInboxDrain(cfg *wakeConfig) error {
	if cfg.inputDelivery.acceptanceUncertain {
		return &wakeInputDemotionBlockedError{
			err: errors.New("inbox progressed after a terminal write with uncertain acceptance"),
		}
	}
	if !cfg.inputDelivery.pending() {
		return nil
	}
	if cfg.inputDelivery.phase == wakeInputPayloadPending {
		if cfg.inputDelivery.acceptedBytes == 0 {
			// No terminal byte was confirmed, so there is no stale composer input
			// to finish after the inbox made progress.
			clearWakeInputState(cfg)
			return nil
		}
	}
	_, err := resumeWakeInputDelivery(cfg)
	return err
}

func injectRawNotification(cfg *wakeConfig, injectedText string) (bool, error) {
	return injectTerminalNotification(cfg, wakeInjectModeRaw, injectedText)
}

func injectTerminalNotification(
	cfg *wakeConfig,
	mode string,
	payload string,
) (bool, error) {
	state := &cfg.inputDelivery
	if state.acceptanceUncertain {
		return false, &wakeInputDemotionBlockedError{
			err: errors.New("new terminal input arrived after a write with uncertain acceptance"),
		}
	}
	if state.pending() && (state.mode != mode || state.payload != payload) {
		if state.phase == wakeInputPayloadPending && state.acceptedBytes == 0 {
			// The previous payload never made confirmed progress.
			state.reset()
		} else {
			completed, err := resumeWakeInputDelivery(cfg)
			if err != nil || !completed {
				return false, err
			}
		}
	}
	if !state.pending() {
		*state = wakeInputDeliveryState{
			phase:   wakeInputPayloadPending,
			mode:    mode,
			payload: payload,
		}
	}
	return resumeWakeInputDelivery(cfg)
}

func resumeWakeInputDelivery(cfg *wakeConfig) (bool, error) {
	state := &cfg.inputDelivery
	for state.pending() {
		switch state.phase {
		case wakeInputPayloadPending:
			if cfg.debug {
				_ = writeWakeDiagnostic(cfg, "amq wake [debug]: injecting %d bytes of text\n", len(state.payload))
			}
			wrote, err := writePendingWakeInputChunk(cfg, state, state.payload)
			if err != nil {
				if cfg.debug {
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: text inject failed: %v\n", err)
				}
				return false, err
			}
			if !wrote {
				return false, nil
			}
			if state.mode == wakeInjectModePaste {
				state.phase = wakeInputPrimarySubmitPending
				continue
			}
			if state.mode != wakeInjectModeRaw {
				return false, fmt.Errorf("unsupported pending wake input mode %q", state.mode)
			}

			// The submit key must arrive in its own read() chunk; otherwise the
			// TUI can treat text+Enter as pasted input instead of a keypress.
			waited, drained, err := waitForRawInputDrained(
				rawInjectDrainTimeout,
				rawInjectDrainPollInterval,
			)
			if cfg.debug {
				switch {
				case err != nil:
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: input drain wait unavailable after %s: %v; continuing on timing alone\n", waited, err)
				case drained:
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: input queue drained after %s\n", waited)
				default:
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: input drain timeout after %s; injecting submit key anyway\n", waited)
				}
			}
			if rawSubmitPrelude(cfg.me) != "" {
				state.phase = wakeInputRawPreludePending
			} else {
				state.phase = wakeInputPrimarySubmitPending
			}

		case wakeInputRawPreludePending:
			prelude := rawSubmitPrelude(cfg.me)
			if prelude == "" {
				state.phase = wakeInputPrimarySubmitPending
				continue
			}
			wrote, err := writePendingWakeInputChunk(cfg, state, prelude)
			if err != nil {
				if cfg.debug {
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: prelude inject failed: %v\n", err)
				}
				return false, err
			}
			if !wrote {
				return false, nil
			}
			if cfg.debug {
				_ = writeWakeDiagnostic(cfg, "amq wake [debug]: prelude injected OK (%q)\n", prelude)
			}
			state.phase = wakeInputPrimarySubmitPending

		case wakeInputPrimarySubmitPending:
			if state.mode == wakeInjectModePaste {
				// Keep Enter in a separate read cycle from the paste payload.
				time.Sleep(25 * time.Millisecond)
			} else {
				// Hold the submit CR past the TUI's paste-burst window.
				rawInjectSleep(rawInjectSettleDelay)
			}
			wrote, err := writePendingWakeInputChunk(cfg, state, "\r")
			if err != nil {
				if cfg.debug {
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: submit key inject failed: %v\n", err)
				}
				return false, err
			}
			if !wrote {
				return false, nil
			}
			if state.mode == wakeInjectModePaste {
				state.reset()
				return true, nil
			}
			if cfg.debug {
				_ = writeWakeDiagnostic(cfg, "amq wake [debug]: submit key injected OK\n")
			}

			// A repeat Enter rescues a swallowed first submit. Skip it only when
			// the first Enter is provably still queued.
			waited, drained, drainErr := waitForRawInputDrained(
				rawInjectCRDrainTimeout,
				rawInjectDrainPollInterval,
			)
			if drainErr == nil && !drained {
				state.phase = wakeInputRawFirstSubmitQueued
				if cfg.debug {
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: submit key still queued after %s; skipping rescue submit\n", waited)
				}
				return false, nil
			}
			// When queue inspection is unavailable, preserve the historical one
			// spaced rescue, but retain this exact step across authority loss.
			state.phase = wakeInputRawRescuePending

		case wakeInputRawFirstSubmitQueued:
			waited, drained, err := waitForRawInputDrained(
				rawInjectCRDrainTimeout,
				rawInjectDrainPollInterval,
			)
			if cfg.debug {
				switch {
				case err != nil:
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: pending submit drain check failed after %s: %v\n", waited, err)
				case !drained:
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: submit key still queued after %s; waiting for terminal reader\n", waited)
				}
			}
			if err != nil || !drained {
				return false, nil
			}
			state.phase = wakeInputRawRescuePending

		case wakeInputRawRescuePending:
			rawInjectSleep(rawInjectSettleDelay)
			wrote, err := writePendingWakeInputChunk(cfg, state, "\r")
			if err != nil {
				if cfg.debug {
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: rescue submit inject failed: %v\n", err)
				}
				return false, err
			}
			if !wrote {
				return false, nil
			}
			if cfg.debug {
				_ = writeWakeDiagnostic(cfg, "amq wake [debug]: rescue submit injected OK\n")
			}
			waited, drained, drainErr := waitForRawInputDrained(
				rawInjectCRDrainTimeout,
				rawInjectDrainPollInterval,
			)
			if drainErr != nil {
				// Queue inspection is best-effort. A successful write remains
				// the strongest available completion evidence.
				state.reset()
				return true, nil
			}
			if !drained {
				state.phase = wakeInputRawRescueQueued
				if cfg.debug {
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: rescue submit still queued after %s; waiting for terminal reader\n", waited)
				}
				return false, nil
			}
			state.reset()
			return true, nil

		case wakeInputRawRescueQueued:
			waited, drained, err := waitForRawInputDrained(
				rawInjectCRDrainTimeout,
				rawInjectDrainPollInterval,
			)
			if cfg.debug {
				switch {
				case err != nil:
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: pending submit drain check failed after %s: %v\n", waited, err)
				case !drained:
					_ = writeWakeDiagnostic(cfg, "amq wake [debug]: submit key still queued after %s; waiting for terminal reader\n", waited)
				}
			}
			if err != nil || !drained {
				return false, nil
			}
			state.reset()
			return true, nil

		default:
			return false, fmt.Errorf("invalid pending wake input phase %d", state.phase)
		}
	}
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
		_ = writeWakeDiagnostic(cfg, "amq wake [debug]: inject-via mode, running: %s %s <text>\n", cfg.injectVia, strings.Join(cfg.injectArgs, " "))
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
	waitDelay := timeout / 10
	if waitDelay <= 0 {
		waitDelay = time.Millisecond
	}
	if waitDelay > 100*time.Millisecond {
		waitDelay = 100 * time.Millisecond
	}
	cmd.WaitDelay = waitDelay
	out, runErr := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded || errors.Is(runErr, exec.ErrWaitDelay) {
		if cfg.debug {
			_ = writeWakeDiagnostic(cfg, "amq wake [debug]: inject-via timed out after %s (%s)\n", timeout, string(out))
		}
		return fmt.Errorf("inject-via timed out after %s", timeout)
	}
	if runErr != nil {
		if cfg.debug {
			_ = writeWakeDiagnostic(cfg, "amq wake [debug]: inject-via failed: %v (%s)\n", runErr, string(out))
		}
		return runErr
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
