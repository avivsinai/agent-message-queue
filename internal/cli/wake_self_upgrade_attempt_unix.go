//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
	"golang.org/x/sys/unix"
)

const (
	wakeSelfUpgradeAttemptFileName = ".wake.selfupgrade.attempt"
	wakeSelfUpgradeAttemptSchemaV1 = 1
)

type wakeSelfUpgradeAttemptFile struct {
	Schema    int                          `json:"schema"`
	Status    string                       `json:"status"`
	Candidate selfupgrade.RefusedCandidate `json:"candidate"`
	UnixTime  int64                        `json:"unix_time"`
}

func wakeSelfUpgradeAttemptFileFromAttempt(attempt selfupgrade.Attempt) wakeSelfUpgradeAttemptFile {
	return wakeSelfUpgradeAttemptFile{
		Schema:    wakeSelfUpgradeAttemptSchemaV1,
		Status:    attempt.Status,
		Candidate: attempt.Candidate,
		UnixTime:  attempt.UnixTime,
	}
}

func (file wakeSelfUpgradeAttemptFile) attempt() selfupgrade.Attempt {
	return selfupgrade.Attempt{
		Status:    file.Status,
		Candidate: file.Candidate,
		UnixTime:  file.UnixTime,
	}
}

func readWakeSelfUpgradeAttemptAt(
	dirfd int,
	agentDir *wakeAgentDir,
) (selfupgrade.Attempt, bool, error) {
	if agentDir == nil {
		return selfupgrade.Attempt{}, false, errors.New("wake self-upgrade attempt agent directory is missing")
	}
	path := filepath.Join(agentDir.path, wakeSelfUpgradeAttemptFileName)
	raw, _, exists, err := readWakeRepairMetadataAt(
		dirfd,
		wakeSelfUpgradeAttemptFileName,
		"wake self-upgrade attempt",
		path,
		maxWakeMetadataFileBytes,
	)
	if err != nil || !exists {
		return selfupgrade.Attempt{}, exists, err
	}
	var file wakeSelfUpgradeAttemptFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return selfupgrade.Attempt{}, true, fmt.Errorf("parse wake self-upgrade attempt: %w", err)
	}
	if file.Schema != wakeSelfUpgradeAttemptSchemaV1 {
		return selfupgrade.Attempt{}, true, fmt.Errorf("wake self-upgrade attempt schema %d is unsupported", file.Schema)
	}
	attempt := file.attempt()
	if err := selfupgrade.ValidateAttempt(attempt); err != nil {
		return selfupgrade.Attempt{}, true, err
	}
	return attempt, true, nil
}

func writeWakeSelfUpgradeAttemptAt(
	dirfd int,
	agentDir *wakeAgentDir,
	attempt selfupgrade.Attempt,
) error {
	if err := selfupgrade.ValidateAttempt(attempt); err != nil {
		return err
	}
	data, err := json.MarshalIndent(wakeSelfUpgradeAttemptFileFromAttempt(attempt), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal wake self-upgrade attempt: %w", err)
	}
	return writeWakeRepairMetadataAt(
		dirfd,
		agentDir,
		wakeSelfUpgradeAttemptFileName,
		"wake self-upgrade attempt",
		append(data, '\n'),
		maxWakeMetadataFileBytes,
	)
}

func removeWakeSelfUpgradeAttemptAt(dirfd int) error {
	err := unix.Unlinkat(dirfd, wakeSelfUpgradeAttemptFileName, 0)
	if err == unix.ENOENT {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove wake self-upgrade attempt: %w", err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync wake self-upgrade attempt removal: %w", err)
	}
	return nil
}

func removeWakeSelfUpgradeArtifactsAt(dirfd int) error {
	return errors.Join(
		removeWakeSelfUpgradeDiagnosticAt(dirfd),
		removeWakeSelfUpgradeAttemptAt(dirfd),
	)
}

func removeWakeSelfUpgradeArtifactsGuarded(root, agent string) error {
	agentDir, err := openWakeAgentDir(root, agent)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return agentDir.withFD(func(dirfd int) error {
		return removeWakeSelfUpgradeArtifactsAt(dirfd)
	})
}

func loadWakeSelfUpgradeAttemptAtStartup(
	state *wakeSelfUpgradeState,
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	running wakeImageEvidenceV1,
) error {
	if state == nil || agentDir == nil || !state.Enabled || !state.Eligible {
		return nil
	}
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
		if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
			return fmt.Errorf("wake changed before self-upgrade attempt inspection")
		}
		attempt, exists, err := readWakeSelfUpgradeAttemptAt(dirfd, agentDir)
		if err != nil || !exists {
			return err
		}
		state.attempt = &attempt
		if attempt.Status == selfupgrade.AttemptStatusAttempt &&
			attempt.IsFresh(wakeSelfUpgradeNow()) && attempt.Matches(running) {
			state.startupRefusalReason = fmt.Sprintf(
				"refused unsettled wake self-upgrade image for 24h after a replacement attempt (candidate=%s)",
				wakeSelfUpgradeEvidenceIdentityString(running),
			)
		}
		return nil
	})
}

func persistWakeSelfUpgradeAttemptAtBoundary(
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	record wakeRestartRecord,
) (selfupgrade.Attempt, error) {
	if record.Source != wakeRestartSourceSelf {
		return selfupgrade.Attempt{}, errors.New("wake self-upgrade attempt requires a self restart record")
	}
	attempt := selfupgrade.NewAttempt(record.Candidate, wakeSelfUpgradeNow())
	if err := selfupgrade.ValidateAttempt(attempt); err != nil {
		return selfupgrade.Attempt{}, err
	}
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
		if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
			return fmt.Errorf("wake changed before self-upgrade attempt publication")
		}
		currentRecord, exists, err := readWakeRestartRecordAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists || !sameWakeRestartRecord(record, currentRecord) {
			return fmt.Errorf("wake restart request changed before self-upgrade attempt publication")
		}
		if err := writeWakeSelfUpgradeAttemptAt(dirfd, agentDir, attempt); err != nil {
			return err
		}
		installed, installedExists, err := readWakeSelfUpgradeAttemptAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !installedExists || installed != attempt {
			return fmt.Errorf("wake self-upgrade attempt changed after publication")
		}
		return nil
	})
	if err != nil {
		return selfupgrade.Attempt{}, err
	}
	return attempt, nil
}

func settleWakeSelfUpgradeAttemptAtBoundary(
	state *wakeSelfUpgradeState,
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	running wakeImageEvidenceV1,
) error {
	if state == nil || agentDir == nil || !state.Enabled || !state.Eligible {
		return nil
	}
	var settled *selfupgrade.Attempt
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
		if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
			return fmt.Errorf("wake changed before self-upgrade attempt settlement")
		}
		attempt, exists, err := readWakeSelfUpgradeAttemptAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if attempt.Status == selfupgrade.AttemptStatusAttempt {
			if !attempt.Matches(running) {
				settled = &attempt
				return nil
			}
			attempt.Status = selfupgrade.AttemptStatusSettled
			if err := writeWakeSelfUpgradeAttemptAt(dirfd, agentDir, attempt); err != nil {
				return err
			}
			installed, installedExists, err := readWakeSelfUpgradeAttemptAt(dirfd, agentDir)
			if err != nil {
				return err
			}
			if !installedExists || installed != attempt {
				return fmt.Errorf("settled wake self-upgrade attempt changed after publication")
			}
		}
		settled = &attempt
		return nil
	})
	if err != nil {
		return err
	}
	if settled != nil {
		state.attempt = settled
	}
	return nil
}
