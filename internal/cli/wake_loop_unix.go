//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/fsnotify/fsnotify"
)

// wakeLoop is the state of one running wake event loop. Its methods are the
// loop phases; runWakeLoop owns every deferred release, so a method return
// never closes a watcher, stops a timer, or stops signal delivery.
type wakeLoop struct {
	cfg       wakeConfig
	sigCh     chan os.Signal
	restartCh chan os.Signal
	inboxNew  string

	ordinaryRecoverable    bool
	ordinaryAgentDir       *wakeAgentDir
	ownsOrdinaryAgentDir   bool
	ordinaryInboxDir       *wakeInboxDir
	startupBaselineWatcher wakeEventWatcher
	watcher                wakeEventWatcher

	debounceTimer               *time.Timer
	pendingNotify               bool
	terminalAuthorityRetryTimer *time.Timer
	terminalAuthorityRetryC     <-chan time.Time
	inboxScanRetryTimer         *time.Timer
	inboxScanRetryC             <-chan time.Time
	inboxScanFailures           uint
	doorbellTimer               *time.Timer
	doorbellTimerC              <-chan time.Time
	unreadableGenerationNotice  wakeUnreadableGenerationNoticeState

	maintenanceOutputs []*os.File
	preconditionCheck  func(*wakeConfig) error
}

func runWakeLoop(cfg wakeConfig) error {
	// Register shutdown handling before any watcher setup, baseline work, or
	// readiness callback so an early parent death cannot be lost.
	signal.Ignore(syscall.SIGTTOU, syscall.SIGTSTP, syscall.SIGTTIN)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	restartCh := cfg.restartSignals
	if restartCh == nil {
		restartCh = make(chan os.Signal, 1)
		signal.Notify(restartCh, syscall.SIGUSR1)
		defer signal.Stop(restartCh)
		cfg.restartSignals = restartCh
	}
	select {
	case <-cfg.controlStop:
		return nil
	case <-sigCh:
		return nil
	default:
	}

	l := &wakeLoop{
		cfg:                 cfg,
		sigCh:               sigCh,
		restartCh:           restartCh,
		inboxNew:            fsq.AgentInboxNew(cfg.root, cfg.me),
		ordinaryRecoverable: cfg.retainedInbox == nil,
	}
	if retained, ok := l.cfg.retainedAgent.(*wakeAgentDir); ok {
		l.ordinaryAgentDir = retained
	}
	defer l.closeWatchers()
	if err := l.provisionAgentDir(); err != nil {
		return err
	}
	if ok, err := l.armMainWatcher(); !ok {
		return err
	}
	if ok, err := l.prepareStartupBaseline(); !ok {
		return err
	}
	if ok, err := l.rearmBeforeAdmission(); !ok {
		return err
	}
	if ok, err := l.publishAdmission(); !ok {
		return err
	}

	defer l.stopTimers()

	// Recheck non-invasive injection preconditions and silently reconcile
	// durable pending work every 30s. Independent doorbell deadlines keep
	// re-notifying unread cohorts, with separate input and attention cadences.
	maintenanceTicks := l.cfg.maintenanceTicks
	var maintenanceTicker *time.Ticker
	if maintenanceTicks == nil {
		maintenanceTicker = time.NewTicker(30 * time.Second)
		maintenanceTicks = maintenanceTicker.C
		defer maintenanceTicker.Stop()
	}
	// Poll .wake.settings: agent-directory watch events are not delivered, so
	// a live change is seen within one tick even with no inbox activity.
	settingsTicks := l.cfg.settingsTicks
	if settingsTicks == nil && l.cfg.settingsSource != nil {
		settingsTicker := time.NewTicker(wakeSettingsPollInterval)
		settingsTicks = settingsTicker.C
		defer settingsTicker.Stop()
	}
	l.maintenanceOutputs = l.cfg.maintenanceOutputs
	if l.maintenanceOutputs == nil {
		l.maintenanceOutputs = []*os.File{os.Stdout, os.Stderr}
	}
	l.preconditionCheck = l.cfg.preconditionCheck
	if l.preconditionCheck == nil {
		l.preconditionCheck = func(cfg *wakeConfig) error {
			return wakeInjectionPreconditionCheck(cfg, controllingTerminalOpenable)
		}
	}

	// Touch presence immediately so `amq who` shows agent as active
	l.touchPresence()

	// Notify if messages already exist
	if err := l.attemptNotification(); err != nil {
		return err
	}
	return l.run(maintenanceTicks, settingsTicks)
}

