//go:build darwin || linux

package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type wakeOwnerPublicationError struct {
	Err       error
	Committed bool
}

var errWakeOwnerLockExists = errors.New("wake lock already exists")
var publishAuthoritativeWakeLinkAt = unix.Linkat
var publishAuthoritativeWakeAfterTargetRename = func() {}
var removeAuthoritativeWakeAfterLockRelease = func() {}
var removeAuthoritativeWakeBeforeFinalAuthorityCheck = func() {}
var afterWakeTargetSnapshotDataRead = func() {}
var removeAuthoritativeWakeLockTempAfterCommitAt = unix.Unlinkat
var syncAuthoritativeWakeLockAfterCommitDirFD = func(fd int) error {
	return syncWakeOwnerDirFD(fd)
}

func (err *wakeOwnerPublicationError) Error() string {
	return err.Err.Error()
}

func (err *wakeOwnerPublicationError) Unwrap() error {
	return err.Err
}

func publishAuthoritativeWakeClaimAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	target wakeTarget,
	lock wakeLock,
) error {
	return publishAuthoritativeWakeClaimWithDebugAt(dirfd, agentDir, root, me, target, lock, false)
}

func publishAuthoritativeWakeClaimWithDebugAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	target wakeTarget,
	lock wakeLock,
	debug bool,
) error {
	if err := validateAuthoritativeWakeTarget(target, root, me); err != nil {
		return err
	}
	if err := validateAuthoritativeWakeLockRecord(lock, root, me, target); err != nil {
		return err
	}

	existing, err := unix.Openat(dirfd, ".wake.lock", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		_ = unix.Close(existing)
		return errWakeOwnerLockExists
	}
	if err != unix.ENOENT {
		return fmt.Errorf("check existing wake lock: %w", err)
	}
	if err := debugWakeNoLockShadowReplacementAt(
		dirfd, agentDir, root, me, "authoritative", debug,
	); err != nil {
		return fmt.Errorf("write no-lock wake shadow diagnostic: %w", err)
	}

	targetData, err := json.MarshalIndent(target, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal authoritative wake target: %w", err)
	}
	targetData = append(targetData, '\n')
	targetTemp, err := writeWakeOwnerTempAt(dirfd, "wake-target", targetData, 0o600)
	if err != nil {
		return err
	}
	targetInstalled := false
	defer func() {
		if !targetInstalled {
			_ = unix.Unlinkat(dirfd, targetTemp, 0)
		}
	}()
	targetFD, err := unix.Openat(
		dirfd,
		targetTemp,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return fmt.Errorf("open authoritative wake target temp: %w", err)
	}
	targetFile := os.NewFile(uintptr(targetFD), targetTemp)
	defer func() { _ = targetFile.Close() }()
	targetInfo, err := targetFile.Stat()
	if err != nil {
		return fmt.Errorf("stat authoritative wake target temp: %w", err)
	}
	if !targetInfo.Mode().IsRegular() || targetInfo.Mode().Perm() != 0o600 {
		return fmt.Errorf("authoritative wake target temp must be a regular 0600 file")
	}
	if err := validateWakeTargetPathOwnership("authoritative wake target temp", targetTemp, targetInfo); err != nil {
		return err
	}
	targetRaw, err := readWakeMetadata(targetFile, "authoritative wake target temp", targetTemp)
	if err != nil {
		return err
	}
	if !bytes.Equal(targetRaw, targetData) {
		return fmt.Errorf("authoritative wake target temp changed before publication")
	}
	targetTempSnapshot := wakeTargetSnapshot{
		Target:   target,
		Raw:      bytes.Clone(targetRaw),
		FileInfo: targetInfo,
	}
	if err := unix.Renameat(dirfd, targetTemp, dirfd, wakeTargetFileName); err != nil {
		return fmt.Errorf("install authoritative wake target: %w", err)
	}
	targetInstalled = true
	publishAuthoritativeWakeAfterTargetRename()
	visibleTarget, exists, err := readWakeTargetSnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return &wakeOwnerPublicationError{
			Err: fmt.Errorf("installed authoritative wake target changed during publication: %w", err),
		}
	}
	if !exists {
		return &wakeOwnerPublicationError{
			Err: fmt.Errorf("installed authoritative wake target changed during publication: target disappeared"),
		}
	}
	if !os.SameFile(targetTempSnapshot.FileInfo, visibleTarget.FileInfo) ||
		!bytes.Equal(targetTempSnapshot.Raw, visibleTarget.Raw) {
		return &wakeOwnerPublicationError{
			Err: fmt.Errorf("installed authoritative wake target changed during publication; preserving it"),
		}
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return &wakeOwnerPublicationError{
			Err: fmt.Errorf("sync authoritative wake target directory: %w", err),
		}
	}
	if err := publishWakeStateAndBindLockAt(dirfd, agentDir, root, me, &lock); err != nil {
		return &wakeOwnerPublicationError{
			Err: fmt.Errorf("publish wake state before authoritative wake lock: %w", err),
		}
	}
	lockData, err := json.Marshal(lock)
	if err != nil {
		return fmt.Errorf("marshal authoritative wake lock: %w", err)
	}

	lockTemp, err := writeWakeOwnerTempAt(dirfd, "wake-lock", lockData, wakeOwnerLockFileMode)
	if err != nil {
		return err
	}
	lockTempPresent := true
	defer func() {
		if lockTempPresent {
			_ = unix.Unlinkat(dirfd, lockTemp, 0)
		}
	}()
	if err := publishAuthoritativeWakeLinkAt(dirfd, lockTemp, dirfd, ".wake.lock", 0); err != nil {
		if err == unix.EEXIST {
			return errWakeOwnerLockExists
		}
		return &wakeOwnerPublicationError{
			Err: fmt.Errorf("publish authoritative wake lock: %w", err),
		}
	}
	if err := removeAuthoritativeWakeLockTempAfterCommitAt(dirfd, lockTemp, 0); err != nil {
		return &wakeOwnerPublicationError{
			Err:       fmt.Errorf("remove authoritative wake lock temp after commit: %w", err),
			Committed: true,
		}
	}
	lockTempPresent = false
	if err := syncAuthoritativeWakeLockAfterCommitDirFD(dirfd); err != nil {
		return &wakeOwnerPublicationError{
			Err:       fmt.Errorf("sync authoritative wake lock directory after commit: %w", err),
			Committed: true,
		}
	}
	visibleLock := readWakeLockMetadataAt(dirfd, agentDir, root, me)
	if !visibleLock.Exists || visibleLock.Lock.Generation != lock.Generation || !bytes.Equal(visibleLock.raw, lockData) {
		return &wakeOwnerPublicationError{
			Err:       fmt.Errorf("verify bound authoritative wake lock after commit"),
			Committed: true,
		}
	}
	if _, err := validateAuthoritativeWakeClaimPairAt(dirfd, agentDir, visibleLock); err != nil {
		return &wakeOwnerPublicationError{
			Err:       fmt.Errorf("verify bound authoritative wake target after commit: %w", err),
			Committed: true,
		}
	}
	if err := validateWakeBoundStateAt(dirfd, agentDir, root, me, visibleLock.Lock); err != nil {
		return &wakeOwnerPublicationError{
			Err:       fmt.Errorf("verify bound authoritative wake state after commit: %w", err),
			Committed: true,
		}
	}
	return nil
}

