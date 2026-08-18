//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

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
		if err := os.Mkdir(stageDir, 0o700); err != nil {
			return nil, fmt.Errorf("create planned adjacent wake restart stage: %w", err)
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
	if err := os.Link(candidate.ExecutionPath, stagePath); err != nil {
		return nil, fmt.Errorf("hardlink wake restart candidate into adjacent stage: %w", err)
	}
	stageCreated = true
	stageLstat, err := os.Lstat(stagePath)
	if err != nil {
		return nil, fmt.Errorf("stat staged wake restart image: %w", err)
	}
	stageIdentity = stageLstat
	stageIdentityKnown = true
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
	if !os.SameFile(sourceOpened, postLinkSource) || !os.SameFile(postLinkSource, stageLstat) ||
		!os.SameFile(stageLstat, stageOpened) {
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
	if !sameWakeImageEvidenceExceptMethodPath(postLinkSourceEvidence, bound.evidence) {
		return nil, fmt.Errorf("darwin wake restart source and staged image changed while hashing")
	}
	if !sameRequestedAndBoundWakeImageEvidence(candidate, bound.evidence) {
		return nil, fmt.Errorf("darwin staged wake restart image differs from requested candidate")
	}
	stageCreated = false
	failed = false
	return bound, nil
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
	return stageBase != "." && stageBase != string(filepath.Separator) &&
		strings.HasPrefix(stageDirBase, prefix) && len(stageDirBase) > len(prefix)
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
			return removeEmptyDarwinWakeRestartStageDir(stagePath)
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
	return nil
}
