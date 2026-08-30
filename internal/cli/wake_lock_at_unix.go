//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var afterWakeLockAtRead = func() {}
var syncWakeLockAfterCommitDirFD = func(fd int) error {
	return syncWakeOwnerDirFD(fd)
}

func readWakeLockFileAt(dirfd int, path string) ([]byte, os.FileInfo, error) {
	open := func() (*os.File, error) {
		fd, err := unix.Openat(dirfd, ".wake.lock", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	file, err := open()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat wake lock: %w", err)
	}
	if err := validateWakeLockFile(path, info); err != nil {
		return nil, nil, err
	}
	data, err := readWakeMetadata(file, "wake lock", path)
	if err != nil {
		return nil, nil, err
	}
	afterWakeLockAtRead()
	pathFile, err := open()
	if err != nil {
		return nil, nil, err
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil {
		return nil, nil, fmt.Errorf("re-stat wake lock: %w", statErr)
	}
	if err := validateWakeLockFile(path, pathInfo); err != nil {
		return nil, nil, err
	}
	if !sameWakeFileIdentity(info, pathInfo) {
		return nil, nil, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake lock %s changed while opening", path),
		)
	}
	return data, info, nil
}

func inspectWakeLockAt(dirfd int, agentDir *wakeAgentDir, root, me string) wakeLockInspection {
	path := filepath.Join(agentDir.path, ".wake.lock")
	return inspectWakeLockWithReader(root, me, path, func() ([]byte, os.FileInfo, error) {
		return readWakeLockFileAt(dirfd, path)
	})
}

func readWakeLockMetadataAt(dirfd int, agentDir *wakeAgentDir, root, me string) wakeLockInspection {
	path := filepath.Join(agentDir.path, ".wake.lock")
	return readWakeLockMetadataWithReader(root, me, path, func() ([]byte, os.FileInfo, error) {
		return readWakeLockFileAt(dirfd, path)
	})
}

func createWakeLockAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	lock wakeLock,
) error {
	if strings.TrimSpace(lock.Generation) == "" {
		return fmt.Errorf("wake lock generation is missing")
	}
	if canonicalWakeRoot(lock.Root) != canonicalWakeRoot(root) {
		return fmt.Errorf("wake lock root mismatch")
	}
	if lock.Agent != me {
		return fmt.Errorf("wake lock agent mismatch")
	}
	if err := validateWakeLockStateBinding(lock); err != nil {
		return err
	}
	data, err := json.Marshal(lock)
	if err != nil {
		return fmt.Errorf("marshal wake lock: %w", err)
	}
	fd, err := unix.Openat(
		dirfd,
		".wake.lock",
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("failed to create wake lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(agentDir.path, ".wake.lock"))
	createdInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return fmt.Errorf("stat created wake lock: %w", statErr)
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			currentFD, openErr := unix.Openat(
				dirfd,
				".wake.lock",
				unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
				0,
			)
			if openErr == nil {
				currentFile := os.NewFile(uintptr(currentFD), filepath.Join(agentDir.path, ".wake.lock"))
				currentInfo, currentErr := currentFile.Stat()
				_ = currentFile.Close()
				if currentErr == nil && sameWakeFileIdentity(createdInfo, currentInfo) {
					_ = unix.Unlinkat(dirfd, ".wake.lock", 0)
					_ = syncWakeOwnerDirFD(dirfd)
				}
			}
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod created wake lock: %w", err)
	}
	n, err := file.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write wake lock: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("failed to write wake lock: %w", io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync wake lock: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close wake lock: %w", err)
	}
	// O_EXCL made this lock name the no-replace ownership commit. Preserve the
	// exact claim if the following durability confirmation reports an error.
	committed = true
	if err := syncWakeLockAfterCommitDirFD(dirfd); err != nil {
		return fmt.Errorf("sync wake lock directory after commit: %w", err)
	}
	created := readWakeLockMetadataAt(dirfd, agentDir, root, me)
	if !created.Exists ||
		created.Lock.Generation != lock.Generation ||
		!bytes.Equal(created.raw, data) {
		return fmt.Errorf("failed to verify created wake lock generation")
	}
	return nil
}

func createWakeRepairLockAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	rootIdentity string,
	lock wakeLock,
) error {
	if err := revalidateWakeRepairRootIdentity(root, rootIdentity); err != nil {
		return err
	}
	return createWakeLockAt(dirfd, agentDir, root, me, lock)
}

func removeWakeLockIfUnchangedGuardedAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	committed, err := removeWakeLockIfUnchangedGuardedAtStatus(dirfd, agentDir, inspection)
	if !committed {
		return err
	}
	blocking, diagnostic := splitWakeSelfUpgradeDiagnosticResidue(err)
	if diagnostic != nil {
		_ = writeStderr(
			"warning: removed wake lock for %s but left diagnostic-only self-upgrade residue: %v\n",
			inspection.Agent,
			diagnostic,
		)
	}
	return blocking
}

func removeWakeLockIfUnchangedGuardedAtStatus(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) (bool, error) {
	outcome := removeWakeLockIfUnchangedGuardedAtOutcome(
		dirfd,
		agentDir,
		inspection,
		func() error { return unix.Unlinkat(dirfd, ".wake.lock", 0) },
	)
	return outcome.Committed, outcome.Err
}

type wakeLockRemovalOutcome struct {
	Committed bool
	Err       error
}

type wakeLockRemovalResidue string

const (
	wakeLockResidueDurability            wakeLockRemovalResidue = "wake lock durability"
	wakeLockResidueDetachedCleanup       wakeLockRemovalResidue = "detached wake cleanup"
	wakeLockResidueReplacement           wakeLockRemovalResidue = "replacement wake lock"
	wakeLockResiduePreservedClaim        wakeLockRemovalResidue = ".wake.lock"
	wakeLockResidueCleanup               wakeLockRemovalResidue = "wake lock cleanup"
	wakeLockResidueSelfUpgradeDiagnostic wakeLockRemovalResidue = "wake self-upgrade diagnostic"
)

type wakeLockResidueError struct {
	residue wakeLockRemovalResidue
	err     error
}

func (err *wakeLockResidueError) Error() string { return err.err.Error() }
func (err *wakeLockResidueError) Unwrap() error { return err.err }

func newWakeLockResidueError(residue wakeLockRemovalResidue, err error) error {
	if err == nil {
		return nil
	}
	return &wakeLockResidueError{residue: residue, err: err}
}

func splitWakeSelfUpgradeDiagnosticResidue(err error) (blocking, diagnostic error) {
	if err == nil {
		return nil, nil
	}
	if typed, ok := err.(*wakeLockResidueError); ok && typed.residue == wakeLockResidueSelfUpgradeDiagnostic {
		return nil, err
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err, nil
	}
	for _, child := range joined.Unwrap() {
		childBlocking, childDiagnostic := splitWakeSelfUpgradeDiagnosticResidue(child)
		blocking = errors.Join(blocking, childBlocking)
		diagnostic = errors.Join(diagnostic, childDiagnostic)
	}
	return blocking, diagnostic
}

