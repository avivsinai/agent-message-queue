//go:build darwin || linux

package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type wakeStatePublicationBoundary string

const (
	wakeStateAfterTempWrite         wakeStatePublicationBoundary = "after-temp-write"
	wakeStateAfterFileSync          wakeStatePublicationBoundary = "after-file-sync"
	wakeStateAfterPreRenameDirSync  wakeStatePublicationBoundary = "after-pre-rename-dir-sync"
	wakeStateAfterRename            wakeStatePublicationBoundary = "after-rename"
	wakeStateAfterPostRenameDirSync wakeStatePublicationBoundary = "after-post-rename-dir-sync"
	wakeStateAfterVerify            wakeStatePublicationBoundary = "after-verify"
)

var afterWakeStatePublicationBoundary = func(wakeStatePublicationBoundary) error { return nil }
var afterWakeStateSnapshotRead = func() {}

type wakeStateLegacySnapshot struct {
	Target          wakeTargetSnapshot
	TargetPresent   bool
	Prepared        wakeGenerationFileSnapshot
	PreparedPresent bool
}

type wakeStateFileSnapshot struct {
	State    wakeState
	Raw      []byte
	FileInfo os.FileInfo
}

func reconcileWakeStateAfterLegacyMutationAt(
	scope *wakeMutationScope,
	root string,
	me string,
) (retErr error) {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return err
	}
	// Precondition: the caller holds this agent directory's lifecycle guard and
	// has already committed the authorized legacy mutation.
	defer func() {
		if retErr != nil {
			retErr = newWakeStateProjectionError(retErr)
		}
	}()
	_, exists, err := readWakeTargetSnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return err
	}
	if !exists {
		state, stateExists, err := readWakeStateRawSnapshotAt(dirfd, agentDir)
		if err != nil || !stateExists {
			return err
		}
		_, err = removeWakeStateIfSnapshotMatchesAt(scope, state)
		return err
	}
	expected, err := captureWakeStateLegacySnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return err
	}
	_, err = publishWakeStateAt(scope, root, me, expected)
	return err
}

func removeWakeStateIfTargetAbsentAt(
	scope *wakeMutationScope,
	expected wakeStateFileSnapshot,
	expectedExists bool,
) (removed bool, retErr error) {
	dirfd, _, err := scope.location()
	if err != nil {
		return false, err
	}
	// Precondition: the caller holds this agent directory's lifecycle guard and
	// captured expected before removing the target.
	defer func() {
		if retErr != nil {
			retErr = newWakeStateProjectionError(retErr)
		}
	}()
	var targetInfo unix.Stat_t
	err = unix.Fstatat(dirfd, wakeTargetFileName, &targetInfo, unix.AT_SYMLINK_NOFOLLOW)
	targetExists := err == nil
	if err != nil && err != unix.ENOENT {
		return false, fmt.Errorf("verify wake target absence before state removal: %w", err)
	}
	if targetExists || !expectedExists {
		return false, nil
	}
	return removeWakeStateIfSnapshotMatchesAt(scope, expected)
}

func (snapshot wakeStateLegacySnapshot) legacy() wakeStateLegacy {
	legacy := wakeStateLegacy{}
	if snapshot.TargetPresent {
		target := snapshot.Target.Target
		legacy.Target = &target
		legacy.TargetRaw = bytes.Clone(snapshot.Target.Raw)
	}
	if snapshot.PreparedPresent {
		legacy.Prepared = &wakeStateLegacyPrepared{
			Schema:       snapshot.Prepared.Marker.Schema,
			Generation:   snapshot.Prepared.Marker.Generation,
			TargetDigest: snapshot.Prepared.Marker.TargetDigest,
		}
		legacy.PreparedRaw = bytes.Clone(snapshot.Prepared.Raw)
	}
	return legacy
}

func captureWakeStateLegacySnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
) (wakeStateLegacySnapshot, error) {
	if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
		return wakeStateLegacySnapshot{}, err
	}
	target, targetPresent, err := readWakeTargetSnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return wakeStateLegacySnapshot{}, err
	}
	if !targetPresent {
		return wakeStateLegacySnapshot{}, fmt.Errorf("wake state target is missing")
	}
	if err := validateWakeTarget(target.Target, root, me); err != nil {
		return wakeStateLegacySnapshot{}, err
	}
	prepared, preparedPresent, err := readWakeGenerationFileSnapshotAt(
		dirfd,
		agentDir,
		wakePreparedFileName,
		"wake prepared marker",
	)
	if err != nil {
		return wakeStateLegacySnapshot{}, err
	}
	snapshot := wakeStateLegacySnapshot{
		Target:          target,
		TargetPresent:   true,
		Prepared:        prepared,
		PreparedPresent: preparedPresent,
	}
	if _, err := newWakeState(snapshot.legacy()); err != nil {
		return wakeStateLegacySnapshot{}, err
	}
	return snapshot, nil
}

func publishWakeStateAt(
	scope *wakeMutationScope,
	root string,
	me string,
	expected wakeStateLegacySnapshot,
) (wakeStateFileSnapshot, error) {
	return publishWakeStateValidatedAt(scope, root, me, expected, nil)
}

func publishWakeStateValidatedAt(
	scope *wakeMutationScope,
	root string,
	me string,
	expected wakeStateLegacySnapshot,
	validateBeforeInstall func() error,
) (wakeStateFileSnapshot, error) {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return wakeStateFileSnapshot{}, err
	}
	// Precondition: the caller holds this agent directory's lifecycle guard.
	// Every schema generation uses that guard, which excludes legitimate writers
	// across the final destination validation-to-rename window. The fd-bound
	// checks below detect bypassers outside that window and preserve replacements.
	if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
		return wakeStateFileSnapshot{}, err
	}
	current, err := captureWakeStateLegacySnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return wakeStateFileSnapshot{}, err
	}
	if !sameWakeStateLegacySnapshot(expected, current) {
		return wakeStateFileSnapshot{}, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake legacy state changed before state publication"),
		)
	}
	state, err := newWakeState(expected.legacy())
	if err != nil {
		return wakeStateFileSnapshot{}, err
	}
	raw, err := encodeWakeState(state)
	if err != nil {
		return wakeStateFileSnapshot{}, err
	}
	if err := validateWakeStateDestinationAt(dirfd, agentDir); err != nil {
		return wakeStateFileSnapshot{}, err
	}

	tempName, tempFile, err := createWakeStateTempAt(dirfd)
	if err != nil {
		return wakeStateFileSnapshot{}, err
	}
	tempPresent := true
	defer func(scope *wakeMutationScope) {
		_ = tempFile.Close()
		if tempPresent {
			_ = scope.unlinkAt(tempName, 0)
		}
	}(scope)
	if err := tempFile.Chmod(0o600); err != nil {
		return wakeStateFileSnapshot{}, fmt.Errorf("chmod wake state temp: %w", err)
	}
	n, err := tempFile.Write(raw)
	if err != nil {
		return wakeStateFileSnapshot{}, fmt.Errorf("write wake state temp: %w", err)
	}
	if n != len(raw) {
		return wakeStateFileSnapshot{}, fmt.Errorf("write wake state temp: %w", io.ErrShortWrite)
	}
	if err := runWakeStatePublicationBoundary(wakeStateAfterTempWrite); err != nil {
		return wakeStateFileSnapshot{}, err
	}
	if err := tempFile.Sync(); err != nil {
		return wakeStateFileSnapshot{}, fmt.Errorf("sync wake state temp: %w", err)
	}
	if err := runWakeStatePublicationBoundary(wakeStateAfterFileSync); err != nil {
		return wakeStateFileSnapshot{}, err
	}
	tempInfo, err := tempFile.Stat()
	if err != nil {
		return wakeStateFileSnapshot{}, fmt.Errorf("stat wake state temp: %w", err)
	}
	if err := validateWakeStateFileInfo(tempName, tempInfo); err != nil {
		return wakeStateFileSnapshot{}, err
	}
	if err := tempFile.Close(); err != nil {
		return wakeStateFileSnapshot{}, fmt.Errorf("close wake state temp: %w", err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return wakeStateFileSnapshot{}, fmt.Errorf("sync wake state directory before install: %w", err)
	}
	if err := runWakeStatePublicationBoundary(wakeStateAfterPreRenameDirSync); err != nil {
		return wakeStateFileSnapshot{}, err
	}

	if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
		return wakeStateFileSnapshot{}, err
	}
	current, err = captureWakeStateLegacySnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return wakeStateFileSnapshot{}, err
	}
	if !sameWakeStateLegacySnapshot(expected, current) {
		return wakeStateFileSnapshot{}, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake legacy state changed before state install"),
		)
	}
	if err := validateWakeStateDestinationAt(dirfd, agentDir); err != nil {
		return wakeStateFileSnapshot{}, err
	}
	if validateBeforeInstall != nil {
		if err := validateBeforeInstall(); err != nil {
			return wakeStateFileSnapshot{}, err
		}
	}
	if err := scope.renameAt(dirfd, tempName, dirfd, wakeStateFileName); err != nil {
		return wakeStateFileSnapshot{}, fmt.Errorf("install wake state: %w", err)
	}
	tempPresent = false
	if err := runWakeStatePublicationBoundary(wakeStateAfterRename); err != nil {
		return wakeStateFileSnapshot{}, err
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return wakeStateFileSnapshot{}, fmt.Errorf("sync wake state directory after install: %w", err)
	}
	if err := runWakeStatePublicationBoundary(wakeStateAfterPostRenameDirSync); err != nil {
		return wakeStateFileSnapshot{}, err
	}
	if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
		return wakeStateFileSnapshot{}, err
	}
	installed, exists, err := readWakeStateSnapshotAt(dirfd, agentDir)
	if err != nil {
		return wakeStateFileSnapshot{}, err
	}
	if !exists {
		return wakeStateFileSnapshot{}, fmt.Errorf("installed wake state disappeared")
	}
	if !os.SameFile(tempInfo, installed.FileInfo) || !bytes.Equal(raw, installed.Raw) {
		return installed, fmt.Errorf("installed wake state changed during publication; preserving it")
	}
	current, err = captureWakeStateLegacySnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return installed, err
	}
	if !sameWakeStateLegacySnapshot(expected, current) {
		return installed, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake legacy state changed during state verification"),
		)
	}
	if err := validateWakeStateAgainstLegacy(installed.State, current.legacy()); err != nil {
		return installed, err
	}
	if err := runWakeStatePublicationBoundary(wakeStateAfterVerify); err != nil {
		return installed, err
	}
	return installed, nil
}

