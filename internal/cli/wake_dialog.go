package cli

import "time"

// Dialog guard: a Claude Code hook (amq wake dialog open|close)
// writes <root>/agents/<me>/.wake.dialog while AskUserQuestion, ExitPlanMode
// or a permission dialog is open, and removes it when the dialog closes. The
// wake reads the marker, and removes it only when the Claude Code transcript
// proves the dialog answered. Doorbell text and its CR would answer the
// dialog, so no input byte is written while the marker is present.

type wakeDialogState uint8

const (
	wakeDialogClosed wakeDialogState = iota
	wakeDialogOpen
	// wakeDialogUnknown is a marker older than wakeDialogQuietHold, dated in
	// the future, or one that cannot be read. It may still hide an open
	// dialog, so input stays held; held attempts follow the attention retry
	// ladder instead of the 2-second recheck.
	wakeDialogUnknown
)

const (
	wakeDialogFileName = ".wake.dialog"
	// A held doorbell re-checks the marker this often, so it rings at most
	// this long after the dialog closes.
	wakeDialogRecheck = 2 * time.Second
	// After this long a marker no longer proves a dialog: a closing hook may
	// not have fired (Esc is a user interrupt with no Stop event). Input
	// stays held; the wake stops the 2-second recheck.
	wakeDialogQuietHold = 10 * time.Minute
)

// dialogState reads the marker now. A wake without a dialog source (direct
// loop tests, non-unix builds) has no dialog guard.
func (cfg *wakeConfig) dialogState() (wakeDialogState, error) {
	if cfg.dialogSource == nil {
		return wakeDialogClosed, nil
	}
	return cfg.dialogSource(cfg.wakeDoorbellNow())
}

// classifyWakeDialogMarker maps one marker read to a dialog state.
func classifyWakeDialogMarker(
	modTime time.Time,
	exists bool,
	readErr error,
	now time.Time,
) (wakeDialogState, error) {
	switch {
	case readErr != nil:
		return wakeDialogUnknown, readErr
	case !exists:
		return wakeDialogClosed, nil
	case modTime.After(now.Add(wakeDialogRecheck)):
		// A future mtime (a clock step back, a stray touch) cannot be aged.
		return wakeDialogUnknown, nil
	case now.Sub(modTime) >= wakeDialogQuietHold:
		return wakeDialogUnknown, nil
	default:
		return wakeDialogOpen, nil
	}
}

// holdWakeForDialog keeps the doorbell while a dialog may be open. It runs
// before the notification attempt starts, so it writes no ledger record and
// records no input attempt: the reminder budget and presentation state stay
// as they are, and the next ring after the dialog closes is the full
// doorbell. The message stays in the inbox. The hold is never silent: each
// held attempt goes through presentWakeDialogHold.
func holdWakeForDialog(
	cfg *wakeConfig,
	state wakeDialogState,
	readErr error,
	output wakePayload,
	pending int,
) error {
	now := cfg.wakeDoorbellNow()
	if cfg.dialogHold != state {
		cfg.dialogHold = state
		var detail string
		if cfg.dialogHoldDetail != nil {
			detail = " (" + cfg.dialogHoldDetail(now) + ")"
		}
		switch {
		case readErr != nil:
			_ = writeWakeDiagnostic(cfg, "amq wake: holding input: cannot read the dialog marker: %v\n", readErr)
		case state == wakeDialogOpen:
			_ = writeWakeDiagnostic(cfg, "amq wake: holding the doorbell while a harness dialog is open%s\n", detail)
		default:
			_ = writeWakeDiagnostic(cfg, "amq wake: the dialog marker is older than %s; holding input, attention only%s\n", wakeDialogQuietHold, detail)
		}
	}
	if state == wakeDialogOpen {
		cfg.doorbell.recordDialogHold(now, wakeDialogRecheck)
		persistWakeDoorbellStatusBestEffort(cfg)
		return presentWakeDialogHold(cfg, output, pending)
	}
	if err := presentWakeDialogHold(cfg, output, pending); err != nil {
		return err
	}
	cfg.doorbell.recordDeferredInputAttempt(now)
	persistWakeDoorbellStatusBestEffort(cfg)
	return nil
}

// presentWakeDialogHold shows a held doorbell to the owner: the desktop
// notice (held_notice) and the terminal attention floor, at the first held
// attempt, then once per attention period, and again at once when more
// messages wait than the last notice named. Input stays held throughout.
func presentWakeDialogHold(cfg *wakeConfig, output wakePayload, pending int) error {
	if err := authorizePeerWakeAttention(cfg, output); err != nil {
		return err
	}
	reason := wakeDialogGenericHoldReason
	if cfg.dialogHoldReason != nil {
		text, marker := cfg.dialogHoldReason()
		if !marker {
			// No marker: coop exec's startup hold has not written or
			// cleared the Startup marker yet, or the dialog just closed.
			return nil
		}
		reason = text
	}
	now := cfg.wakeDoorbellNow()
	if !cfg.doorbell.heldNoticeDue(now, pending) {
		cfg.doorbell.heldNoticeCount = min(cfg.doorbell.heldNoticeCount, pending)
		return nil
	}
	cfg.doorbell.recordHeldNotice(now, pending)
	if cfg.settings.heldNotice && cfg.desktopNotify != nil {
		if err := cfg.desktopNotify(wakeDesktopNoticeTitle, wakeHeldNoticeBody(cfg.me, pending, reason)); err != nil {
			_ = writeWakeDiagnostic(cfg, "amq wake: held-doorbell desktop notice failed: %v\n", err)
		}
	}
	return emitWakeAttention(cfg, output)
}