func (l *wakeLoop) closeWatchers() {
	if l.startupBaselineWatcher != nil {
		_ = l.startupBaselineWatcher.Close()
	}
	if l.watcher != nil {
		_ = l.watcher.Close()
	}
	_ = l.ordinaryInboxDir.Close()
	if l.ownsOrdinaryAgentDir && l.ordinaryAgentDir != nil {
		_ = l.ordinaryAgentDir.Close()
	}
}

func (l *wakeLoop) stopTimers() {
	if l.terminalAuthorityRetryTimer != nil {
		l.terminalAuthorityRetryTimer.Stop()
	}
	if l.inboxScanRetryTimer != nil {
		l.inboxScanRetryTimer.Stop()
	}
	if l.doorbellTimer != nil {
		l.doorbellTimer.Stop()
	}
}

func (l *wakeLoop) touchPresence() {
	if l.cfg.touchPresence != nil {
		_ = l.cfg.touchPresence()
	} else {
		_ = presence.Touch(l.cfg.root, l.cfg.me)
	}
}

func (l *wakeLoop) provisionAgentDir() error {
	if l.ordinaryRecoverable {
		if l.ordinaryAgentDir == nil {
			// Direct loop tests and legacy in-process callers do not acquire the
			// wake lock first. Provision only for those unowned invocations.
			if err := os.MkdirAll(l.inboxNew, 0o700); err != nil {
				return err
			}
			var err error
			l.ordinaryAgentDir, err = openWakeAgentDir(l.cfg.root, l.cfg.me)
			if err != nil {
				return err
			}
			l.ownsOrdinaryAgentDir = true
		} else if err := validateCanonicalWakeAgentDir(l.ordinaryAgentDir); err != nil {
			return fmt.Errorf("validate acquired wake agent authority: %w", err)
		}
		if l.cfg.touchPresence == nil {
			l.cfg.touchPresence = func() error {
				return touchWakePresenceInDir(l.ordinaryAgentDir, l.cfg.me)
			}
		}
	} else if _, ok := l.cfg.retainedInbox.(*wakeInboxDir); !ok {
		if err := os.MkdirAll(l.inboxNew, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (l *wakeLoop) openMainWatcher() error {
	if l.ordinaryRecoverable {
		inboxDir, err := openWakeRepairInboxDir(l.ordinaryAgentDir)
		if err != nil {
			if validateErr := validateCanonicalWakeAgentDir(l.ordinaryAgentDir); validateErr != nil {
				return errors.Join(
					fmt.Errorf("open acquired wake inbox capability: %w", err),
					fmt.Errorf(
						"validate acquired wake agent authority after inbox failure: %w",
						validateErr,
					),
				)
			}
			return fmt.Errorf(
				"open acquired wake inbox capability: %w",
				err,
			)
		}
		nextWatcher, err := newWakeInboxEventWatcher(inboxDir)
		if err != nil {
			return errors.Join(err, inboxDir.Close())
		}
		l.ordinaryInboxDir = inboxDir
		l.watcher = nextWatcher
		l.cfg.retainedInbox = inboxDir
		l.cfg.retainedInboxRebindable = true
		return nil
	}
	if retained, ok := l.cfg.retainedInbox.(*wakeInboxDir); ok {
		nextWatcher, err := newWakeInboxEventWatcher(retained)
		if err != nil {
			if validateErr := retained.ValidateCanonical(); validateErr != nil {
				return errors.Join(
					err,
					fmt.Errorf(
						"validate retained wake inbox authority after watcher failure: %w",
						validateErr,
					),
				)
			}
			return err
		}
		l.watcher = nextWatcher
		return nil
	}

	nextWatcher, err := newWakePathEventWatcher(l.inboxNew)
	if err == nil {
		l.watcher = nextWatcher
	}
	return err
}

// armMainWatcher returns false with a nil error when a stop arrives during
// the retry backoff.
func (l *wakeLoop) armMainWatcher() (bool, error) {
	for failures := uint(1); ; failures++ {
		err := l.openMainWatcher()
		if err == nil {
			return true, nil
		}
		if classifyWakeFailure(err) == wakeFailureFatal {
			return false, err
		}
		delay := wakeStartupRetryBackoff(failures)
		_ = writeWakeDiagnostic(
			&l.cfg,
			"amq wake: create wake inbox watcher: %v; retrying in %s\n",
			err,
			delay,
		)
		if !waitWakeRetry(l.cfg.controlStop, l.sigCh, delay) {
			return false, nil
		}
	}
}

// prepareStartupBaseline returns false with a nil error when a stop arrives
// during the retry backoff.
func (l *wakeLoop) prepareStartupBaseline() (bool, error) {
	if !l.cfg.baselineRequested || l.cfg.baselineInherited {
		if err := prepareWakeBaselineEvents(
			&l.cfg,
			l.watcher.Events(),
			l.watcher.Errors(),
			l.inboxNew,
		); err != nil {
			return false, err
		}
		return true, nil
	}
	retained, ok := l.cfg.retainedInbox.(*wakeInboxDir)
	if !ok {
		return false, fmt.Errorf("wake baseline requires a retained inbox capability")
	}
	var failures uint
	historyUncertain := false
	for {
		var prepareErr error
		l.startupBaselineWatcher, prepareErr = newWakeBaselineEventWatcher(retained.path)
		if prepareErr == nil {
			prepareErr = retained.ValidateCanonical()
		}
		if prepareErr == nil {
			// The startup boundary is watcher installation, not lock
			// acquisition; messages delivered in between are intentionally
			// treated as startup backlog.
			prepareErr = prepareWakeBaselineEvents(
				&l.cfg,
				l.startupBaselineWatcher.Events(),
				l.startupBaselineWatcher.Errors(),
				l.inboxNew,
			)
		}
		if l.startupBaselineWatcher != nil {
			closeErr := l.startupBaselineWatcher.Close()
			l.startupBaselineWatcher = nil
			if prepareErr == nil && closeErr != nil {
				_ = writeWakeDiagnostic(
					&l.cfg,
					"amq wake: close startup baseline watcher: %v; continuing\n",
					closeErr,
				)
			}
		}
		if prepareErr == nil {
			if historyUncertain {
				// The original watcher lost event history. Reaching a new
				// barrier proves readiness, but treating all backlog as new
				// is the only lossless delivery fallback.
				l.cfg.baselineExisting = nil
			}
			return true, nil
		}
		l.cfg.baselineExisting = nil
		if err := retained.ValidateCanonical(); err != nil {
			validationErr := fmt.Errorf(
				"validate startup baseline watcher authority: %w",
				err,
			)
			if classifyWakeFailure(validationErr) == wakeFailureFatal {
				return false, validationErr
			}
			prepareErr = errors.Join(prepareErr, validationErr)
		}
		historyUncertain = true
		failures++
		delay := wakeStartupRetryBackoff(failures)
		_ = writeWakeDiagnostic(
			&l.cfg,
			"amq wake: startup baseline watcher failed: %v; retrying in %s\n",
			prepareErr,
			delay,
		)
		if !waitWakeRetry(l.cfg.controlStop, l.sigCh, delay) {
			return false, nil
		}
	}
}

// rearmBeforeAdmission: a separate baseline watcher can take long enough for
// the main watcher to fail before admission. Rearm it and discard startup
// tombstones so messages from the uncertain interval are delivered instead of
// publishing false readiness with an already-dead watcher. It returns false
// with a nil error when a stop arrives during the rearm backoff.
func (l *wakeLoop) rearmBeforeAdmission() (bool, error) {
	for {
		if err := pendingWakeWatcherError(l.watcher); err == nil {
			return true, nil
		} else {
			l.cfg.baselineExisting = nil
			_ = l.watcher.Close()
			l.watcher = nil
			if l.ordinaryRecoverable {
				_ = l.ordinaryInboxDir.Close()
				l.ordinaryInboxDir = nil
				l.cfg.retainedInbox = nil
				l.cfg.retainedInboxRebindable = false
			}
			_ = writeWakeDiagnostic(
				&l.cfg,
				"amq wake: main watcher failed before admission: %v; rearming\n",
				err,
			)
		}
		if ok, err := l.armMainWatcher(); !ok {
			return false, err
		}
	}
}

// publishAdmission returns false with a nil error when a stop is already
// pending before or after readiness publication.
func (l *wakeLoop) publishAdmission() (bool, error) {
	if retained, ok := l.cfg.retainedInbox.(*wakeInboxDir); ok {
		if err := retained.ValidateCanonical(); err != nil {
			return false, fmt.Errorf("validate wake inbox before admission publication: %w", err)
		}
	}
	if l.ordinaryAgentDir != nil {
		if err := validateCanonicalWakeAgentDir(l.ordinaryAgentDir); err != nil {
			return false, fmt.Errorf("validate wake agent before admission publication: %w", err)
		}
	}
	if l.cfg.onBaselineReady != nil {
		if err := l.cfg.onBaselineReady(cloneWakeFileIdentities(l.cfg.baselineExisting)); err != nil {
			return false, err
		}
	}
	// This closes the already-pending stop case only; a stop or process death can
	// still race immediately after readiness publication.
	select {
	case <-l.cfg.controlStop:
		return false, nil
	case <-l.sigCh:
		return false, nil
	default:
	}
	if l.cfg.onPrepared != nil {
		if err := l.cfg.onPrepared(l.watcher); err != nil {
			return false, err
		}
	}
	select {
	case <-l.cfg.controlStop:
		return false, nil
	case <-l.sigCh:
		return false, nil
	default:
	}
	return true, nil
}

func (l *wakeLoop) clearTerminalAuthorityRetry() {
	l.terminalAuthorityRetryC = nil
	if l.terminalAuthorityRetryTimer == nil {
		return
	}
	if !l.terminalAuthorityRetryTimer.Stop() {
		select {
		case <-l.terminalAuthorityRetryTimer.C:
		default:
		}
	}
}

func (l *wakeLoop) scheduleTerminalAuthorityRetry() {
	if l.terminalAuthorityRetryTimer == nil {
		l.terminalAuthorityRetryTimer = time.NewTimer(wakeTerminalAuthorityRetryDelay)
	} else {
		if !l.terminalAuthorityRetryTimer.Stop() {
			select {
			case <-l.terminalAuthorityRetryTimer.C:
			default:
			}
		}
		l.terminalAuthorityRetryTimer.Reset(wakeTerminalAuthorityRetryDelay)
	}
	l.terminalAuthorityRetryC = l.terminalAuthorityRetryTimer.C
}

func (l *wakeLoop) clearInboxScanRetry() {
	l.inboxScanRetryC = nil
	if l.inboxScanRetryTimer == nil {
		return
	}
	if !l.inboxScanRetryTimer.Stop() {
		select {
		case <-l.inboxScanRetryTimer.C:
		default:
		}
	}
}

func (l *wakeLoop) scheduleInboxScanRetry(delay time.Duration) {
	if l.inboxScanRetryTimer == nil {
		l.inboxScanRetryTimer = time.NewTimer(delay)
	} else {
		if !l.inboxScanRetryTimer.Stop() {
			select {
			case <-l.inboxScanRetryTimer.C:
			default:
			}
		}
		l.inboxScanRetryTimer.Reset(delay)
	}
	l.inboxScanRetryC = l.inboxScanRetryTimer.C
}

func (l *wakeLoop) clearDoorbellDeadline() {
	l.doorbellTimerC = nil
	if l.doorbellTimer != nil && !l.doorbellTimer.Stop() {
		select {
		case <-l.doorbellTimer.C:
		default:
		}
	}
}

func (l *wakeLoop) scheduleDoorbellDeadline() {
	if l.terminalAuthorityRetryC != nil ||
		l.inboxScanRetryC != nil {
		l.clearDoorbellDeadline()
		return
	}
	deadline, ok := l.cfg.doorbell.nextDeadline()
	if !ok {
		l.clearDoorbellDeadline()
		return
	}
	delay := deadline.Sub(l.cfg.wakeDoorbellNow())
	if delay < 0 {
		delay = 0
	}
	if l.doorbellTimer == nil {
		l.doorbellTimer = time.NewTimer(delay)
	} else {
		if !l.doorbellTimer.Stop() {
			select {
			case <-l.doorbellTimer.C:
			default:
			}
		}
		l.doorbellTimer.Reset(delay)
	}
	l.doorbellTimerC = l.doorbellTimer.C
}

func (l *wakeLoop) retryWatcher(context string, cause error) {
	l.cfg.baselineExisting = nil
	if l.watcher != nil {
		_ = l.watcher.Close()
		l.watcher = nil
	}
	if l.ordinaryRecoverable {
		_ = l.ordinaryInboxDir.Close()
		l.ordinaryInboxDir = nil
		l.cfg.retainedInbox = nil
		l.cfg.retainedInboxRebindable = false
	}
	l.pendingNotify = true
	l.clearTerminalAuthorityRetry()
	l.inboxScanFailures++
	l.scheduleInboxScanRetry(wakeInboxScanRetryBackoff(l.inboxScanFailures))
	l.clearDoorbellDeadline()
	if cause == nil {
		_ = writeWakeDiagnostic(&l.cfg, "amq wake: %s; retrying watcher setup\n", context)
		return
	}
	_ = writeWakeDiagnostic(&l.cfg, "amq wake: %s: %v; retrying watcher setup\n", context, cause)
}

func (l *wakeLoop) scheduleWatcherRebindFailure(context string, cause error) {
	l.pendingNotify = true
	l.inboxScanFailures++
	l.scheduleInboxScanRetry(wakeInboxScanRetryBackoff(l.inboxScanFailures))
	l.clearDoorbellDeadline()
	_ = writeWakeDiagnostic(
		&l.cfg,
		"amq wake: %s: %v; retrying watcher setup\n",
		context,
		cause,
	)
}

func (l *wakeLoop) rebindWatcher() (bool, error) {
	if l.watcher != nil {
		return true, nil
	}
	if l.ordinaryRecoverable {
		if err := validateCanonicalWakeAgentDir(l.ordinaryAgentDir); err != nil {
			return false, failWakeOnWatcherError(
				&l.cfg,
				"ordinary wake agent authority changed while rearming watcher",
				err,
			)
		}
		inboxDir, nextWatcher, err := openWatchedWakeInboxDir(l.ordinaryAgentDir)
		if err != nil {
			l.scheduleWatcherRebindFailure(
				"ordinary wake inbox is unavailable while rearming watcher",
				err,
			)
			return false, nil
		}
		l.ordinaryInboxDir = inboxDir
		l.watcher = nextWatcher
		l.cfg.retainedInbox = inboxDir
		l.cfg.retainedInboxRebindable = true
		return true, nil
	}

	if retained, ok := l.cfg.retainedInbox.(*wakeInboxDir); ok {
		if err := retained.ValidateCanonical(); err != nil {
			return false, failWakeOnWatcherError(
				&l.cfg,
				"retained wake inbox authority changed while rearming watcher",
				err,
			)
		}
		nextWatcher, err := newWakeInboxEventWatcher(retained)
		if err != nil {
			l.scheduleWatcherRebindFailure(
				"retained wake inbox watcher is unavailable while rearming",
				err,
			)
			return false, nil
		}
		l.watcher = nextWatcher
		return true, nil
	}

	nextWatcher, err := newWakePathEventWatcher(l.inboxNew)
	if err != nil {
		l.scheduleWatcherRebindFailure(
			"wake inbox watcher is unavailable while rearming",
			err,
		)
		return false, nil
	}
	l.watcher = nextWatcher
	return true, nil
}

func (l *wakeLoop) enterRetainedInputRecovery(cause error) error {
	markWakeInputRecoveryRequired(&l.cfg, cause)
	l.cfg.doorbell.retainRecoveryRequired(l.cfg.wakeDoorbellNow())
	l.pendingNotify = false
	l.clearTerminalAuthorityRetry()
	if err := emitWakeAttention(&l.cfg, wakePayload{
		text:       wakeInputRecoveryNotice,
		provenance: wakePayloadSystemFixed,
	}); err == nil {
		l.cfg.doorbell.recordRecoveryAttentionDelivered()
	}
	l.scheduleDoorbellDeadline()
	return nil
}

func (l *wakeLoop) attemptNotification() error {
	defer func() {
		if l.cfg.onNotificationAttemptComplete != nil {
			l.cfg.onNotificationAttemptComplete()
		}
	}()
	if l.terminalAuthorityRetryC != nil || l.inboxScanRetryC != nil {
		return nil
	}
	err := notifyNewMessages(&l.cfg)
	// A canonical mismatch (wakeInboxCanonicalMismatchError) is handled
	// BEFORE the ordinary scan-retry path: rebindWatcher returns at once
	// while watcher != nil, so the watcher must be invalidated here.
	var canonicalMismatch *wakeInboxCanonicalMismatchError
	if errors.As(err, &canonicalMismatch) {
		switch classifyCanonicalMismatch(err, l.cfg.retainedInboxRebindable) {
		case canonicalMismatchRebindableRearm:
			// Rebindable (ordinary-recoverable): nil the watcher + inbox so
			// rebindWatcher re-arms on the new canonical inbox. A legitimate
			// remove/recreate is a rearm, not a fatal swap.
			if l.watcher != nil {
				_ = l.watcher.Close()
				l.watcher = nil
			}
			if l.ordinaryInboxDir != nil {
				_ = l.ordinaryInboxDir.Close()
				l.ordinaryInboxDir = nil
			}
			l.cfg.retainedInbox = nil
			l.cfg.retainedInboxRebindable = false
			l.pendingNotify = true
			l.clearTerminalAuthorityRetry()
			l.inboxScanFailures++
			l.scheduleInboxScanRetry(wakeInboxScanRetryBackoff(l.inboxScanFailures))
			l.clearDoorbellDeadline()
			_ = writeWakeDiagnostic(&l.cfg, "amq wake: canonical inbox mismatch (rebindable): rearming watcher: %v\n", err)
			return nil
		case canonicalMismatchFatal:
			// !Rebindable: terminal exit (mirrors rebindWatcher's retained-
			// authority handling). classifyWakeFailure also returns fatal.
			l.clearInboxScanRetry()
			l.inboxScanFailures = 0
			return err
		}
	}
	var scanErr *wakeInboxScanError
	if errors.As(err, &scanErr) {
		l.pendingNotify = true
		l.clearTerminalAuthorityRetry()
		l.inboxScanFailures++
		l.scheduleInboxScanRetry(wakeInboxScanRetryBackoff(l.inboxScanFailures))
		l.clearDoorbellDeadline()
		_ = writeWakeDiagnostic(&l.cfg, "amq wake: notify error: %v\n", err)
		return nil
	}
	l.clearInboxScanRetry()
	l.inboxScanFailures = 0
	disposition := classifyWakeFailure(err)
	if disposition == wakeFailureFatal {
		return err
	}
	if isWakeTerminalForegroundPGRPChanged(err) {
		l.pendingNotify = true
		l.scheduleTerminalAuthorityRetry()
		l.clearDoorbellDeadline()
		if l.cfg.debug {
			_ = writeWakeDiagnostic(
				&l.cfg,
				"amq wake [debug]: holding notification until foreground process group is restored: %v\n",
				err,
			)
		}
		return nil
	}
	if isWakeTerminalPartialProgress(err) {
		l.pendingNotify = true
		l.scheduleTerminalAuthorityRetry()
		l.clearDoorbellDeadline()
		if l.cfg.debug {
			_ = writeWakeDiagnostic(
				&l.cfg,
				"amq wake [debug]: holding partial terminal delivery for exact suffix retry: %v\n",
				err,
			)
		}
		return nil
	}
	if isWakeInjectorDeferred(err) {
		// The provider refused before dispatch. The transport already
		// retained the cohort and armed its input retry deadline; keep this
		// loop silent and let that existing deadline drive the next attempt.
		l.pendingNotify = false
		l.clearTerminalAuthorityRetry()
		l.scheduleDoorbellDeadline()
		if l.cfg.debug {
			_ = writeWakeDiagnostic(&l.cfg, "amq wake [debug]: provider deferred notification; retaining cohort for retry: %v\n", err)
		}
		return nil
	}
	var attentionErr *wakeAttentionDeliveryError
	if errors.As(err, &attentionErr) &&
		!isWakeTerminalAuthorityLoss(err) {
		l.pendingNotify = false
		l.clearTerminalAuthorityRetry()
		l.scheduleDoorbellDeadline()
		return nil
	}
	if disposition == wakeFailureDegrade {
		return l.enterRetainedInputRecovery(err)
	}
	l.pendingNotify = false
	l.clearTerminalAuthorityRetry()
	l.scheduleDoorbellDeadline()
	if err == nil {
		return nil
	}
	_ = writeWakeDiagnostic(&l.cfg, "amq wake: notify error: %v\n", err)
	return nil
}

func (l *wakeLoop) emitScanFallback() error {
	if err := emitWakeAttention(&l.cfg, wakePayload{
		text:       "AMQ messages may be pending; run amq drain --include-body",
		provenance: wakePayloadSystemFixed,
	}); err != nil {
		return err
	}
	l.pendingNotify = false
	l.clearInboxScanRetry()
	return nil
}

// run is the event loop. It returns nil on a stop or shutdown signal and the
// first fatal error otherwise.
func (l *wakeLoop) run(maintenanceTicks, settingsTicks <-chan time.Time) error {
	for {
		var debounceC <-chan time.Time
		if l.debounceTimer != nil {
			debounceC = l.debounceTimer.C
		}
		var watcherEvents <-chan fsnotify.Event
		var watcherErrors <-chan error
		if l.watcher != nil {
			watcherEvents = l.watcher.Events()
			watcherErrors = l.watcher.Errors()
		}

		select {
		case <-l.cfg.controlStop:
			return nil
		case <-l.sigCh:
			// Clean exit on SIGHUP/SIGTERM
			return nil
		case <-l.restartCh:
			handleWakeRestartAtLoopBoundary(
				&l.cfg,
				l.watcher,
				l.terminalAuthorityRetryC != nil,
				l.inboxScanRetryC != nil,
			)

		case event, ok := <-watcherEvents:
			if !ok {
				l.retryWatcher("wake inbox watcher closed", nil)
				continue
			}
			l.onWatcherEvent(event)

		case err, ok := <-watcherErrors:
			if !ok {
				l.retryWatcher("wake inbox watcher closed", nil)
				continue
			}
			l.retryWatcher("wake inbox watcher failed", err)
			continue

		case <-debounceC:
			if !l.pendingNotify {
				continue
			}

			// Collect and notify
			if err := l.attemptNotification(); err != nil {
				return err
			}

		case <-l.terminalAuthorityRetryC:
			l.terminalAuthorityRetryC = nil
			if !l.pendingNotify {
				continue
			}
			if err := l.attemptNotification(); err != nil {
				return err
			}

		case <-l.inboxScanRetryC:
			l.inboxScanRetryC = nil
			if err := l.onInboxScanRetry(); err != nil {
				return err
			}

		case <-l.doorbellTimerC:
			l.doorbellTimerC = nil
			if err := l.attemptNotification(); err != nil {
				return err
			}

		case <-settingsTicks:
			// Only a hold-policy change needs a scan now: it re-times held
			// cohorts. A scan for any other key could resend an interrupt
			// key for mail that is still unread; those keys apply at next use.
			if !l.cfg.reloadSettings() || !l.cfg.holdPolicyChanged {
				continue
			}
			l.pendingNotify = true
			if err := l.attemptNotification(); err != nil {
				return err
			}

		case <-maintenanceTicks:
			if err := l.maintain(); err != nil {
				return err
			}
		}
	}
}

func (l *wakeLoop) onWatcherEvent(event fsnotify.Event) {
	invalidateWakeBaselineEvent(&l.cfg, event)
	// Creates/additions can accelerate delivery; removals are durable
	// progress and must rearm the generation without waiting for the
	// maintenance reconciliation fallback.
	if event.Op&(fsnotify.Create|fsnotify.Rename|fsnotify.Write|fsnotify.Remove) == 0 {
		return
	}
	// Skip non-.md files
	if !strings.HasSuffix(event.Name, ".md") {
		return
	}

	// Start or reset debounce timer
	l.pendingNotify = true
	if l.cfg.onPendingNotify != nil {
		l.cfg.onPendingNotify()
	}
	if l.debounceTimer == nil {
		l.debounceTimer = time.NewTimer(l.cfg.settings.debounce)
	} else {
		if !l.debounceTimer.Stop() {
			select {
			case <-l.debounceTimer.C:
			default:
			}
		}
	}
	l.debounceTimer.Reset(l.cfg.settings.debounce)
}

func (l *wakeLoop) onInboxScanRetry() error {
	rebound, err := l.rebindWatcher()
	if err != nil {
		if classifyWakeFailure(err) == wakeFailureFatal {
			return err
		}
		l.scheduleWatcherRebindFailure(
			"wake inbox watcher rearm failed",
			err,
		)
		return nil
	}
	if !rebound {
		return nil
	}
	return l.attemptNotification()
}

func (l *wakeLoop) maintain() error {
	if err := maintainWakeOutputBounds(l.maintenanceOutputs...); err != nil {
		_ = writeWakeDiagnostic(
			&l.cfg,
			"amq wake: maintain output bound: %v; continuing without truncation\n",
			err,
		)
	}
	pendingInputWork := !l.cfg.inputRecoveryRequired &&
		(l.cfg.doorbell.pendingInput() || l.cfg.inputDelivery.pending())
	hadPendingInputWork := l.pendingNotify || pendingInputWork
	if pendingInputWork {
		if err := l.attemptNotification(); err != nil {
			return err
		}
	}

	// Keep presence alive so `amq who` reports the agent as active
	l.touchPresence()
	if err := retryPendingWakeNotifierStatus(&l.cfg); err != nil {
		_ = writeWakeDiagnostic(
			&l.cfg,
			"amq wake: persist notifier status: %v; retrying\n",
			err,
		)
	}
	if err := retryPendingWakeDoorbellStatus(&l.cfg); err != nil {
		_ = writeWakeDiagnostic(
			&l.cfg,
			"amq wake: persist doorbell parked status: %v; retrying\n",
			err,
		)
	}
	if l.cfg.inputRecoveryRequired {
		l.unreadableGenerationNotice.resetWithoutStatusWrite()
		l.scheduleDoorbellDeadline()
		return nil
	}

	inputModeBeforeCheck := l.cfg.injectMode
	inputEnabledBeforeCheck := inputModeBeforeCheck != wakeInjectModeNone
	inputStateBeforeCheck := l.cfg.inputDelivery
	doorbellBeforeCheck := l.cfg.doorbell
	preconditionErr := l.preconditionCheck(&l.cfg)
	if inputEnabledBeforeCheck &&
		l.cfg.injectMode == wakeInjectModeNone &&
		inputStateBeforeCheck.blocksDemotion() {
		l.cfg.injectMode = inputModeBeforeCheck
		l.cfg.inputDelivery = inputStateBeforeCheck
		l.cfg.doorbell = doorbellBeforeCheck
		preconditionErr = errors.Join(
			preconditionErr,
			&wakeInputDemotionBlockedError{
				err: errors.New("maintenance attempted output-only demotion before terminal input completed"),
			},
		)
	}
	if isWakeInputDemotionBlocked(preconditionErr) {
		l.unreadableGenerationNotice.resetWithoutStatusWrite()
		return l.enterRetainedInputRecovery(preconditionErr)
	}
	if !inputEnabledBeforeCheck && l.cfg.injectMode != wakeInjectModeNone {
		l.cfg.doorbell.makeDue(l.cfg.wakeDoorbellNow())
		l.pendingNotify = true
		if err := l.attemptNotification(); err != nil {
			return err
		}
	}
	if l.cfg.injectMode == wakeInjectModeNone {
		if inputEnabledBeforeCheck {
			if err := retireWakeInputState(
				&l.cfg,
				errors.New("maintenance transferred pending work to output-only delivery"),
			); err != nil {
				l.cfg.injectMode = inputModeBeforeCheck
				preconditionErr = errors.Join(preconditionErr, err)
				l.scheduleDoorbellDeadline()
				return preconditionErr
			}
		} else {
			clearWakeInputState(&l.cfg)
		}
		l.clearTerminalAuthorityRetry()
		if inputEnabledBeforeCheck && hadPendingInputWork {
			l.pendingNotify = true
			if preconditionErr != nil && l.inboxScanRetryC != nil {
				preconditionErr = errors.Join(
					preconditionErr,
					l.emitScanFallback(),
				)
			} else {
				if err := l.attemptNotification(); err != nil {
					return err
				}
				if preconditionErr != nil && l.pendingNotify && l.inboxScanRetryC != nil {
					preconditionErr = errors.Join(
						preconditionErr,
						l.emitScanFallback(),
					)
				}
			}
		} else if inputEnabledBeforeCheck {
			l.pendingNotify = false
		}
	}
	l.scheduleDoorbellDeadline()
	if preconditionErr != nil {
		if classifyWakeFailure(preconditionErr) == wakeFailureFatal {
			return preconditionErr
		}
		_ = writeWakeDiagnostic(
			&l.cfg,
			"amq wake: maintenance precondition failed: %v; retrying\n",
			preconditionErr,
		)
	}
	if agentDir, ok := l.cfg.retainedAgent.(*wakeAgentDir); ok &&
		agentDir != nil && l.cfg.inspectTerminalGeneration != nil {
		if err := maintainWakeSelfUpgradeAtLoopBoundary(
			&l.cfg,
			agentDir,
			l.watcher,
			l.terminalAuthorityRetryC != nil,
			l.inboxScanRetryC != nil,
		); err != nil {
			_ = writeWakeDiagnostic(
				&l.cfg,
				"amq wake: self-upgrade maintenance: %v; retrying\n",
				err,
			)
		}
	}
	l.unreadableGenerationNotice.observe(&l.cfg)
	return nil
}