func removeWakeLockIfUnchangedGuardedAtOutcome(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	unlink func() error,
) wakeLockRemovalOutcome {
	detached := retainedWakeAgentDirIsDetached(agentDir)
	var detachedValidationErr error
	if err := validateBoundWakeMutationAt(dirfd, agentDir, inspection); err != nil {
		// A retained directory capability can outlive replacement of its
		// canonical pathname. Exact cleanup inside that proven-detached inode
		// cannot signal or unlink the successor claim, so it may reap its own
		// private residue even though canonical bound-state validation is no
		// longer possible.
		if !detached {
			return wakeLockRemovalOutcome{Err: err}
		}
		detachedValidationErr = err
	}
	if detached && detachedValidationErr == nil {
		detachedValidationErr = wakeDetachedCleanupValidationError()
	}
	if err := reclaimWakeRestartStateForLockRemovalAt(dirfd, agentDir, inspection); err != nil {
		return wakeLockRemovalOutcome{Err: fmt.Errorf(
			"reconcile wake restart ownership before lock removal: %w",
			err,
		)}
	}
	path := filepath.Join(agentDir.path, ".wake.lock")
	// The retained descriptor keeps the unlink bound to the old inode. Recheck
	// the canonical pathname immediately before the mutation so a swap after
	// the initial sample is reported as detached cleanup, not canonical success.
	// A non-cooperating rename between this check and the unlink remains
	// undetectable. The retained descriptor still prevents successor mutation;
	// the post-commit check below surfaces swaps observed before return.
	unlinkWithDetachedClassification := func() error {
		markWakeDetachedCleanup(&detachedValidationErr, agentDir)
		return unlink()
	}
	committed, err := removeWakeLockIfUnchangedGuardedWithIOStatus(
		inspection,
		func() ([]byte, os.FileInfo, error) { return readWakeLockFileAt(dirfd, path) },
		unlinkWithDetachedClassification,
	)
	if err != nil {
		return wakeLockRemovalOutcome{Committed: committed, Err: err}
	}
	if !committed {
		return wakeLockRemovalOutcome{}
	}
	// The pre-unlink check and unlink cannot be atomic against a
	// non-cooperating namespace rename. The retained descriptor still prevents
	// successor mutation, so surface a late replacement as detached cleanup.
	markWakeDetachedCleanup(&detachedValidationErr, agentDir)
	outcome := wakeLockRemovalOutcome{Committed: true}
	if detachedValidationErr != nil {
		outcome.Err = newWakeDetachedCleanupOnlyError(detachedValidationErr)
	}
	if err := removeWakeSelfUpgradeArtifactsAt(dirfd); err != nil {
		outcome.Err = errors.Join(
			outcome.Err,
			newWakeLockResidueError(
				wakeLockResidueSelfUpgradeDiagnostic,
				fmt.Errorf("remove wake self-upgrade metadata after lock removal: %w", err),
			),
		)
	}
	return outcome
}

func removeWakeLockIfUnchangedGuardedAtDurableOutcome(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
	unlink func() error,
) wakeLockRemovalOutcome {
	outcome := removeWakeLockIfUnchangedGuardedAtOutcome(
		dirfd, agentDir, inspection, unlink,
	)
	if !outcome.Committed {
		return outcome
	}
	if err := syncWakeLockAfterCommitDirFD(dirfd); err != nil {
		outcome.Err = errors.Join(
			outcome.Err,
			newWakeLockResidueError(
				wakeLockResidueDurability,
				fmt.Errorf("sync wake lock directory after exact removal: %w", err),
			),
		)
	}
	return outcome
}

func appendWakeLockRemovalResidue(
	residue []wakeLockRemovalResidue,
	value wakeLockRemovalResidue,
) []wakeLockRemovalResidue {
	for _, existing := range residue {
		if existing == value {
			return residue
		}
	}
	return append(residue, value)
}

func wakeLockRemovalResiduesFromError(err error) []wakeLockRemovalResidue {
	var residue []wakeLockRemovalResidue
	var collect func(error)
	collect = func(current error) {
		if current == nil {
			return
		}
		if typed, ok := current.(*wakeLockResidueError); ok {
			residue = appendWakeLockRemovalResidue(residue, typed.residue)
			return
		}
		if _, ok := current.(*wakeDetachedCleanupOnlyError); ok {
			residue = appendWakeLockRemovalResidue(residue, wakeLockResidueDetachedCleanup)
			return
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				collect(child)
			}
			return
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			collect(wrapped.Unwrap())
		}
	}
	collect(err)
	return residue
}

// wakeDetachedCleanupOnlyError reports that a retained descriptor proved
// detached from its canonical pathname. The exact old residue was removed,
// but callers must not continue mutation through that detached capability.
type wakeDetachedCleanupOnlyError struct {
	err error
}

