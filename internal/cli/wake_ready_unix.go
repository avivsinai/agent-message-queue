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
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !sameWakeLockGeneration(expected, current) {
			return fmt.Errorf("wake lock generation changed before readiness publication")
		}
		ready := wakeReady{
			Schema:       wakeReadySchema,
			Generation:   current.Lock.Generation,
			TargetDigest: current.Lock.TargetDigest,
		}
		if err := validateWakeReadyLockAndTargetAt(dirfd, agentDir, root, me, current, ready); err != nil {
			return err
		}
		return writeWakeGenerationFile(path, "wake ready file", ready)
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
	if err := validateWakeReadyAgainstLock(current, ready); err != nil {
		return err
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
	return validateWakeReadyTargetAndOwner(current, target)
}

func validateWakeReadyLockAndTargetAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	current wakeLockInspection,
	ready wakeReady,
) error {
	if err := validateWakeReadyAgainstLock(current, ready); err != nil {
		return err
	}
	target, exists, err := readWakeTargetAt(dirfd, agentDir, root, me)
	if err != nil {
		return err
	}
	if current.Lock.TargetDigest == "" {
		if exists {
			return fmt.Errorf("wake readiness target does not match current wake lock")
		}
		return nil
	}
	if !exists {
		return fmt.Errorf("wake readiness target is missing")
	}
	return validateWakeReadyTargetAndOwner(current, target)
}

func validateWakeReadyAgainstLock(current wakeLockInspection, ready wakeReady) error {
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
	return nil
}

func validateWakeReadyTargetAndOwner(current wakeLockInspection, target wakeTarget) error {
	if err := validateWakeTargetMatchesLock(current.Lock, target); err != nil {
		return err
	}
	if current.Lock.WakeMode == wakeOwnerWakeMode {
		observation, err := observeAuthoritativeWakeOwner(*current.Lock.Owner)
		defer func() { _ = observation.Close() }()
		if err != nil {
			return fmt.Errorf("inspect wake owner during readiness validation: %w", err)
		}
		if observation.State != wakeOwnerSame {
			return fmt.Errorf("wake owner is %s during readiness validation: %s", observation.State, observation.Reason)
		}
	}
	return nil
}

func validateWakeReadyFileAgainstOwner(
	root string,
	me string,
	path string,
	requestedOwner *wakeOwner,
) (bool, error) {
	// A wake can still die immediately after this guarded validation; the local
	// process notifier has no durable liveness lease beyond the lock generation.
	ready := false
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return false, err
	}
	defer func() { _ = agentDir.Close() }()
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		published, exists, err := readWakeReadyFile(path)
		if err != nil || !exists {
			return err
		}
		current := inspectWakeLockAt(dirfd, agentDir, root, me)
		if err := validateWakeReadyLockAndTargetAt(dirfd, agentDir, root, me, current, published); err != nil {
			return err
		}
		if requestedOwner != nil &&
			current.Lock.WakeMode == wakeOwnerWakeMode &&
			!sameWakeOwner(current.Lock.Owner, requestedOwner) {
			return fmt.Errorf("wake readiness owner does not match the requested owner")
		}
		ready = true
		return nil
	})
	return ready, err
}
