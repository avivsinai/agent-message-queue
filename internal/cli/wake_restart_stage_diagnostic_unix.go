//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type wakeRestartStageDiagnostic struct {
	Path   string
	Status string
	Reason string
}

func diagnoseWakeRestartStage(
	root string,
	agent string,
	inspection wakeLockInspection,
) wakeRestartStageDiagnostic {
	if err := fsq.ValidateHandle(agent); err != nil {
		return wakeRestartStageDiagnostic{Status: "inspection-error", Reason: err.Error()}
	}
	agentDirPath := fsq.AgentBase(root, agent)
	if _, err := os.Lstat(agentDirPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return wakeRestartStageDiagnostic{}
		}
		return wakeRestartStageDiagnostic{Status: "inspection-error", Reason: err.Error()}
	}
	agentDir, err := openWakeDirectory(agentDirPath, "wake agent directory")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return wakeRestartStageDiagnostic{}
		}
		return wakeRestartStageDiagnostic{Status: "inspection-error", Reason: err.Error()}
	}
	defer func() { _ = agentDir.Close() }()

	var snapshot wakeRestartRecordSnapshot
	var exists bool
	err = agentDir.withFD(func(dirfd int) error {
		var readErr error
		snapshot, exists, readErr = readWakeRestartRecordSnapshotAt(dirfd, agentDir)
		return readErr
	})
	if err != nil {
		return wakeRestartStageDiagnostic{
			Status: "record-invalid",
			Reason: fmt.Sprintf("wake restart record is not safely readable: %v", err),
		}
	}
	if exists && snapshot.Record.PreviousBoundImage != nil {
		previous := *snapshot.Record.PreviousBoundImage
		if wakeRecordedPathParentGone(previous.ExecutionPath) {
			return wakeRestartStageDiagnostic{
				Path:   previous.ExecutionPath,
				Status: wakeRestartStageBinaryDirGone,
				Reason: wakeBinaryDirGoneMessage,
			}
		}
		if _, stageErr := os.Lstat(previous.ExecutionPath); stageErr != nil && !os.IsNotExist(stageErr) {
			return wakeRestartStageDiagnostic{
				Path:   previous.ExecutionPath,
				Status: "inspection-error",
				Reason: stageErr.Error(),
			}
		} else if stageErr == nil {
			live := inspection.Exists && inspection.Status == wakeLockValid &&
				inspection.IdentityConfirmed && inspection.Process.Running
			previousIsLivePredecessor := snapshot.Record.Status == wakeRestartPending && live &&
				snapshot.Record.Schema == wakeRestartSchemaV1 &&
				snapshot.Record.Generation == inspection.Lock.Generation &&
				inspection.Lock.RunningImageEvidence != nil &&
				sameDarwinStagedWakeImageEvidence(previous, *inspection.Lock.RunningImageEvidence)
			if !previousIsLivePredecessor {
				status := "orphan"
				reason := "previous Darwin restart stage is retained by a non-live handoff"
				if snapshot.Record.Status == wakeRestartRefused {
					status = "cleanup-failed"
					reason = "refused wake restart retained its previous Darwin stage for cleanup"
				} else if snapshot.Record.Status == wakeRestartPending && live &&
					snapshot.Record.Schema == wakeRestartSchemaV2 &&
					snapshot.Record.SuccessorGeneration == inspection.Lock.Generation {
					status = "cleanup-pending"
					reason = "previous Darwin restart stage is retained until successor readiness cleanup"
				}
				return wakeRestartStageDiagnostic{
					Path:   previous.ExecutionPath,
					Status: status,
					Reason: reason,
				}
			}
		}
	}
	if exists && snapshot.Record.StagePath != "" {
		if wakeRestartRecordBinaryDirGone(snapshot.Record) {
			return wakeRestartStageDiagnostic{
				Path:   snapshot.Record.StagePath,
				Status: wakeRestartStageBinaryDirGone,
				Reason: wakeBinaryDirGoneMessage,
			}
		}
		present, stageErr := wakeRestartStageExistsPlatform(snapshot.Record)
		if stageErr != nil {
			if errors.Is(stageErr, os.ErrNotExist) {
				return wakeRestartStageDiagnostic{}
			}
			return wakeRestartStageDiagnostic{
				Path:   snapshot.Record.StagePath,
				Status: "inspection-error",
				Reason: stageErr.Error(),
			}
		}
		if present {
			status := "orphan"
			reason := "persisted Darwin restart stage is not bound to a live handoff"
			if snapshot.Record.Status == wakeRestartRefused {
				status = "cleanup-failed"
				reason = "refused wake restart retained its Darwin stage for cleanup"
			} else if snapshot.Record.Status == wakeRestartPending &&
				inspection.Exists && inspection.Status == wakeLockValid &&
				inspection.IdentityConfirmed && inspection.Process.Running {
				switch {
				case snapshot.Record.Schema == wakeRestartSchemaV1 &&
					snapshot.Record.Generation == inspection.Lock.Generation:
					status = "pending"
					reason = "restart stage is owned by the live predecessor generation"
				case snapshot.Record.Schema == wakeRestartSchemaV2 &&
					snapshot.Record.SuccessorGeneration == inspection.Lock.Generation:
					status = "handoff"
					reason = "restart stage is owned by the live successor handoff"
				}
			}
			return wakeRestartStageDiagnostic{
				Path:   snapshot.Record.StagePath,
				Status: status,
				Reason: reason,
			}
		}
	}

	running := inspection.Lock.RunningImageEvidence
	if running == nil || !validPreviousWakeRestartBoundImagePlatform(*running) {
		return wakeRestartStageDiagnostic{}
	}
	if wakeRecordedPathParentGone(running.ExecutionPath) {
		return wakeRestartStageDiagnostic{
			Path:   running.ExecutionPath,
			Status: wakeRestartStageBinaryDirGone,
			Reason: wakeBinaryDirGoneMessage,
		}
	}
	if _, err := os.Lstat(running.ExecutionPath); os.IsNotExist(err) {
		return wakeRestartStageDiagnostic{}
	} else if err != nil {
		return wakeRestartStageDiagnostic{
			Path:   running.ExecutionPath,
			Status: "inspection-error",
			Reason: err.Error(),
		}
	}
	if inspection.Status == wakeLockValid && inspection.IdentityConfirmed && inspection.Process.Running {
		return wakeRestartStageDiagnostic{
			Path:   running.ExecutionPath,
			Status: "active",
			Reason: "restart stage is the live wake image",
		}
	}
	return wakeRestartStageDiagnostic{
		Path:   running.ExecutionPath,
		Status: "orphan",
		Reason: "restart stage is retained by a non-live wake generation",
	}
}