func validateAuthoritativeWakeTarget(target wakeTarget, root, me string) error {
	if err := validateWakeTarget(target, root, me); err != nil {
		return err
	}
	if target.Owner == nil {
		return fmt.Errorf("authoritative wake target owner is missing")
	}
	if err := validateAuthoritativeWakeOwner(*target.Owner); err != nil {
		return fmt.Errorf("authoritative wake target owner is invalid: %w", err)
	}
	return nil
}

func validateAuthoritativeWakeLockRecord(lock wakeLock, root, me string, target wakeTarget) error {
	if err := validateAuthoritativeWakeLockEnvelope(lock, root, me); err != nil {
		return err
	}
	return validateWakeTargetMatchesLock(lock, target)
}

func validateAuthoritativeWakeLockEnvelope(lock wakeLock, root, me string) error {
	if lock.OwnerSchema != wakeOwnerLockSchema {
		return fmt.Errorf("authoritative wake owner schema %d unsupported", lock.OwnerSchema)
	}
	if lock.WakeMode != wakeOwnerWakeMode {
		return fmt.Errorf("authoritative wake mode %q unsupported", lock.WakeMode)
	}
	if lock.Owner == nil {
		return fmt.Errorf("authoritative wake lock owner is missing")
	}
	if err := validateAuthoritativeWakeOwner(*lock.Owner); err != nil {
		return fmt.Errorf("authoritative wake lock owner is invalid: %w", err)
	}
	if lock.PID <= 0 {
		return fmt.Errorf("authoritative wake lock pid must be > 0")
	}
	if err := validateAuthoritativeWakeProcessIdentity(lock); err != nil {
		return fmt.Errorf("authoritative wake lock process identity is invalid: %w", err)
	}
	if lock.Generation == "" {
		return fmt.Errorf("authoritative wake lock generation is missing")
	}
	if lock.TargetDigest == "" {
		return fmt.Errorf("authoritative wake target digest is missing")
	}
	if lock.Root == "" {
		return fmt.Errorf("authoritative wake lock root is missing")
	}
	if canonicalWakeRoot(lock.Root) != canonicalWakeRoot(root) {
		return fmt.Errorf("authoritative wake lock root mismatch")
	}
	if lock.Agent != me {
		return fmt.Errorf("authoritative wake lock agent mismatch")
	}
	return nil
}