func publishWakeStateAndBindLockAt(
	scope *wakeMutationScope,
	root string,
	me string,
	lock *wakeLock,
) error {
	if lock == nil {
		return fmt.Errorf("wake lock is missing")
	}
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return err
	}
	expected, err := captureWakeStateLegacySnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return err
	}
	installed, err := publishWakeStateAt(scope, root, me, expected)
	if err != nil {
		return err
	}
	if installed.State.Target.TargetDigest != lock.TargetDigest {
		return fmt.Errorf("published wake state target digest does not match wake lock target")
	}
	lock.StateGeneration = lock.Generation
	lock.StateDigest = installed.State.Target.TargetDigest
	if err := validateWakeLockStateBinding(*lock); err != nil {
		return fmt.Errorf("bind wake lock to published state: %w", err)
	}
	return nil
}

func validateWakeBoundStateAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	lock wakeLock,
) error {
	if err := validateWakeLockStateBinding(lock); err != nil {
		return err
	}
	if lock.StateGeneration == "" {
		return fmt.Errorf("wake lock is not state-bound")
	}
	state, exists, err := readWakeStateSnapshotAt(dirfd, agentDir)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("bound wake state is missing")
	}
	legacy, err := captureWakeStateLegacySnapshotAt(dirfd, agentDir, root, me)
	if err != nil {
		return err
	}
	if err := validateWakeStateAgainstLegacy(state.State, legacy.legacy()); err != nil {
		return err
	}
	if state.State.Target.TargetDigest != lock.TargetDigest || state.State.Target.TargetDigest != lock.StateDigest {
		return fmt.Errorf("bound wake state target digest does not match wake lock")
	}
	return nil
}

func readWakeStateSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
) (wakeStateFileSnapshot, bool, error) {
	return readWakeStateSnapshotAtWithCanonicalValidation(dirfd, agentDir, true)
}

