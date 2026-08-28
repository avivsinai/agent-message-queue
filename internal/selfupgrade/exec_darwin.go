//go:build darwin

package selfupgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const darwinCodeSignProbeTimeout = 5 * time.Second

var (
	selfUpgradeCodesignPath  = "/usr/bin/codesign"
	selfUpgradeCodesignProbe = RunBoundedProbe
	selfUpgradeDarwinExec    = syscall.Exec
)

func execSupportedPlatform() bool { return true }

func verifyDarwinCodeSignature(stagePath string) error {
	if err := verifyDarwinSystemTool(selfUpgradeCodesignPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("verify Darwin code signature tool: %w", err)
		}
		return fmt.Errorf(
			"verify Darwin code signature: codesign is unavailable; install Xcode Command Line Tools with xcode-select --install: %w",
			err,
		)
	}
	ctx, cancel := context.WithTimeout(context.Background(), darwinCodeSignProbeTimeout)
	defer cancel()
	out, err := selfUpgradeCodesignProbe(
		ctx,
		selfUpgradeCodesignPath,
		[]string{"--verify", "--strict", stagePath},
		BoundedProbeOptions{Env: os.Environ()},
	)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf(
				"verify Darwin code signature: codesign probe timed out: %w",
				errors.Join(ctx.Err(), err),
			)
		}
		return fmt.Errorf("verify Darwin code signature for %s: %w", stagePath, err)
	}
	if diagnostic := strings.TrimSpace(string(out)); diagnostic != "" {
		return fmt.Errorf("verify Darwin code signature for %s: ambiguous codesign output %q", stagePath, diagnostic)
	}
	return nil
}

func VerifyDarwinCodeSignature(stagePath string) error {
	return verifyDarwinCodeSignature(stagePath)
}

func execImagePlatform(candidate ImageEvidence, argv, env []string) error {
	if candidate.ExecutionPath == "" {
		return errors.New("self-upgrade candidate path is empty")
	}
	sourceInfo, err := os.Lstat(candidate.ExecutionPath)
	if err != nil {
		return fmt.Errorf("stat self-upgrade candidate: %w", err)
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("self-upgrade candidate must not be a symlink")
	}
	sourceFD, err := unix.Open(candidate.ExecutionPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open self-upgrade candidate: %w", err)
	}
	source := os.NewFile(uintptr(sourceFD), candidate.ExecutionPath)
	if source == nil {
		_ = unix.Close(sourceFD)
		return errors.New("open self-upgrade candidate file descriptor")
	}
	defer func() { _ = source.Close() }()
	opened, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat opened self-upgrade candidate: %w", err)
	}
	if !os.SameFile(sourceInfo, opened) {
		return errors.New("self-upgrade candidate changed while binding")
	}
	sourceEvidence, err := CaptureImageEvidenceFromOpenFile(
		source,
		candidate.ExecutionPath,
		candidate.EmbeddedVersion,
		ImageMethodPathnameObserved,
	)
	if err != nil {
		return err
	}
	if !SameDarwinStagedImageEvidence(sourceEvidence, candidate) {
		return errors.New("self-upgrade candidate changed while binding")
	}

	stageDir, err := os.MkdirTemp(
		filepath.Dir(candidate.ExecutionPath),
		selfUpgradeStagePrefix(candidate.ExecutionPath)+strconv.Itoa(os.Getpid())+"-",
	)
	if err != nil {
		return fmt.Errorf("create self-upgrade stage: %w", err)
	}
	stagePath := filepath.Join(stageDir, filepath.Base(candidate.ExecutionPath))
	removeStage := true
	defer func() {
		if removeStage {
			_ = os.Remove(stagePath)
			_ = os.Remove(stageDir)
		}
	}()
	if err := os.Link(candidate.ExecutionPath, stagePath); err != nil {
		return fmt.Errorf("stage self-upgrade candidate: %w", err)
	}
	stageFD, err := unix.Open(stagePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open self-upgrade stage: %w", err)
	}
	stage := os.NewFile(uintptr(stageFD), stagePath)
	if stage == nil {
		_ = unix.Close(stageFD)
		return errors.New("open self-upgrade stage file descriptor")
	}
	defer func() { _ = stage.Close() }()
	staged, err := CaptureImageEvidenceFromOpenFile(
		stage,
		stagePath,
		candidate.EmbeddedVersion,
		ImageMethodPathnameExecObserved,
	)
	if err != nil {
		return err
	}
	if !SameDarwinStagedImageEvidence(staged, candidate) {
		return errors.New("self-upgrade stage changed while binding")
	}
	if err := verifyDarwinCodeSignature(stagePath); err != nil {
		return err
	}
	// Darwin cannot exec an open file descriptor. The private 0700 stage
	// directory named with our PID is re-verified immediately before exec;
	// same-UID races in that window are outside AMQ's threat model, as for wake
	// self-upgrade.
	return selfUpgradeDarwinExec(stagePath, argv, env)
}