func writeWakeOwnerTempAt(dirfd int, label string, data []byte, mode os.FileMode) (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate %s temp name: %w", label, err)
	}
	name := fmt.Sprintf(".%s.tmp.%d.%s", label, os.Getpid(), hex.EncodeToString(nonce[:]))
	fd, err := unix.Openat(
		dirfd,
		name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		uint32(mode.Perm()),
	)
	if err != nil {
		return "", fmt.Errorf("create %s temp: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), name)
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = unix.Unlinkat(dirfd, name, 0)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return "", fmt.Errorf("chmod %s temp: %w", label, err)
	}
	n, err := file.Write(data)
	if err != nil {
		return "", fmt.Errorf("write %s temp: %w", label, err)
	}
	if n != len(data) {
		return "", fmt.Errorf("write %s temp: %w", label, io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync %s temp: %w", label, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close %s temp: %w", label, err)
	}
	keep = true
	return name, nil
}

var syncWakeOwnerDirFD = unix.Fsync

func readAuthoritativeWakeTargetAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	lock wakeLock,
) (wakeTarget, error) {
	snapshot, err := readAuthoritativeWakeTargetSnapshotAt(dirfd, agentDir, root, me, lock)
	if err != nil {
		return wakeTarget{}, err
	}
	return snapshot.Target, nil
}

type wakeTargetSnapshot struct {
	Target   wakeTarget
	Raw      []byte
	FileInfo os.FileInfo
}

func readAuthoritativeWakeTargetSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	lock wakeLock,
) (wakeTargetSnapshot, error) {
	snapshot, exists, err := readWakeTargetSnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return wakeTargetSnapshot{}, err
	}
	if !exists {
		return wakeTargetSnapshot{}, fmt.Errorf("authoritative wake target is missing")
	}
	if err := validateAuthoritativeWakeTarget(snapshot.Target, root, me); err != nil {
		return wakeTargetSnapshot{}, err
	}
	if err := validateAuthoritativeWakeLockRecord(lock, root, me, snapshot.Target); err != nil {
		return wakeTargetSnapshot{}, err
	}
	return snapshot, nil
}

func readWakeTargetAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
) (wakeTarget, bool, error) {
	target, exists, err := readWakeTargetRawAt(dirfd, agentDir, root, me)
	if err != nil || !exists {
		return target, exists, err
	}
	if err := validateWakeTarget(target, root, me); err != nil {
		return wakeTarget{}, true, err
	}
	return target, true, nil
}

func readWakeTargetRawAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
) (wakeTarget, bool, error) {
	snapshot, exists, err := readWakeTargetSnapshotAt(dirfd, agentDir, root, me)
	return snapshot.Target, exists, err
}

func readWakeTargetSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
) (wakeTargetSnapshot, bool, error) {
	path := wakeTargetPath(root, me)
	open := func() (*os.File, error) {
		fd, err := unix.Openat(dirfd, wakeTargetFileName, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	file, err := open()
	if err != nil {
		if err == unix.ENOENT {
			return wakeTargetSnapshot{}, false, nil
		}
		return wakeTargetSnapshot{}, true, fmt.Errorf("open wake target: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return wakeTargetSnapshot{}, true, fmt.Errorf("stat wake target: %w", err)
	}
	if !info.Mode().IsRegular() {
		return wakeTargetSnapshot{}, true, fmt.Errorf("wake target must be a regular file")
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return wakeTargetSnapshot{}, true, fmt.Errorf("wake target mode is %o, want 0600", got)
	}
	if err := validateWakeTargetPathOwnership("wake target", path, info); err != nil {
		return wakeTargetSnapshot{}, true, err
	}
	data, err := readWakeMetadata(file, "wake target", path)
	if err != nil {
		return wakeTargetSnapshot{}, true, err
	}
	afterWakeTargetSnapshotDataRead()
	pathFile, err := open()
	if err != nil {
		return wakeTargetSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake target changed while reopening: %w", err),
		)
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil {
		return wakeTargetSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake target changed while restating: %w", statErr),
		)
	}
	if !sameWakeFileIdentity(info, pathInfo) {
		return wakeTargetSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake target changed while opening"),
		)
	}
	snapshot := wakeTargetSnapshot{
		Raw:      data,
		FileInfo: info,
	}
	var target wakeTarget
	if err := json.Unmarshal(data, &target); err != nil {
		return snapshot, true, fmt.Errorf("parse wake target: %w", err)
	}
	_ = agentDir
	snapshot.Target = target
	return snapshot, true, nil
}

func removeAuthoritativeWakeClaimAt(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	target *wakeTarget,
) error {
	current := readWakeLockMetadataAt(dirfd, agentDir, expected.Root, expected.Agent)
	if !sameWakeLockGeneration(expected, current) {
		return fmt.Errorf("authoritative wake claim changed before release")
	}
	if err := validateBoundWakeMutationAt(dirfd, agentDir, current); err != nil {
		return err
	}
	if classifyPersistedWakeClaim(current) != wakeClaimAuthoritative {
		return fmt.Errorf("authoritative wake lock became invalid before release")
	}
	var releaseTargetSnapshot *wakeTargetSnapshot
	if target != nil {
		currentTarget, err := readAuthoritativeWakeTargetSnapshotAt(
			dirfd,
			agentDir,
			expected.Root,
			expected.Agent,
			current.Lock,
		)
		if err != nil {
			return fmt.Errorf("authoritative wake claim became invalid before release: %w", err)
		}
		expectedDigest, err := wakeTargetDigest(*target)
		if err != nil {
			return err
		}
		currentDigest, err := wakeTargetDigest(currentTarget.Target)
		if err != nil {
			return err
		}
		if expectedDigest != current.Lock.TargetDigest ||
			currentDigest != current.Lock.TargetDigest {
			return fmt.Errorf("authoritative wake target changed before release")
		}
		releaseTargetSnapshot = &currentTarget
	}

	var releasePreparedSnapshot *wakeGenerationFileSnapshot
	var preparedSnapshotErr error
	preparedSnapshot, preparedExists, err := readWakeGenerationFileSnapshotAt(
		dirfd,
		agentDir,
		wakePreparedFileName,
		"wake prepared marker",
	)
	if err != nil {
		preparedSnapshotErr = fmt.Errorf("snapshot released wake prepared marker: %w", err)
		if releaseTargetSnapshot == nil {
			return preparedSnapshotErr
		}
	} else if preparedExists &&
		preparedSnapshot.Marker.Schema == wakeReadySchema &&
		preparedSnapshot.Marker.Generation == current.Lock.Generation &&
		preparedSnapshot.Marker.TargetDigest == current.Lock.TargetDigest {
		if releaseTargetSnapshot == nil {
			return fmt.Errorf(
				"validate released wake prepared marker: authoritative wake target snapshot is unavailable",
			)
		}
		releasePreparedSnapshot = &preparedSnapshot
	}
	releaseStateSnapshot, releaseStateExists, stateSnapshotErr := readWakeStateRawSnapshotAt(dirfd, agentDir)
	if stateSnapshotErr != nil {
		continueAfterWakeStateProjectionError(newWakeStateProjectionError(
			fmt.Errorf("snapshot wake state before release: %w", stateSnapshotErr),
		))
		releaseStateExists = false
	}
	if err := reclaimWakeRestartStateForLockRemovalAt(dirfd, agentDir, current); err != nil {
		return fmt.Errorf("reconcile wake restart ownership before authoritative lock release: %w", err)
	}

	// Pathname unlink is safe under the lifecycle guard held by every
	// cooperating writer; an unguarded same-UID writer is out of contract. A
	// rename-and-verify alternative would expose lock absence to pre-P2b readers
	// during a two-step removal, creating a real competing-authority hazard.
	if err := unix.Unlinkat(dirfd, ".wake.lock", 0); err != nil {
		if err == unix.ENOENT {
			return preparedSnapshotErr
		}
		return errors.Join(
			preparedSnapshotErr,
			fmt.Errorf("unlink authoritative wake lock: %w", err),
		)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return errors.Join(
			preparedSnapshotErr,
			fmt.Errorf("sync authoritative wake lock release: %w", err),
		)
	}
	cleanupErr := preparedSnapshotErr
	cleaned := false
	if err := removeWakeSelfUpgradeDiagnosticAt(dirfd); err != nil {
		diagnosticCleanupErr := newWakeLockResidueError(
			wakeLockResidueSelfUpgradeDiagnostic,
			fmt.Errorf("remove wake self-upgrade diagnostic after authoritative lock release: %w", err),
		)
		_ = writeStderr(
			"warning: removed authoritative wake lock for %s but left diagnostic-only self-upgrade residue: %v\n",
			current.Agent,
			diagnosticCleanupErr,
		)
	}
	removeAuthoritativeWakeAfterLockRelease()
	if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
		if retainedWakeAgentDirIsDetached(agentDir) {
			return errors.Join(
				cleanupErr,
				newWakeDetachedCleanupOnlyError(err),
			)
		}
		return errors.Join(cleanupErr, err)
	}

	// A replacement is never selected or cleaned. Cooperative writers cannot
	// install one while this guard is held; this check also catches bypassers.
	replacement, err := unix.Openat(dirfd, ".wake.lock", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		_ = unix.Close(replacement)
		return cleanupErr
	}
	if err != unix.ENOENT {
		return errors.Join(
			cleanupErr,
			fmt.Errorf("check replacement wake lock after release: %w", err),
		)
	}

	if releaseTargetSnapshot != nil {
		removed, err := removeWakeTargetIfSnapshotMatchesAt(
			dirfd,
			agentDir,
			expected.Root,
			expected.Agent,
			*releaseTargetSnapshot,
		)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else if removed {
			cleaned = true
		}
	}
	stateRemoved, stateErr := removeWakeStateIfTargetAbsentAt(
		dirfd,
		agentDir,
		releaseStateSnapshot,
		releaseStateExists,
	)
	if stateErr != nil {
		if !continueAfterWakeStateProjectionError(stateErr) {
			cleanupErr = errors.Join(cleanupErr, stateErr)
		}
	} else if stateRemoved {
		cleaned = true
	}
	if releasePreparedSnapshot != nil {
		_, err := removeWakeGenerationFileIfSnapshotMatchesAt(
			dirfd,
			agentDir,
			wakePreparedFileName,
			"wake prepared marker",
			*releasePreparedSnapshot,
		)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if expected.Lock.ControlSocket != "" {
		name, nameErr := darwinControlSocketBasenameForCleanup(agentDir, expected.Lock.ControlSocket)
		if nameErr != nil {
			cleanupErr = errors.Join(cleanupErr, nameErr)
		} else if name != "" {
			if err := unix.Unlinkat(dirfd, name, 0); err != nil && err != unix.ENOENT {
				cleanupErr = errors.Join(
					cleanupErr,
					fmt.Errorf("remove released wake control socket: %w", err),
				)
			} else {
				cleaned = true
			}
		}
	}
	if cleaned {
		if err := syncWakeOwnerDirFD(dirfd); err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("sync authoritative wake claim cleanup: %w", err),
			)
		}
	}
	removeAuthoritativeWakeBeforeFinalAuthorityCheck()
	if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
		if retainedWakeAgentDirIsDetached(agentDir) {
			return errors.Join(cleanupErr, newWakeDetachedCleanupOnlyError(err))
		}
		return errors.Join(cleanupErr, err)
	}
	return cleanupErr
}

func removeWakeTargetIfSnapshotMatchesAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	expected wakeTargetSnapshot,
) (bool, error) {
	current, exists, err := readWakeTargetSnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return false, fmt.Errorf("re-read released wake target: %w", err)
	}
	if !exists {
		return false, nil
	}
	if expected.FileInfo == nil ||
		current.FileInfo == nil ||
		!sameWakeFileIdentity(expected.FileInfo, current.FileInfo) ||
		!bytes.Equal(expected.Raw, current.Raw) {
		return false, fmt.Errorf("released wake target changed before cleanup; preserving it")
	}
	expectedDigest, err := wakeTargetDigest(expected.Target)
	if err != nil {
		return false, err
	}
	currentDigest, err := wakeTargetDigest(current.Target)
	if err != nil {
		return false, err
	}
	if expectedDigest != currentDigest {
		return false, fmt.Errorf("released wake target digest changed before cleanup; preserving it")
	}
	if err := unix.Unlinkat(dirfd, wakeTargetFileName, 0); err != nil {
		if err == unix.ENOENT {
			return false, nil
		}
		return false, fmt.Errorf("remove released wake target: %w", err)
	}
	return true, nil
}