func readWakeStateSnapshotAtWithCanonicalValidation(
	dirfd int,
	agentDir *wakeAgentDir,
	validateCanonical bool,
) (wakeStateFileSnapshot, bool, error) {
	snapshot, exists, err := readWakeStateRawSnapshotAtWithCanonicalValidation(
		dirfd, agentDir, validateCanonical,
	)
	if err != nil || !exists {
		return snapshot, exists, err
	}
	state, err := decodeWakeState(snapshot.Raw)
	if err != nil {
		return wakeStateFileSnapshot{}, true, err
	}
	snapshot.State = state
	return snapshot, true, nil
}

func readWakeStateRawSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
) (wakeStateFileSnapshot, bool, error) {
	return readWakeStateRawSnapshotAtWithCanonicalValidation(dirfd, agentDir, true)
}

func readWakeStateRawSnapshotAtWithCanonicalValidation(
	dirfd int,
	agentDir *wakeAgentDir,
	validateCanonical bool,
) (wakeStateFileSnapshot, bool, error) {
	if validateCanonical {
		if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
			return wakeStateFileSnapshot{}, false, err
		}
	}
	if agentDir == nil || agentDir.file == nil || dirfd != int(agentDir.file.Fd()) {
		return wakeStateFileSnapshot{}, false, fmt.Errorf("wake state agent directory capability is missing")
	}
	path := filepath.Join(agentDir.path, wakeStateFileName)
	open := func() (*os.File, error) {
		fd, err := unix.Openat(
			dirfd,
			wakeStateFileName,
			unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
			0,
		)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	file, err := open()
	if err != nil {
		if err == unix.ENOENT {
			return wakeStateFileSnapshot{}, false, nil
		}
		return wakeStateFileSnapshot{}, true, fmt.Errorf("open wake state: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return wakeStateFileSnapshot{}, true, fmt.Errorf("stat wake state: %w", err)
	}
	if err := validateWakeStateFileInfo(path, info); err != nil {
		return wakeStateFileSnapshot{}, true, err
	}
	raw, err := readWakeMetadata(file, "wake state", path)
	if err != nil {
		return wakeStateFileSnapshot{}, true, err
	}
	afterWakeStateSnapshotRead()
	pathFile, err := open()
	if err != nil {
		return wakeStateFileSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake state changed while reopening: %w", err),
		)
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil {
		return wakeStateFileSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake state changed while restating: %w", statErr),
		)
	}
	if !sameWakeFileIdentity(info, pathInfo) {
		return wakeStateFileSnapshot{}, true, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake state changed while opening"),
		)
	}
	if err := validateWakeStateFileInfo(path, pathInfo); err != nil {
		return wakeStateFileSnapshot{}, true, err
	}
	return wakeStateFileSnapshot{
		Raw:      bytes.Clone(raw),
		FileInfo: info,
	}, true, nil
}

func removeWakeStateIfSnapshotMatchesAt(
	scope *wakeMutationScope,
	expected wakeStateFileSnapshot,
) (bool, error) {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return false, err
	}
	// Precondition: the caller holds this agent directory's lifecycle guard.
	// The guard closes the check-to-unlink window against legitimate writers;
	// the exact fd-bound snapshot check preserves bypassing replacements.
	if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
		return false, err
	}
	if _, _, newer := newerWakeStateSchema(expected.Raw); newer {
		// A newer reader will reject this now-stale projection by its legacy
		// digest, so preserving it cannot authorize state while retaining data
		// that only the newer reader or an operator can interpret.
		return false, fmt.Errorf("wake state uses a newer schema; preserving it")
	}
	current, exists, err := readWakeStateRawSnapshotAt(dirfd, agentDir)
	if err != nil {
		return false, fmt.Errorf("re-read wake state before removal: %w", err)
	}
	if !exists {
		return false, nil
	}
	if expected.FileInfo == nil || current.FileInfo == nil ||
		!sameWakeFileIdentity(expected.FileInfo, current.FileInfo) ||
		!bytes.Equal(expected.Raw, current.Raw) {
		return false, fmt.Errorf("wake state changed before removal; preserving it")
	}
	if err := scope.unlinkAt(wakeStateFileName, 0); err != nil {
		if err == unix.ENOENT {
			return false, nil
		}
		return false, fmt.Errorf("remove wake state: %w", err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return true, fmt.Errorf("sync wake state removal: %w", err)
	}
	return true, nil
}

