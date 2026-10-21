package cli

import "time"

// Dialog guard: a Claude Code hook (amq wake dialog open|close)
// writes <root>/agents/<me>/.wake.dialog while AskUserQuestion, ExitPlanMode
// or a permission dialog is open, and removes it when the dialog closes. The
// wake only reads the marker. Doorbell text and its CR would answer the
// dialog, so no input byte is written while the marker is present.

type wakeDialogState uint8

const (
	wakeDialogClosed wakeDialogState = iota
	wakeDialogOpen
	// wakeDialogUnknown is a marker older than wakeDialogQuietHold, dated in
	// the future, or one that cannot be read. It may still hide an open dialog, so input stays
	// held, but the wake emits attention output instead of waiting silently.
	wakeDialogUnknown
)

const (
	wakeDialogFileName = ".wake.dialog"
	// A held doorbell re-checks the marker this often, so it rings at most
	// this long after the dialog closes.
	wakeDialogRecheck = 2 * time.Second
	// After this long a marker no longer proves a dialog: a closing hook may
	// not have fired (Esc is a user interrupt with no Stop event). Input
	// stays held; only attention output is added.
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
// doorbell. The message stays in the inbox.
func holdWakeForDialog(
	cfg *wakeConfig,
	state wakeDialogState,
	readErr error,
	output wakePayload,
) error {
	now := cfg.wakeDoorbellNow()
	if cfg.dialogHold != state {
		cfg.dialogHold = state
		switch {
		case readErr != nil:
			_ = writeWakeDiagnostic(cfg, "amq wake: holding input: cannot read the dialog marker: %v\n", readErr)
		case state == wakeDialogOpen:
			_ = writeWakeDiagnostic(cfg, "amq wake: holding the doorbell while a Claude Code dialog is open\n")
		default:
			_ = writeWakeDiagnostic(cfg, "amq wake: the dialog marker is older than %s; holding input, attention only\n", wakeDialogQuietHold)
		}
	}
	if state == wakeDialogOpen {
		cfg.doorbell.recordDialogHold(now, wakeDialogRecheck)
		persistWakeDoorbellStatusBestEffort(cfg)
		return nil
	}
	if err := deliverWakeTransientAttention(cfg, output, nil); err != nil {
		return err
	}
	cfg.doorbell.recordDeferredInputAttempt(now)
	persistWakeDoorbellStatusBestEffort(cfg)
	return nil
}
