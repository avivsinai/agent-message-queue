//go:build darwin || linux

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
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

func bindWakeRestartCandidateForRecord(record wakeRestartRecord) (*wakeRestartBoundImage, error) {
	if err := validateWakeRestartRecord(record); err != nil {
		return nil, err
	}
	return bindWakeRestartCandidateForRecordPlatform(record)
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
	out, err := selfupgrade.RunBoundedProbe(ctx, path, preflightArgs, selfupgrade.BoundedProbeOptions{
		ExtraFiles: extraFiles,
		Env: setEnvVar(
			unsetEnvVar(unsetEnvVar(os.Environ(), envWakeResumeBootstrap), envWakeResumePreflight),
			envWakeResumePreflight,
			encodedBootstrap,
		),
	})
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

// probeBoundWakeSelfUpgradeVersion is a bounded post-bind preflight of the
// exact image already verified for wake. It confirms the tentative version
// before exec; it does not discover a version from an untrusted candidate.
func probeBoundWakeSelfUpgradeVersion(
	image *wakeRestartBoundImage,
	record wakeRestartRecord,
	incumbentVersion string,
) error {
	if image == nil || image.file == nil {
		return fmt.Errorf("bound wake restart image is missing")
	}
	path, extraFiles, err := boundWakeRestartPreflightCommandPlatform(image)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wakeResumePreflightTimeout)
	defer cancel()
	out, err := selfupgrade.RunBoundedProbe(
		ctx,
		path,
		[]string{"--no-update-check", "--version"},
		selfupgrade.BoundedProbeOptions{
			ExtraFiles: extraFiles,
			Env:        unsetEnvVar(unsetEnvVar(os.Environ(), envWakeResumeBootstrap), envWakeResumePreflight),
		},
	)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("bound wake self-upgrade version probe timed out: %w", ctx.Err())
		}
		return fmt.Errorf("bound wake self-upgrade version probe failed: %w", err)
	}
	version := strings.TrimSpace(string(out))
	if version == "" || version != record.Candidate.EmbeddedVersion {
		return fmt.Errorf(
			"bound candidate version %q does not match tentative version %q",
			version,
			record.Candidate.EmbeddedVersion,
		)
	}
	if err := revalidateBoundWakeRestartImagePlatform(image); err != nil {
		return err
	}
	if !wakeSelfUpgradeVersionStrictlyNewer(incumbentVersion, version) {
		return fmt.Errorf(
			"bound candidate version %q is not strictly newer than incumbent %q",
			version,
			incumbentVersion,
		)
	}
	return nil
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
	return selfupgrade.SameImageEvidenceExceptMethodPath(first, second)
}

// Link and unlink operations on any name of a Darwin hardlink mutate the
// shared inode ctime. A ctime-only difference on the same inode is not an image
// change when SHA256 agrees: the digest is the content proof. An in-place
// rewrite is not guaranteed to change mtime or size. Method and ExecutionPath
// describe how and where the image was captured, not what it is, so when the
// stable identity fields agree they do not affect identity. This lets two
// wakes staging the same brew candidate on nearby ticks tolerate the ctime
// mutation each other's hardlink imposes.
//
// Path-sensitive callers do not rely on this helper to detect a different name:
// revalidate uses SameFile(held fd, Lstat(executionPath)); verify compares
// os.Executable() to bound.ExecutionPath before calling this; persisted-bound
// validation requires BoundImage.ExecutionPath == StagePath; cleanup deletes
// bound.ExecutionPath after opening that path. Device/inode/size/sha256/version
// is sufficient authority here.
func sameDarwinStagedWakeImageEvidence(first, second wakeImageEvidenceV1) bool {
	return selfupgrade.SameDarwinStagedImageEvidence(first, second)
}

// A Darwin hardlink changes the shared inode's ctime. Cross-device staging
// instead copies and fsyncs the image, so the bound evidence has the stage's
// own device, inode, and ctime. Both paths require exact content and version.
func sameRequestedAndBoundWakeImageEvidence(first, second wakeImageEvidenceV1) bool {
	if first.Platform != second.Platform || first.Method != wakeImageMethodPathnameObserved {
		return false
	}
	switch first.Platform {
	case "darwin":
		if !wakeImageMethodIsDarwinExecObserved(second.Method) {
			return false
		}
		first.Device = second.Device
		first.Inode = second.Inode
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