func fixWakeRestartResidueWithoutLock(root, agent string) error {
	if err := fsq.ValidateHandle(agent); err != nil {
		return err
	}
	agentDir, err := openWakeDirectory(fsq.AgentBase(root, agent), "wake agent directory")
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()

	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		if current := inspectWakeLockAt(dirfd, agentDir, root, agent); current.Exists {
			return fmt.Errorf("wake lock appeared before restart residue fix; preserving restart state")
		}
		var diagnosticErr error
		if err := removeWakeSelfUpgradeArtifactsAt(dirfd); err != nil {
			diagnosticErr = newWakeLockResidueError(
				wakeLockResidueSelfUpgradeDiagnostic,
				fmt.Errorf("remove wake self-upgrade metadata after confirming no lock: %w", err),
			)
		}

		snapshot, exists, readErr := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
		if readErr != nil {
			if !exists || snapshot.Object.FileInfo == nil {
				return errors.Join(diagnosticErr, fmt.Errorf("inspect wake restart record before fix: %w", readErr))
			}
		} else if !exists {
			return diagnosticErr
		} else if err := reclaimWakeRestartStagePlatform(snapshot.Record); err != nil && !preserveChangedWakeRestartStage(err) {
			return errors.Join(diagnosticErr, fmt.Errorf("reclaim wake restart stage before quarantine: %w", err))
		}

		if _, err := quarantineWakeRestartRecordAt(dirfd, agentDir, snapshot); err != nil {
			return errors.Join(diagnosticErr, fmt.Errorf("quarantine wake restart record: %w", err))
		}
		return diagnosticErr
	})
}
