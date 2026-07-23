//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	wakeReadySchema              = 1
	wakeCatchupReadyFileName     = ".wake.catchup-ready"
	wakeCatchupReadyPollInterval = 20 * time.Millisecond
)

type wakeReady struct {
	Schema       int    `json:"schema"`
	Generation   string `json:"generation"`
	TargetDigest string `json:"target_digest,omitempty"`
}

func writeWakeReadyFile(root, me, path string, expected wakeLockInspection) error {
	if path == "" {
		return nil
	}
	return withWakeLifecycleGuard(root, me, func() error {
		current := inspectWakeLock(root, me)
		if !sameWakeLockGeneration(expected, current) {
			return fmt.Errorf("wake lock generation changed before readiness publication")
		}
		if err := validateWakeReadyLockAndTarget(root, me, current, wakeReady{
			Schema:       wakeReadySchema,
			Generation:   current.Lock.Generation,
			TargetDigest: current.Lock.TargetDigest,
		}); err != nil {
			return err
		}
		return writeWakeGenerationFile(path, "wake ready file", wakeReady{
			Schema:       wakeReadySchema,
			Generation:   current.Lock.Generation,
			TargetDigest: current.Lock.TargetDigest,
		})
	})
}

func writeWakeGenerationFile(path, label string, marker wakeReady) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", label, err)
	}
	return writeWakeMetadataFile(path, append(data, '\n'), label)
}

func readWakeReadyFile(path string) (wakeReady, bool, error) {
	return readWakeGenerationFile(path, "wake ready file")
}

func readWakeGenerationFile(path, label string) (wakeReady, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return wakeReady{}, false, nil
		}
		return wakeReady{}, false, fmt.Errorf("stat %s: %w", label, err)
	}
	if err := validateWakeGenerationFile(path, label, info); err != nil {
		return wakeReady{}, true, err
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		return wakeReady{}, true, fmt.Errorf("open %s: %w", label, err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return wakeReady{}, true, fmt.Errorf("stat opened %s: %w", label, err)
	}
	if err := validateWakeGenerationFile(path, label, openedInfo); err != nil {
		return wakeReady{}, true, err
	}
	if !os.SameFile(info, openedInfo) {
		return wakeReady{}, true, fmt.Errorf("%s %s changed while opening", label, path)
	}
	data, err := readWakeMetadata(file, label, path)
	if err != nil {
		return wakeReady{}, true, err
	}
	var ready wakeReady
	if err := json.Unmarshal(data, &ready); err != nil {
		return wakeReady{}, true, fmt.Errorf("legacy %s refused", label)
	}
	if ready.Schema != wakeReadySchema || ready.Generation == "" {
		return wakeReady{}, true, fmt.Errorf("legacy %s refused", label)
	}
	return ready, true, nil
}

func validateWakeGenerationFile(path, label string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s %s must not be a symlink", label, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s %s must be a regular file", label, path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("%s %s mode is %o, want 0600", label, path, got)
	}
	return validateWakeTargetPathOwnership(label, path, info)
}

func validateWakeReadyLockAndTarget(root, me string, current wakeLockInspection, ready wakeReady) error {
	confirmed := current.Status == wakeLockValid && current.IdentityConfirmed
	if !confirmed && !currentWakeLockMatches(current.Lock) {
		return fmt.Errorf("wake lock is not a confirmed valid wake during readiness validation")
	}
	if current.Lock.Generation == "" || ready.Generation != current.Lock.Generation {
		return fmt.Errorf("wake readiness generation does not match current wake lock")
	}
	if ready.TargetDigest != current.Lock.TargetDigest {
		return fmt.Errorf("wake readiness target does not match current wake lock")
	}
	if current.Lock.TargetDigest == "" {
		_, exists, err := readWakeTarget(root, me)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("wake readiness target does not match current wake lock")
		}
		return nil
	}
	target, exists, err := readWakeTarget(root, me)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("wake readiness target is missing")
	}
	if err := validateWakeTarget(target, root, me); err != nil {
		return err
	}
	return validateWakeTargetMatchesLock(current.Lock, target)
}

func validateWakeReadyFileAgainstCurrent(root, me, path string) (bool, error) {
	// A wake can still die immediately after this guarded validation; the local
	// process notifier has no durable liveness lease beyond the lock generation.
	ready := false
	err := withWakeLifecycleGuard(root, me, func() error {
		published, exists, err := readWakeReadyFile(path)
		if err != nil || !exists {
			return err
		}
		if err := validateWakeReadyLockAndTarget(root, me, inspectWakeLock(root, me), published); err != nil {
			return err
		}
		ready = true
		return nil
	})
	return ready, err
}

func wakeCatchupReadyPath(root, me string) string {
	return filepath.Join(fsq.AgentBase(root, me), wakeCatchupReadyFileName)
}

func writeWakeCatchupReadyFile(root, me string, expected wakeLockInspection) error {
	return writeWakeReadyFile(root, me, wakeCatchupReadyPath(root, me), expected)
}

func wakeCatchupReadyMatches(root, me string, expected wakeLockInspection) (bool, error) {
	ready := false
	err := withWakeLifecycleGuard(root, me, func() error {
		current := inspectWakeLock(root, me)
		if !sameWakeLockGeneration(expected, current) {
			return fmt.Errorf("wake lock generation changed while waiting for catch-up readiness")
		}
		path := wakeCatchupReadyPath(root, me)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := validateWakeGenerationFile(path, "wake catch-up ready file", info); err != nil {
			return err
		}
		published, exists, err := readWakeReadyFile(path)
		if err != nil || !exists {
			// A securely located but partial/corrupt stale marker is never
			// trusted. The active generation may replace it before the timeout.
			return nil
		}
		// A previous generation's securely written attestation is harmless and
		// remains not-ready until the current generation replaces it.
		if published.Generation != current.Lock.Generation || published.TargetDigest != current.Lock.TargetDigest {
			return nil
		}
		if err := validateWakeReadyLockAndTarget(root, me, current, published); err != nil {
			return err
		}
		ready = true
		return nil
	})
	return ready, err
}

func existingWakeCatchupTimeout(injectTimeout time.Duration) time.Duration {
	timeout := 2*injectTimeout + time.Second
	if timeout < wakeReadyTimeout {
		return wakeReadyTimeout
	}
	if timeout > 30*time.Second {
		return 30 * time.Second
	}
	return timeout
}

func waitForWakeCatchupReady(root, me string, expected wakeLockInspection, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ready, err := wakeCatchupReadyMatches(root, me, expected)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("existing wake for %s did not attest initial catch-up readiness", me)
		}
		time.Sleep(wakeCatchupReadyPollInterval)
	}
}