func (err *wakeDetachedCleanupOnlyError) Error() string {
	return fmt.Sprintf("wake lock residue removed from detached directory; refusing further mutation: %v", err.err)
}

func (err *wakeDetachedCleanupOnlyError) Unwrap() error {
	return err.err
}

func newWakeDetachedCleanupOnlyError(err error) error {
	if err == nil {
		return nil
	}
	return &wakeDetachedCleanupOnlyError{err: err}
}

func wakeDetachedCleanupValidationError() error {
	return fmt.Errorf("retained wake agent directory is detached from the canonical successor")
}

func markWakeDetachedCleanup(err *error, agentDir *wakeAgentDir) {
	if err == nil || *err != nil || !retainedWakeAgentDirIsDetached(agentDir) {
		return
	}
	*err = wakeDetachedCleanupValidationError()
}

func retainedWakeAgentDirIsDetached(agentDir *wakeAgentDir) bool {
	if agentDir == nil || agentDir.file == nil {
		return false
	}
	retainedInfo, err := agentDir.file.Stat()
	if err != nil {
		return false
	}
	fd, err := unix.Open(
		agentDir.path,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return false
	}
	canonical := os.NewFile(uintptr(fd), agentDir.path)
	defer func() { _ = canonical.Close() }()
	canonicalInfo, err := canonical.Stat()
	return err == nil && !os.SameFile(retainedInfo, canonicalInfo)
}

type wakeGenerationFileSnapshot struct {
	Marker   wakeReady
	Raw      []byte
	FileInfo os.FileInfo
	Failure  *wakeGenerationFileFailureSnapshot
}

type wakeGenerationFileFailureSnapshot struct {
	Stage         string
	Class         string
	Identity      wakeFileIdentity
	IdentityKnown bool
	Mode          uint32
	Size          int64
}

func wakeGenerationFileFailureClass(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return "errno:" + strconv.FormatUint(uint64(errno), 10)
	}
	return err.Error()
}

func captureWakeGenerationFileIdentityAt(
	dirfd int,
	name string,
) (wakeFileIdentity, uint32, int64, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return wakeFileIdentity{}, 0, 0, err
	}
	return wakeFileIdentity{
		Device:    uint64(stat.Dev),
		Inode:     uint64(stat.Ino),
		CTimeSec:  int64(stat.Ctim.Sec),
		CTimeNsec: int64(stat.Ctim.Nsec),
	}, uint32(stat.Mode), stat.Size, nil
}

func recordWakeGenerationFileFailureAt(
	dirfd int,
	name string,
	stage string,
	err error,
	snapshot wakeGenerationFileSnapshot,
) wakeGenerationFileSnapshot {
	failure := &wakeGenerationFileFailureSnapshot{
		Stage: stage,
		Class: wakeGenerationFileFailureClass(err),
	}
	if snapshot.FileInfo != nil {
		failure.Identity, failure.IdentityKnown = captureWakeFileIdentity(snapshot.FileInfo)
		failure.Mode = uint32(snapshot.FileInfo.Mode())
		failure.Size = snapshot.FileInfo.Size()
	} else if stage == "open" {
		identity, mode, size, statErr := captureWakeGenerationFileIdentityAt(dirfd, name)
		if statErr != nil {
			snapshot.Failure = failure
			return snapshot
		}
		failure.Identity = identity
		failure.IdentityKnown = true
		failure.Mode = mode
		failure.Size = size
	}
	snapshot.Failure = failure
	return snapshot
}

var afterWakeGenerationFileSnapshotDataRead = func(string) {}

func sameWakeGenerationFileSnapshot(first, second os.FileInfo) bool {
	return sameWakeFileIdentity(first, second) &&
		first.Mode() == second.Mode() && first.Size() == second.Size()
}

func readWakeGenerationFileSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
	name string,
	label string,
) (wakeGenerationFileSnapshot, bool, error) {
	path := filepath.Join(agentDir.path, name)
	open := func() (*os.File, error) {
		fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	file, err := open()
	if err != nil {
		if err == unix.ENOENT {
			return wakeGenerationFileSnapshot{}, false, nil
		}
		snapshot := recordWakeGenerationFileFailureAt(
			dirfd, name, "open", err, wakeGenerationFileSnapshot{},
		)
		return snapshot, true, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		snapshot := recordWakeGenerationFileFailureAt(
			dirfd, name, "stat", err, wakeGenerationFileSnapshot{},
		)
		return snapshot, true, err
	}
	snapshot := wakeGenerationFileSnapshot{FileInfo: info}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		err := fmt.Errorf("%s must be a regular 0600 file", label)
		return recordWakeGenerationFileFailureAt(dirfd, name, "shape", err, snapshot), true, err
	}
	if err := validateWakeTargetPathOwnership(label, path, info); err != nil {
		return recordWakeGenerationFileFailureAt(dirfd, name, "ownership", err, snapshot), true, err
	}
	data, err := readWakeMetadata(file, label, path)
	if err != nil {
		return recordWakeGenerationFileFailureAt(dirfd, name, "read", err, snapshot), true, err
	}
	snapshot.Raw = bytes.Clone(data)
	afterWakeGenerationFileSnapshotDataRead(name)
	pathFile, err := open()
	if err != nil {
		return wakeGenerationFileSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("%s changed while reopening: %w", label, err),
		)
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil {
		return wakeGenerationFileSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("%s changed while restating: %w", label, statErr),
		)
	}
	// Some Linux filesystems can report the same ctime for a same-timestamp
	// chmod. Freeze the portable acceptance shape explicitly so a mid-read mode
	// or size change is still classified as a changed snapshot, not as stable
	// malformed state.
	if !sameWakeGenerationFileSnapshot(info, pathInfo) {
		return wakeGenerationFileSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("%s changed while opening", label),
		)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm() != 0o600 {
		return wakeGenerationFileSnapshot{}, true, fmt.Errorf("%s must be a regular 0600 file", label)
	}
	if err := validateWakeTargetPathOwnership(label, path, pathInfo); err != nil {
		return wakeGenerationFileSnapshot{}, true, err
	}
	var marker wakeReady
	if err := json.Unmarshal(data, &marker); err != nil {
		return recordWakeGenerationFileFailureAt(dirfd, name, "parse", err, snapshot), true, err
	}
	if marker.Schema != wakeReadySchema || marker.Generation == "" {
		err := fmt.Errorf("%s schema is unsupported", label)
		return recordWakeGenerationFileFailureAt(dirfd, name, "schema", err, snapshot), true, err
	}
	snapshot.Marker = marker
	return snapshot, true, nil
}

func readWakeGenerationFileAt(
	dirfd int,
	agentDir *wakeAgentDir,
	name string,
	label string,
) (wakeReady, bool, error) {
	snapshot, exists, err := readWakeGenerationFileSnapshotAt(
		dirfd,
		agentDir,
		name,
		label,
	)
	return snapshot.Marker, exists, err
}

