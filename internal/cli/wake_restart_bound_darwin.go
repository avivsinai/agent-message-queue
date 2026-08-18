//go:build darwin

package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

var linkDarwinWakeRestartStage = os.Link
var copyDarwinWakeRestartStageFn = copyDarwinWakeRestartStage

func bindWakeRestartCandidatePlatform(candidate wakeImageEvidenceV1) (*wakeRestartBoundImage, error) {
	return bindWakeRestartCandidateAtPlatform(candidate, "")
}

func bindWakeRestartCandidateForRecordPlatform(record wakeRestartRecord) (*wakeRestartBoundImage, error) {
	return bindWakeRestartCandidateAtPlatform(record.Candidate, record.StagePath)
}

func bindWakeRestartCandidateAtPlatform(candidate wakeImageEvidenceV1, plannedStagePath string) (*wakeRestartBoundImage, error) {
	sourceLstat, err := os.Lstat(candidate.ExecutionPath)
	if err != nil {
		return nil, fmt.Errorf("stat wake restart candidate: %w", err)
	}
	if sourceLstat.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("wake restart candidate must not be a symlink")
	}
	sourceFD, err := unix.Open(candidate.ExecutionPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open wake restart candidate: %w", err)
	}
	source := os.NewFile(uintptr(sourceFD), candidate.ExecutionPath)
	if source == nil {
		_ = unix.Close(sourceFD)
		return nil, fmt.Errorf("bind wake restart candidate file descriptor")
	}
	defer func() { _ = source.Close() }()
	sourceOpened, err := source.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened wake restart candidate: %w", err)
	}
	if !os.SameFile(sourceLstat, sourceOpened) {
		return nil, fmt.Errorf("wake restart candidate changed while binding")
	}
	requestedSource, err := captureWakeImageEvidenceFromOpenFile(
		source,
		candidate.ExecutionPath,
		candidate.EmbeddedVersion,
		wakeImageMethodPathnameObserved,
	)
	if err != nil {
		return nil, err
	}
	if requestedSource != candidate {
		return nil, fmt.Errorf("wake restart candidate changed before Darwin staging")
	}

	var stageDir string
	if plannedStagePath == "" {
		stageDir, err = os.MkdirTemp(
			filepath.Dir(candidate.ExecutionPath),
			"."+filepath.Base(candidate.ExecutionPath)+".amq-restart-",
		)
		if err != nil {
			return nil, fmt.Errorf("create adjacent wake restart stage: %w", err)
		}
	} else {
		stageDir = filepath.Dir(plannedStagePath)
		if filepath.Base(plannedStagePath) != filepath.Base(candidate.ExecutionPath) {
			return nil, fmt.Errorf("planned Darwin wake restart stage path is invalid")
		}
		if err := prepareStableDarwinWakeRestartStageParent(plannedStagePath); err != nil {
			return nil, err
		}
		if err := os.Mkdir(stageDir, 0o700); err != nil {
			return nil, fmt.Errorf("create planned Darwin wake restart stage: %w", err)
		}
	}
	stagePath := filepath.Join(stageDir, filepath.Base(candidate.ExecutionPath))
	stageCreated := false
	stageIdentityKnown := false
	var stageIdentity os.FileInfo
	defer func() {
		if !stageCreated {
			_ = os.Remove(stageDir)
			return
		}
		if stageIdentityKnown {
			if current, statErr := os.Lstat(stagePath); statErr == nil && os.SameFile(stageIdentity, current) {
				_ = os.Remove(stagePath)
				_ = os.Remove(stageDir)
			}
		}
	}()
	linked := true
	if err := linkDarwinWakeRestartStage(candidate.ExecutionPath, stagePath); errors.Is(err, syscall.EXDEV) {
		linked = false
		if err := copyDarwinWakeRestartStageFn(source, stagePath); err != nil {
			return nil, fmt.Errorf("copy cross-device wake restart candidate into stage: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("hardlink wake restart candidate into stage: %w", err)
	}
	stageCreated = true
	stageLstat, err := os.Lstat(stagePath)
	if err != nil {
		return nil, fmt.Errorf("stat staged wake restart image: %w", err)
	}
	stageIdentity = stageLstat
	stageIdentityKnown = true
	if err := syncDarwinWakeRestartStageDirectory(stageDir); err != nil {
		return nil, fmt.Errorf("sync Darwin wake restart stage directory: %w", err)
	}
	if stageLstat.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("staged wake restart image must not be a symlink")
	}
	stageFD, err := unix.Open(stagePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open staged wake restart image: %w", err)
	}
	stage := os.NewFile(uintptr(stageFD), stagePath)
	if stage == nil {
		_ = unix.Close(stageFD)
		return nil, fmt.Errorf("bind staged wake restart image file descriptor")
	}
	bound := &wakeRestartBoundImage{
		file:          stage,
		executionPath: stagePath,
	}
	failed := true
	defer func() {
		if failed {
			_ = stage.Close()
		}
	}()
	stageOpened, err := stage.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened staged wake restart image: %w", err)
	}
	postLinkSource, err := source.Stat()
	if err != nil {
		return nil, fmt.Errorf("re-stat linked wake restart source: %w", err)
	}
	if !os.SameFile(sourceOpened, postLinkSource) || !os.SameFile(stageLstat, stageOpened) ||
		(linked && !os.SameFile(postLinkSource, stageLstat)) {
		return nil, fmt.Errorf("darwin wake restart source and stage identities do not match")
	}
	postLinkSourceEvidence, err := captureWakeImageEvidenceFromOpenFile(
		source,
		candidate.ExecutionPath,
		candidate.EmbeddedVersion,
		wakeImageMethodPathnameObserved,
	)
	if err != nil {
		return nil, err
	}
	bound.evidence, err = captureWakeImageEvidenceFromOpenFile(
		stage,
		stagePath,
		candidate.EmbeddedVersion,
		wakeImageMethodPathnameExecObserved,
	)
	if err != nil {
		return nil, err
	}
	if linked && !sameWakeImageEvidenceExceptMethodPath(postLinkSourceEvidence, bound.evidence) {
		return nil, fmt.Errorf("darwin wake restart source and staged image changed while hashing")
	}
	if !linked && !sameWakeImageContent(postLinkSourceEvidence, bound.evidence) {
		return nil, fmt.Errorf("copied Darwin wake restart stage content differs from source")
	}
	if !sameRequestedAndBoundWakeImageEvidence(candidate, bound.evidence) {
		return nil, fmt.Errorf("darwin staged wake restart image differs from requested candidate")
	}
	stageCreated = false
	failed = false
	return bound, nil
}

