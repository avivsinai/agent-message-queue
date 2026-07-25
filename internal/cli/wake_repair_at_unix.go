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
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type wakeRepairFloorSnapshot struct {
	Floor    wakeRepairFloor
	Raw      []byte
	FileInfo os.FileInfo
}

type wakeRepairFloorAuthority struct {
	ChildGeneration   string
	SourceFloorDigest string
	RawDigest         string
	FileIdentity      wakeFileIdentity
}

const wakeRepairFloorQuarantinePrefix = ".wake.repair-floor.quarantine."

var renameWakeRepairFloorNoReplaceAt = renameWakeRepairNoReplaceAt

func newWakeRepairFloorAuthority(snapshot wakeRepairFloorSnapshot) (wakeRepairFloorAuthority, error) {
	if snapshot.FileInfo == nil {
		return wakeRepairFloorAuthority{}, fmt.Errorf("wake repair floor file identity is missing")
	}
	identity, ok := captureWakeFileIdentity(snapshot.FileInfo)
	if !ok {
		return wakeRepairFloorAuthority{}, fmt.Errorf("capture wake repair floor file identity")
	}
	authority := wakeRepairFloorAuthority{
		ChildGeneration:   snapshot.Floor.Generation,
		SourceFloorDigest: snapshot.Floor.SourceFloorDigest,
		RawDigest:         wakeMetadataDigest(snapshot.Raw),
		FileIdentity:      identity,
	}
	if err := authority.validate(); err != nil {
		return wakeRepairFloorAuthority{}, err
	}
	return authority, nil
}

func (authority wakeRepairFloorAuthority) validate() error {
	if strings.TrimSpace(authority.ChildGeneration) == "" {
		return fmt.Errorf("wake repair child generation is missing")
	}
	if strings.TrimSpace(authority.SourceFloorDigest) == "" {
		return fmt.Errorf("wake repair source floor digest is missing")
	}
	if err := validateWakeRepairHandoffDigest("floor raw", authority.RawDigest); err != nil {
		return err
	}
	if authority.FileIdentity.Device == 0 || authority.FileIdentity.Inode == 0 {
		return fmt.Errorf("wake repair floor file identity is invalid")
	}
	return nil
}

func sameWakeRepairFloorAuthorityAfterRename(
	expected wakeRepairFloorAuthority,
	actual wakeRepairFloorAuthority,
) bool {
	// rename(2) may update ctime on the same file. Device+inode prove that the
	// quarantined file is the captured file instance; the raw digest proves its
	// bytes did not change while it moved.
	return actual.ChildGeneration == expected.ChildGeneration &&
		actual.SourceFloorDigest == expected.SourceFloorDigest &&
		actual.RawDigest == expected.RawDigest &&
		actual.FileIdentity.Device == expected.FileIdentity.Device &&
		actual.FileIdentity.Inode == expected.FileIdentity.Inode
}

func revalidateWakeRepairRootIdentity(root, expected string) error {
	if strings.TrimSpace(expected) == "" {
		return fmt.Errorf("wake repair root identity is missing")
	}
	switch verifyTreeIdentityToken(root, expected) {
	case TreeRelationSame:
		return nil
	case TreeRelationDifferent:
		return fmt.Errorf("wake repair root identity changed")
	default:
		return fmt.Errorf("wake repair root identity is unverified")
	}
}

func readWakeRepairFloorAt(
	dirfd int,
	agentDir *wakeAgentDir,
) (wakeRepairFloor, bool, error) {
	snapshot, exists, err := readWakeRepairFloorSnapshotAt(dirfd, agentDir)
	return snapshot.Floor, exists, err
}

func readWakeRepairFloorSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
) (wakeRepairFloorSnapshot, bool, error) {
	return readWakeRepairFloorSnapshotNamedAt(
		dirfd,
		agentDir,
		wakeRepairFloorFileName,
	)
}

func readWakeRepairFloorSnapshotNamedAt(
	dirfd int,
	agentDir *wakeAgentDir,
	name string,
) (wakeRepairFloorSnapshot, bool, error) {
	path := filepath.Join(agentDir.path, name)
	data, info, exists, err := readWakeRepairMetadataAt(
		dirfd,
		name,
		"wake repair floor",
		path,
		maxWakeRepairFloorFileBytes,
	)
	if err != nil || !exists {
		return wakeRepairFloorSnapshot{}, exists, err
	}
	var floor wakeRepairFloor
	if err := json.Unmarshal(data, &floor); err != nil {
		return wakeRepairFloorSnapshot{}, true, fmt.Errorf("parse wake repair floor: %w", err)
	}
	return wakeRepairFloorSnapshot{
		Floor:    floor,
		Raw:      data,
		FileInfo: info,
	}, true, nil
}

func writeWakeRepairFloorAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	floor wakeRepairFloor,
) error {
	if err := revalidateWakeRepairRootIdentity(root, floor.RootIdentity); err != nil {
		return err
	}
	data, err := json.MarshalIndent(floor, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal wake repair floor: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxWakeRepairFloorFileBytes {
		return fmt.Errorf("wake repair floor has too many existing messages (%d-byte limit)", maxWakeRepairFloorFileBytes)
	}
	return writeWakeRepairMetadataAt(
		dirfd,
		agentDir,
		wakeRepairFloorFileName,
		"wake repair floor",
		data,
		maxWakeRepairFloorFileBytes,
	)
}

func writeWakeRepairFloorAndCaptureAuthorityAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	floor wakeRepairFloor,
) (wakeRepairFloorAuthority, error) {
	if err := writeWakeRepairFloorAt(dirfd, agentDir, root, floor); err != nil {
		return wakeRepairFloorAuthority{}, err
	}
	snapshot, exists, err := readWakeRepairFloorSnapshotAt(dirfd, agentDir)
	if err != nil {
		return wakeRepairFloorAuthority{}, err
	}
	if !exists {
		return wakeRepairFloorAuthority{}, fmt.Errorf("published wake repair floor disappeared")
	}
	return newWakeRepairFloorAuthority(snapshot)
}

func removeWakeRepairFloorGuardedAt(dirfd int, agentDir *wakeAgentDir) error {
	if err := unix.Unlinkat(dirfd, wakeRepairFloorFileName, 0); err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("remove wake repair floor: %w", err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync wake repair floor removal: %w", err)
	}
	_ = agentDir
	return nil
}

func removeWakeRepairFloorIfGenerationGuardedAt(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeRepairFloorAuthority,
) error {
	if err := expected.validate(); err != nil {
		return err
	}
	current, exists, err := readWakeRepairFloorSnapshotAt(dirfd, agentDir)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	currentAuthority, err := newWakeRepairFloorAuthority(current)
	if err != nil {
		return err
	}
	if currentAuthority != expected {
		return nil
	}

	quarantine, moved, err := quarantineWakeRepairFloorAt(dirfd)
	if err != nil {
		return err
	}
	if !moved {
		return nil
	}
	quarantined, exists, inspectErr := readWakeRepairFloorSnapshotNamedAt(
		dirfd,
		agentDir,
		quarantine,
	)
	if inspectErr != nil {
		if restoreErr := restoreWakeRepairFloorQuarantineAt(dirfd, quarantine); restoreErr != nil {
			return errors.Join(
				fmt.Errorf("inspect quarantined wake repair floor: %w", inspectErr),
				restoreErr,
			)
		}
		return fmt.Errorf("inspect quarantined wake repair floor: %w", inspectErr)
	}
	if !exists {
		missingErr := fmt.Errorf("quarantined wake repair floor disappeared before verification")
		if restoreErr := restoreWakeRepairFloorQuarantineAt(dirfd, quarantine); restoreErr != nil {
			return errors.Join(missingErr, restoreErr)
		}
		return missingErr
	}
	quarantinedAuthority, authorityErr := newWakeRepairFloorAuthority(quarantined)
	if authorityErr != nil ||
		!sameWakeRepairFloorAuthorityAfterRename(expected, quarantinedAuthority) {
		restoreErr := restoreWakeRepairFloorQuarantineAt(dirfd, quarantine)
		if authorityErr != nil {
			return errors.Join(
				fmt.Errorf("verify quarantined wake repair floor: %w", authorityErr),
				restoreErr,
			)
		}
		return errors.Join(
			fmt.Errorf("wake repair floor changed before cleanup; preserving it"),
			restoreErr,
		)
	}
	if err := unix.Unlinkat(dirfd, quarantine, 0); err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("remove quarantined wake repair floor: %w", err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync wake repair floor removal: %w", err)
	}
	return nil
}

func quarantineWakeRepairFloorAt(dirfd int) (string, bool, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var nonce [12]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", false, fmt.Errorf("generate wake repair floor quarantine name: %w", err)
		}
		name := fmt.Sprintf(
			"%s%d.%s",
			wakeRepairFloorQuarantinePrefix,
			os.Getpid(),
			hex.EncodeToString(nonce[:]),
		)
		err := renameWakeRepairFloorNoReplaceAt(
			dirfd,
			wakeRepairFloorFileName,
			dirfd,
			name,
		)
		switch {
		case err == nil:
			if err := syncWakeOwnerDirFD(dirfd); err != nil {
				restoreErr := restoreWakeRepairFloorQuarantineAt(dirfd, name)
				return "", false, errors.Join(
					fmt.Errorf("sync wake repair floor quarantine: %w", err),
					restoreErr,
				)
			}
			return name, true, nil
		case errors.Is(err, unix.ENOENT):
			return "", false, nil
		case errors.Is(err, unix.EEXIST):
			continue
		default:
			return "", false, fmt.Errorf("quarantine wake repair floor: %w", err)
		}
	}
	return "", false, fmt.Errorf("quarantine wake repair floor: too many name collisions")
}