func removeWakeGenerationFileIfSnapshotMatchesAt(
	dirfd int,
	agentDir *wakeAgentDir,
	name string,
	label string,
	expected wakeGenerationFileSnapshot,
) (bool, error) {
	current, exists, err := readWakeGenerationFileSnapshotAt(
		dirfd,
		agentDir,
		name,
		label,
	)
	if err != nil {
		return false, fmt.Errorf("re-read %s before removal: %w", label, err)
	}
	if !exists {
		return false, nil
	}
	if expected.FileInfo == nil ||
		current.FileInfo == nil ||
		!sameWakeFileIdentity(expected.FileInfo, current.FileInfo) ||
		!bytes.Equal(expected.Raw, current.Raw) {
		return false, fmt.Errorf("%s changed before removal; preserving it", label)
	}
	if expected.Marker.Schema != current.Marker.Schema ||
		expected.Marker.Generation != current.Marker.Generation ||
		expected.Marker.TargetDigest != current.Marker.TargetDigest {
		return false, fmt.Errorf("%s semantics changed before removal; preserving it", label)
	}
	if err := unix.Unlinkat(dirfd, name, 0); err != nil {
		if err == unix.ENOENT {
			return false, nil
		}
		return false, fmt.Errorf("remove %s: %w", label, err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return true, fmt.Errorf("sync %s removal: %w", label, err)
	}
	return true, nil
}

func writeWakeGenerationFileAt(
	dirfd int,
	name string,
	label string,
	marker wakeReady,
) error {
	_, err := writeWakeGenerationFileAtWithSnapshot(dirfd, name, label, marker)
	return err
}

func writeWakeGenerationFileAtWithSnapshot(
	dirfd int,
	name string,
	label string,
	marker wakeReady,
) (wakeGenerationFileSnapshot, error) {
	data, err := json.Marshal(marker)
	if err != nil {
		return wakeGenerationFileSnapshot{}, fmt.Errorf("marshal %s: %w", label, err)
	}
	raw := append(data, '\n')
	temp, err := writeWakeOwnerTempAt(dirfd, "wake-generation", raw, 0o600)
	if err != nil {
		return wakeGenerationFileSnapshot{}, err
	}
	tempPresent := true
	defer func() {
		if tempPresent {
			_ = unix.Unlinkat(dirfd, temp, 0)
		}
	}()
	tempFD, err := unix.Openat(
		dirfd,
		temp,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return wakeGenerationFileSnapshot{}, fmt.Errorf("open %s temp file: %w", label, err)
	}
	tempFile := os.NewFile(uintptr(tempFD), temp)
	tempInfo, statErr := tempFile.Stat()
	_ = tempFile.Close()
	if statErr != nil {
		return wakeGenerationFileSnapshot{}, fmt.Errorf("stat %s temp file: %w", label, statErr)
	}
	if !tempInfo.Mode().IsRegular() || tempInfo.Mode().Perm() != 0o600 {
		return wakeGenerationFileSnapshot{}, fmt.Errorf("%s temp must be a regular 0600 file", label)
	}
	if err := validateWakeTargetPathOwnership(label+" temp", temp, tempInfo); err != nil {
		return wakeGenerationFileSnapshot{}, err
	}
	snapshot := wakeGenerationFileSnapshot{
		Marker:   marker,
		Raw:      bytes.Clone(raw),
		FileInfo: tempInfo,
	}
	if err := unix.Renameat(dirfd, temp, dirfd, name); err != nil {
		return wakeGenerationFileSnapshot{}, fmt.Errorf("install %s: %w", label, err)
	}
	tempPresent = false
	installedFD, err := unix.Openat(
		dirfd,
		name,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return snapshot, fmt.Errorf("open installed %s: %w", label, err)
	}
	installedFile := os.NewFile(uintptr(installedFD), name)
	installedInfo, statErr := installedFile.Stat()
	if statErr != nil {
		_ = installedFile.Close()
		return snapshot, fmt.Errorf("stat installed %s: %w", label, statErr)
	}
	if !os.SameFile(tempInfo, installedInfo) {
		_ = installedFile.Close()
		return snapshot, fmt.Errorf("installed %s changed during publication; preserving it", label)
	}
	snapshot.FileInfo = installedInfo
	if !installedInfo.Mode().IsRegular() || installedInfo.Mode().Perm() != 0o600 {
		_ = installedFile.Close()
		return snapshot, fmt.Errorf("installed %s must be a regular 0600 file", label)
	}
	installedRaw, readErr := readWakeMetadata(installedFile, label, name)
	_ = installedFile.Close()
	if readErr != nil {
		return snapshot, readErr
	}
	if !bytes.Equal(installedRaw, raw) {
		return snapshot, fmt.Errorf("installed %s content changed during publication; preserving it", label)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return snapshot, fmt.Errorf("sync %s directory: %w", label, err)
	}
	return snapshot, nil
}