func syncDarwinWakeRestartStageDirectory(stageDir string) error {
	fd, err := unix.Open(stageDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.Fsync(fd)
}

func copyDarwinWakeRestartStage(source *os.File, stagePath string) error {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind wake restart candidate for copy: %w", err)
	}
	fd, err := unix.Open(stagePath, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o700)
	if err != nil {
		return err
	}
	destination := os.NewFile(uintptr(fd), stagePath)
	if destination == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("bind copied Darwin wake restart stage")
	}
	published := false
	defer func() {
		_ = destination.Close()
		if !published {
			_ = os.Remove(stagePath)
		}
	}()
	if _, err := io.Copy(destination, source); err != nil {
		return err
	}
	if err := destination.Sync(); err != nil {
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind wake restart candidate after copy: %w", err)
	}
	published = true
	return nil
}

func prepareStableDarwinWakeRestartStageParent(stagePath string) error {
	parts, stable, err := stableDarwinWakeRestartStageParts(stagePath)
	if err != nil || !stable {
		return err
	}
	root, err := darwinWakeRestartStageRoot()
	if err != nil {
		return err
	}
	parent := filepath.Dir(filepath.Dir(stagePath))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create stable Darwin wake restart stage hierarchy: %w", err)
	}
	for _, managed := range []string{
		root,
		filepath.Join(root, parts[0]),
		parent,
	} {
		info, err := os.Lstat(managed)
		if err != nil {
			return fmt.Errorf("inspect stable Darwin wake restart stage hierarchy: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 ||
			stat.Uid != uint32(os.Geteuid()) {
			return fmt.Errorf("refuse unsafe stable Darwin wake restart stage hierarchy")
		}
	}
	return nil
}

func stableDarwinWakeRestartStageParts(stagePath string) ([]string, bool, error) {
	root, err := darwinWakeRestartStageRoot()
	if err != nil {
		return nil, false, err
	}
	rel, err := filepath.Rel(root, stagePath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false, nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 4 || len(parts[0]) != sha256HexLength {
		return nil, true, fmt.Errorf("planned stable Darwin wake restart stage path is invalid")
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return nil, true, fmt.Errorf("planned stable Darwin wake restart root identity is invalid")
	}
	if err := fsq.ValidateHandle(parts[1]); err != nil || !validWakeReloadTransportGeneration(parts[2]) ||
		parts[3] == "" || filepath.Base(parts[3]) != parts[3] {
		return nil, true, fmt.Errorf("planned stable Darwin wake restart stage path is invalid")
	}
	return parts, true, nil
}

func wakeRestartStageUsesStableStatePlatform(stagePath string) (bool, error) {
	_, stable, err := stableDarwinWakeRestartStageParts(stagePath)
	return stable, err
}

func sameWakeImageContent(first, second wakeImageEvidenceV1) bool {
	first.Method = second.Method
	first.ExecutionPath = second.ExecutionPath
	first.Device = second.Device
	first.Inode = second.Inode
	first.CTimeNS = second.CTimeNS
	return first == second
}

func boundWakeRestartPreflightCommandPlatform(image *wakeRestartBoundImage) (string, []*os.File, error) {
	if image == nil || image.file == nil || image.executionPath == "" {
		return "", nil, fmt.Errorf("bound Darwin wake restart image is missing")
	}
	return image.executionPath, nil, nil
}

func revalidateBoundWakeRestartImagePlatform(image *wakeRestartBoundImage) error {
	if image == nil || image.file == nil {
		return fmt.Errorf("bound Darwin wake restart image is missing")
	}
	pathInfo, err := os.Lstat(image.executionPath)
	if err != nil {
		return fmt.Errorf("stat bound Darwin wake restart image: %w", err)
	}
	opened, err := image.file.Stat()
	if err != nil {
		return fmt.Errorf("stat opened bound Darwin wake restart image: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(pathInfo, opened) {
		return fmt.Errorf("bound Darwin wake restart path changed during preflight")
	}
	evidence, err := captureWakeImageEvidenceFromOpenFile(
		image.file,
		image.executionPath,
		image.evidence.EmbeddedVersion,
		wakeImageMethodPathnameExecObserved,
	)
	if err != nil {
		return err
	}
	if !sameDarwinStagedWakeImageEvidence(evidence, image.evidence) {
		return fmt.Errorf("bound Darwin wake restart image changed during preflight")
	}
	return nil
}

func verifyWakeResumeBoundImagePlatform(bound wakeImageEvidenceV1) (wakeImageEvidenceV1, error) {
	path, err := os.Executable()
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("resolve running wake image: %w", err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("resolve running wake image path: %w", err)
	}
	path = filepath.Clean(path)
	if path != bound.ExecutionPath {
		return wakeImageEvidenceV1{}, fmt.Errorf("running wake path does not match bound restart path")
	}
	actual, err := captureWakeImageEvidence(path, bound.EmbeddedVersion)
	if err != nil {
		return wakeImageEvidenceV1{}, err
	}
	actual.Method = wakeImageMethodPathnameExecObserved
	if !sameDarwinStagedWakeImageEvidence(actual, bound) {
		return wakeImageEvidenceV1{}, fmt.Errorf("running wake image does not match bound restart image")
	}
	return actual, nil
}

func cleanupBoundWakeRestartImagePlatform(image wakeRestartBoundImage) error {
	if image.evidence.ExecutionPath == "" {
		return nil
	}
	return cleanupDarwinWakeRestartStage(image.evidence, true)
}

func cleanupWakeResumeBoundImagePlatform(bound wakeImageEvidenceV1) error {
	return cleanupDarwinWakeRestartStage(bound, true)
}

func validPreviousWakeRestartBoundImagePlatform(bound wakeImageEvidenceV1) bool {
	if bound.Platform != "darwin" || !wakeImageMethodIsDarwinExecObserved(bound.Method) {
		return false
	}
	stagePath := bound.ExecutionPath
	stageBase := filepath.Base(stagePath)
	stageDirBase := filepath.Base(filepath.Dir(stagePath))
	prefix := "." + stageBase + ".amq-restart-"
	legacy := stageBase != "." && stageBase != string(filepath.Separator) &&
		strings.HasPrefix(stageDirBase, prefix) && len(stageDirBase) > len(prefix)
	if legacy {
		return true
	}
	_, stable, err := stableDarwinWakeRestartStageParts(stagePath)
	return err == nil && stable
}

func cleanupDarwinWakeRestartStage(bound wakeImageEvidenceV1, allowNamespaceCTimeChange bool) error {
	if !validPreviousWakeRestartBoundImagePlatform(bound) {
		return fmt.Errorf("refuse cleanup of non-staged Darwin wake image")
	}
	stagePath := bound.ExecutionPath
	stageDir := filepath.Dir(stagePath)
	lstat, err := os.Lstat(stagePath)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			if err := removeEmptyDarwinWakeRestartStageDir(stagePath); err != nil {
				return err
			}
			return pruneEmptyStableDarwinWakeRestartStageHierarchy(stagePath)
		}
		return fmt.Errorf("stat Darwin wake restart stage: %w", err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse cleanup of replaced Darwin wake restart stage")
	}
	fd, err := unix.Open(stagePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open Darwin wake restart stage for cleanup: %w", err)
	}
	file := os.NewFile(uintptr(fd), stagePath)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("bind Darwin wake restart stage for cleanup")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(lstat, opened) {
		return fmt.Errorf("darwin wake restart stage changed before cleanup")
	}
	evidence, err := captureWakeImageEvidenceFromOpenFile(
		file,
		stagePath,
		bound.EmbeddedVersion,
		wakeImageMethodPathnameExecObserved,
	)
	if err != nil {
		return err
	}
	if evidence != bound &&
		(!allowNamespaceCTimeChange || !sameDarwinStagedWakeImageEvidence(evidence, bound)) {
		return fmt.Errorf("refuse cleanup of changed Darwin wake restart stage")
	}
	confirmed, err := os.Lstat(stagePath)
	if err != nil || !os.SameFile(lstat, confirmed) {
		return fmt.Errorf("darwin wake restart stage changed at cleanup boundary")
	}
	if err := os.Remove(stagePath); err != nil {
		return fmt.Errorf("remove Darwin wake restart stage: %w", err)
	}
	if err := os.Remove(stageDir); err != nil {
		return fmt.Errorf("remove Darwin wake restart stage directory: %w", err)
	}
	return pruneEmptyStableDarwinWakeRestartStageHierarchy(stagePath)
}
