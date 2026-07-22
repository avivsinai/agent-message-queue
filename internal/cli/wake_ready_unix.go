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
		data, err := json.Marshal(wakeReady{
			Schema:       wakeReadySchema,
			Generation:   current.Lock.Generation,
			TargetDigest: current.Lock.TargetDigest,
		})
		if err != nil {
			return fmt.Errorf("marshal wake readiness: %w", err)
		}
		return writeWakeMetadataFile(path, append(data, '\n'), "wake ready file")
	})
}

func readWakeReadyFile(path string) (wakeReady, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return wakeReady{}, false, nil
		}
		return wakeReady{}, false, fmt.Errorf("stat wake ready file: %w", err)
	}
	if err := validateWakeReadyFile(path, info); err != nil {
		return wakeReady{}, true, err
	}
	file, err := openWakeMetadataFile(path)
	if err != nil {
		return wakeReady{}, true, fmt.Errorf("open wake ready file: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return wakeReady{}, true, fmt.Errorf("stat opened wake ready file: %w", err)
	}
	if err := validateWakeReadyFile(path, openedInfo); err != nil {
		return wakeReady{}, true, err
	}
	if !os.SameFile(info, openedInfo) {
		return wakeReady{}, true, fmt.Errorf("wake ready file %s changed while opening", path)
	}
	data, err := readWakeMetadata(file, "wake ready file", path)
	if err != nil {
		return wakeReady{}, true, err
	}
	var ready wakeReady
	if err := json.Unmarshal(data, &ready); err != nil {
		return wakeReady{}, true, fmt.Errorf("legacy wake ready file refused")
	}
	if ready.Schema != wakeReadySchema || ready.Generation == "" {
		return wakeReady{}, true, fmt.Errorf("legacy wake ready file refused")
	}
	return ready, true, nil
}

func validateWakeReadyFile(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("wake ready file %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("wake ready file %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("wake ready file %s mode is %o, want 0600", path, got)
	}
	return validateWakeTargetPathOwnership("wake ready file", path, info)
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
		if err := validateWakeReadyFile(path, info); err != nil {
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