func restoreWakeRepairFloorQuarantineAt(dirfd int, quarantine string) error {
	err := renameWakeRepairFloorNoReplaceAt(
		dirfd,
		quarantine,
		dirfd,
		wakeRepairFloorFileName,
	)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf(
				"wake repair floor changed during cleanup; preserved mismatch as %s",
				quarantine,
			)
		}
		return fmt.Errorf(
			"restore mismatched wake repair floor from %s: %w",
			quarantine,
			err,
		)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync restored wake repair floor: %w", err)
	}
	return nil
}

func writeWakeTargetGuardedAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	target wakeTarget,
) error {
	if err := validateWakeTarget(target, root, me); err != nil {
		return err
	}
	data, err := json.MarshalIndent(target, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal wake target: %w", err)
	}
	return writeWakeRepairMetadataAt(
		dirfd,
		agentDir,
		wakeTargetFileName,
		"wake target",
		append(data, '\n'),
		maxWakeMetadataFileBytes,
	)
}

func removeWakeTargetGuardedAt(dirfd int, agentDir *wakeAgentDir) error {
	if err := unix.Unlinkat(dirfd, wakeTargetFileName, 0); err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("remove wake target: %w", err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync wake target removal: %w", err)
	}
	_ = agentDir
	return nil
}

func writeWakeRepairMetadataAt(
	dirfd int,
	agentDir *wakeAgentDir,
	name string,
	label string,
	data []byte,
	maxBytes int,
) error {
	if err := validateWakeRepairMetadataDestinationAt(dirfd, agentDir, name, label); err != nil {
		return err
	}
	temp, err := writeWakeOwnerTempAt(dirfd, strings.TrimLeft(name, "."), data, 0o600)
	if err != nil {
		return err
	}
	tempPresent := true
	defer func() {
		if tempPresent {
			_ = unix.Unlinkat(dirfd, temp, 0)
		}
	}()
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync %s directory before install: %w", label, err)
	}
	if err := validateWakeRepairMetadataDestinationAt(dirfd, agentDir, name, label); err != nil {
		return err
	}
	if err := unix.Renameat(dirfd, temp, dirfd, name); err != nil {
		return fmt.Errorf("install %s: %w", label, err)
	}
	tempPresent = false
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync %s directory after install: %w", label, err)
	}
	installed, _, exists, err := readWakeRepairMetadataAt(dirfd, name, label, filepath.Join(agentDir.path, name), maxBytes)
	if err != nil {
		return fmt.Errorf("verify installed %s: %w", label, err)
	}
	if !exists || !bytes.Equal(installed, data) {
		return fmt.Errorf("installed %s changed before verification", label)
	}
	return nil
}

func validateWakeRepairMetadataDestinationAt(
	dirfd int,
	agentDir *wakeAgentDir,
	name string,
	label string,
) error {
	path := filepath.Join(agentDir.path, name)
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ENOENT {
			return nil
		}
		if err == unix.ELOOP {
			return fmt.Errorf("%s %s must not be a symlink", label, path)
		}
		return fmt.Errorf("open %s destination: %w", label, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s destination: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s %s must be a regular file", label, path)
	}
	return nil
}

func readWakeRepairMetadataAt(
	dirfd int,
	name string,
	label string,
	path string,
	maxBytes int,
) ([]byte, os.FileInfo, bool, error) {
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
			return nil, nil, false, nil
		}
		if err == unix.ELOOP {
			return nil, nil, true, fmt.Errorf("%s %s must not be a symlink", label, path)
		}
		return nil, nil, true, fmt.Errorf("open %s: %w", label, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, true, fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, true, fmt.Errorf("%s %s must be a regular file", label, path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return nil, nil, true, fmt.Errorf("%s %s mode is %o, want 0600", label, path, got)
	}
	if err := validateWakeTargetPathOwnership(label, path, info); err != nil {
		return nil, nil, true, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, nil, true, fmt.Errorf("read %s: %w", label, err)
	}
	if len(data) > maxBytes {
		return nil, nil, true, fmt.Errorf("%s %s is too large", label, path)
	}
	pathFile, err := open()
	if err != nil {
		return nil, nil, true, fmt.Errorf("re-open %s: %w", label, err)
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil {
		return nil, nil, true, fmt.Errorf("re-stat %s: %w", label, statErr)
	}
	if !sameWakeFileIdentity(info, pathInfo) {
		return nil, nil, true, fmt.Errorf("%s changed while opening", label)
	}
	return data, info, true, nil
}
