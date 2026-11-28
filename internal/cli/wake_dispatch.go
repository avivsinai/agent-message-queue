package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
)

var errWakeDoorbellStale = errors.New("peer doorbell cancelled: no qualifying message remains in the scanned cohort")

func validateDispatchInbox(path string, inbox wakeInboxReader) error {
	if validator, ok := inbox.(interface{ ValidateCanonical() error }); ok {
		if err := validator.ValidateCanonical(); err != nil {
			return &wakeInboxCanonicalMismatchError{detachedPath: path, cause: err}
		}
	}
	return nil
}

func (cfg *wakeConfig) checkPeerDispatch() error {
	if cfg == nil || cfg.dispatchCohort == nil {
		return nil
	}
	return checkDoorbellFiles(fsq.AgentInboxNew(cfg.root, cfg.me), cfg.retainedInbox, cfg.dispatchCohort)
}

// The scan already applied wakeEligible. On the wake platforms an unchanged
// device, inode and ctime proves that the same qualifying file still waits.
// New arrivals and same-name replacements get their own scan and attribution.
func checkDoorbellFiles(path string, retained wakeInboxReader, cohort map[string]os.FileInfo) error {
	if err := validateDispatchInbox(path, retained); err != nil {
		return err
	}
	var unknown error
	for name, scanned := range cohort {
		var info os.FileInfo
		var err error
		if inbox, ok := retained.(wakeInboxFileInfoReader); ok {
			info, err = inbox.FileInfo(name)
		} else {
			info, err = os.Lstat(filepath.Join(path, name))
		}
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			unknown = err
		case scanned == nil:
			unknown = fmt.Errorf("missing scanned identity for %s", name)
		case info.Mode().IsRegular() && sameWakeFileIdentity(scanned, info):
			return validateDispatchInbox(path, retained)
		}
	}
	if err := validateDispatchInbox(path, retained); err != nil {
		return err
	}
	if unknown != nil {
		return &wakeInboxScanError{err: unknown}
	}
	return errWakeDoorbellStale
}

func isWakeDispatchRefusal(err error) bool {
	var scan *wakeInboxScanError
	var mismatch *wakeInboxCanonicalMismatchError
	return errors.Is(err, errWakeDoorbellStale) || errors.As(err, &scan) || errors.As(err, &mismatch)
}

func (cfg *wakeConfig) cancelPeerDoorbell() {
	cohort := cfg.doorbell.cohort
	cfg.doorbell.reset()
	if !cfg.inputDelivery.oneSubmit {
		if cfg.inputDelivery.blocksDemotion() {
			// No further byte is authorized. Keep the exact input debt until
			// the ordinary recovery scan observes physical consumer progress.
			markWakeInputRecoveryRequired(cfg, errWakeDoorbellStale)
			cfg.doorbell.phase = wakeDoorbellRecoveryRequired
			cfg.doorbell.cohort = cohort
		} else {
			clearWakeInputState(cfg)
		}
	}
	cfg.closeCancelledProviderAttempt()
	persistWakeDoorbellStatusBestEffort(cfg)
	_ = writeWakeDiagnostic(cfg, "amq wake: %v\n", errWakeDoorbellStale)
}

func (cfg *wakeConfig) closeCancelledProviderAttempt() {
	if attempt := cfg.notificationAttempt; attempt != nil {
		writer := notificationattempt.NewWriter(cfg.root, cfg.me)
		if err := writer.Transition(attempt, notificationattempt.StateCancelled, errWakeDoorbellStale.Error()); err != nil {
			_ = writeWakeDiagnostic(cfg, "amq wake: record cancelled notification: %v\n", err)
		}
		cfg.notificationAttempt, cfg.notificationAttemptPhysical = nil, nil
	}
}

func wakeCohortStillEligible(previous map[string]*wakeFileIdentity, current map[string]os.FileInfo) bool {
	for name, identity := range previous {
		if info, ok := current[name]; ok && (info == nil || identity == nil || matchesWakeFileIdentity(*identity, info)) {
			return true
		}
	}
	return false
}
