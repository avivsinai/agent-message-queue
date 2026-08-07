//go:build darwin || linux

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type wakeRestartBoundImage struct {
	file          *os.File
	executionPath string
	evidence      wakeImageEvidenceV1
}

func (image *wakeRestartBoundImage) close() error {
	if image == nil {
		return nil
	}
	var closeErr error
	if image.file != nil {
		closeErr = image.file.Close()
		image.file = nil
	}
	cleanupErr := cleanupBoundWakeRestartImagePlatform(*image)
	if closeErr != nil {
		return fmt.Errorf("close bound wake restart image: %w", closeErr)
	}
	return cleanupErr
}

func bindWakeRestartCandidate(candidate wakeImageEvidenceV1) (*wakeRestartBoundImage, error) {
	if err := validateWakeImageEvidence(candidate); err != nil {
		return nil, err
	}
	return bindWakeRestartCandidatePlatform(candidate)
}

func preflightBoundWakeRestartCandidate(
	image *wakeRestartBoundImage,
	argv []string,
	bootstrap wakeResumeBootstrap,
) error {
	if image == nil || image.file == nil {
		return fmt.Errorf("bound wake restart image is missing")
	}
	if len(argv) == 0 {
		return fmt.Errorf("wake restart argv is empty")
	}
	bound := image.evidence
	bootstrap.BoundImage = &bound
	encodedBootstrap, err := encodeWakeResumeBootstrap(bootstrap)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wakeResumePreflightTimeout)
	defer cancel()
	preflightArgs := append(append([]string(nil), argv[1:]...), "--"+wakeResumePreflightFlag)
	path, extraFiles, err := boundWakeRestartPreflightCommandPlatform(image)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, preflightArgs...)
	cmd.ExtraFiles = extraFiles
	cmd.Env = setEnvVar(
		unsetEnvVar(unsetEnvVar(os.Environ(), envWakeResumeBootstrap), envWakeResumePreflight),
		envWakeResumePreflight,
		encodedBootstrap,
	)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("wake resume preflight timed out: %w", ctx.Err())
		}
		return err
	}
	if strings.TrimSpace(string(out)) != wakeResumePreflightOK {
		return fmt.Errorf("wake restart candidate did not confirm exact wake argv and bootstrap")
	}
	return revalidateBoundWakeRestartImagePlatform(image)
}

func verifyWakeResumeBoundImage(bootstrap wakeResumeBootstrap) (wakeImageEvidenceV1, error) {
	if bootstrap.BoundImage == nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake resume bound image evidence is missing")
	}
	if err := validateWakeImageEvidence(*bootstrap.BoundImage); err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("wake resume bound image evidence is invalid: %w", err)
	}
	return verifyWakeResumeBoundImagePlatform(*bootstrap.BoundImage)
}

func cleanupWakeResumeBoundImage(bootstrap wakeResumeBootstrap) error {
	if bootstrap.BoundImage == nil {
		return nil
	}
	return cleanupWakeResumeBoundImagePlatform(*bootstrap.BoundImage)
}

func cleanupPreviousWakeResumeBoundImage(bootstrap wakeResumeBootstrap) error {
	if bootstrap.PreviousBoundImage == nil {
		return nil
	}
	return cleanupWakeResumeBoundImagePlatform(*bootstrap.PreviousBoundImage)
}

func sameWakeImageEvidenceExceptMethodPath(first, second wakeImageEvidenceV1) bool {
	first.Method = second.Method
	first.ExecutionPath = second.ExecutionPath
	return first == second
}

// Link and unlink operations on any name for a Darwin hardlink mutate the
// shared inode ctime. Once both values identify the same private staged path,
// ctime cannot distinguish a public-name swap from an image mutation; the
// retained device/inode plus size and digest remain the binding authority.
func sameDarwinStagedWakeImageEvidence(first, second wakeImageEvidenceV1) bool {
	if first.Platform != "darwin" || second.Platform != "darwin" ||
		first.Method != wakeImageMethodPathnameExecVerified ||
		second.Method != wakeImageMethodPathnameExecVerified ||
		first.ExecutionPath != second.ExecutionPath {
		return first == second
	}
	first.CTimeNS = second.CTimeNS
	return first == second
}

// A Darwin hardlink necessarily changes the shared inode's ctime. This is the
// only extra delta allowed between the requester's pre-link observation and
// the post-link bound image; every stable identity, content, and version field
// remains exact. Linux does not receive this exception.
func sameRequestedAndBoundWakeImageEvidence(first, second wakeImageEvidenceV1) bool {
	if first.Platform != second.Platform || first.Method != wakeImageMethodPathnameObserved {
		return false
	}
	switch first.Platform {
	case "darwin":
		if second.Method != wakeImageMethodPathnameExecVerified {
			return false
		}
		first.CTimeNS = second.CTimeNS
	case "linux":
		if second.Method != wakeImageMethodFDExec {
			return false
		}
	default:
		return false
	}
	return sameWakeImageEvidenceExceptMethodPath(first, second)
}