func sameWakeStateLegacySnapshot(first, second wakeStateLegacySnapshot) bool {
	if first.TargetPresent != second.TargetPresent ||
		first.PreparedPresent != second.PreparedPresent {
		return false
	}
	if first.TargetPresent {
		if first.Target.FileInfo == nil || second.Target.FileInfo == nil ||
			!sameWakeFileIdentity(first.Target.FileInfo, second.Target.FileInfo) ||
			!bytes.Equal(first.Target.Raw, second.Target.Raw) ||
			!sameWakeTarget(first.Target.Target, second.Target.Target) {
			return false
		}
	}
	if first.PreparedPresent {
		if first.Prepared.Failure != nil || second.Prepared.Failure != nil {
			if first.Prepared.Failure == nil || second.Prepared.Failure == nil ||
				*first.Prepared.Failure != *second.Prepared.Failure {
				return false
			}
			return true
		}
		if first.Prepared.FileInfo == nil || second.Prepared.FileInfo == nil ||
			!sameWakeFileIdentity(first.Prepared.FileInfo, second.Prepared.FileInfo) ||
			!bytes.Equal(first.Prepared.Raw, second.Prepared.Raw) ||
			first.Prepared.Marker != second.Prepared.Marker {
			return false
		}
	}
	return true
}

func validateWakeStateDestinationAt(dirfd int, agentDir *wakeAgentDir) error {
	current, exists, err := readWakeStateRawSnapshotAt(dirfd, agentDir)
	if err != nil || !exists {
		return err
	}
	if _, _, newer := newerWakeStateSchema(current.Raw); newer {
		// The authorized legacy mutation has already committed. Preserve newer
		// state; its legacy digest becomes stale and therefore non-authoritative.
		return fmt.Errorf("wake state uses a newer schema; preserving it")
	}
	return nil
}

func validateWakeStateFileInfo(path string, info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("wake state %s must be a regular 0600 file", path)
	}
	return validateWakeTargetPathOwnership("wake state", path, info)
}

func validateWakeStateAgentDirAt(dirfd int, agentDir *wakeAgentDir) error {
	if agentDir == nil || agentDir.file == nil || dirfd != int(agentDir.file.Fd()) {
		return fmt.Errorf("wake state agent directory capability is missing")
	}
	retainedInfo, err := agentDir.file.Stat()
	if err != nil {
		return fmt.Errorf("stat retained wake agent directory: %w", err)
	}
	fd, err := unix.Open(
		agentDir.path,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return fmt.Errorf("canonical wake agent directory no longer matches retained authority: %w", err)
	}
	canonical := os.NewFile(uintptr(fd), agentDir.path)
	defer func() { _ = canonical.Close() }()
	canonicalInfo, err := canonical.Stat()
	if err != nil {
		return fmt.Errorf("stat canonical wake agent directory: %w", err)
	}
	if err := validateWakeAgentDir(agentDir.path, canonicalInfo); err != nil {
		return err
	}
	if !os.SameFile(retainedInfo, canonicalInfo) {
		return fmt.Errorf("canonical wake agent directory no longer matches retained authority")
	}
	return nil
}

func validateWakeStateRetainedAgentDirAt(dirfd int, agentDir *wakeAgentDir) error {
	if agentDir == nil || agentDir.file == nil || dirfd != int(agentDir.file.Fd()) {
		return fmt.Errorf("wake state agent directory capability is missing")
	}
	info, err := agentDir.file.Stat()
	if err != nil {
		return fmt.Errorf("stat retained wake agent directory: %w", err)
	}
	return validateWakeAgentDir(agentDir.path, info)
}

func createWakeStateTempAt(dirfd int) (string, *os.File, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, fmt.Errorf("generate wake state temp name: %w", err)
	}
	name := fmt.Sprintf(".wake-state.tmp.%d.%s", os.Getpid(), hex.EncodeToString(nonce[:]))
	fd, err := unix.Openat(
		dirfd,
		name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return "", nil, fmt.Errorf("create wake state temp: %w", err)
	}
	return name, os.NewFile(uintptr(fd), name), nil
}

func runWakeStatePublicationBoundary(boundary wakeStatePublicationBoundary) error {
	if err := afterWakeStatePublicationBoundary(boundary); err != nil {
		return fmt.Errorf("wake state publication %s: %w", boundary, err)
	}
	return nil
}
