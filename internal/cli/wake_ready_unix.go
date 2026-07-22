//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
	"os"
)

const wakeReadySchema = 1

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
