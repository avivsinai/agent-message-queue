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
	wakeSelfUpgradeAttemptSchema   = 2
)

var (
	errWakeSelfUpgradeAttemptTimestampUncertain = errors.New("wake self-upgrade attempt timestamp is uncertain")
	errWakeSelfUpgradeAttemptNotFresh           = errors.New("wake self-upgrade attempt is not fresh")
)

type wakeSelfUpgradeAttemptRefusalError struct {
	attempt selfupgrade.Attempt
}

func (err wakeSelfUpgradeAttemptRefusalError) Error() string {
	return err.attempt.RefusalReason()
}

type wakeSelfUpgradeAttemptFile struct {
	Schema    int                           `json:"schema"`
	Attempts  []selfupgrade.Attempt         `json:"attempts,omitempty"`
	Status    string                        `json:"status,omitempty"`
	Candidate *selfupgrade.RefusedCandidate `json:"candidate,omitempty"`
	UnixTime  int64                         `json:"unix_time,omitempty"`
}

func wakeSelfUpgradeAttemptFileFromAttempts(attempts []selfupgrade.Attempt) wakeSelfUpgradeAttemptFile {
	return wakeSelfUpgradeAttemptFile{
		Schema:   wakeSelfUpgradeAttemptSchema,
		Attempts: append([]selfupgrade.Attempt(nil), attempts...),
	}
}

func (file wakeSelfUpgradeAttemptFile) attempts() []selfupgrade.Attempt {
	if file.Schema == wakeSelfUpgradeAttemptSchemaV1 {
		if file.Status == "" || file.Candidate == nil {
			return nil
		}
		return []selfupgrade.Attempt{{
			Status:    file.Status,
			Candidate: *file.Candidate,
			UnixTime:  file.UnixTime,
		}}
	}
	return append([]selfupgrade.Attempt(nil), file.Attempts...)
}

func readWakeSelfUpgradeAttemptAt(
	dirfd int,
	agentDir *wakeAgentDir,
) ([]selfupgrade.Attempt, bool, error) {
	if agentDir == nil {
		return nil, false, errors.New("wake self-upgrade attempt agent directory is missing")
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
		return nil, exists, err
	}
	var file wakeSelfUpgradeAttemptFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, true, fmt.Errorf("parse wake self-upgrade attempt: %w", err)
	}
	if file.Schema != wakeSelfUpgradeAttemptSchemaV1 && file.Schema != wakeSelfUpgradeAttemptSchema {
		return nil, true, fmt.Errorf("wake self-upgrade attempt schema %d is unsupported", file.Schema)
	}
	if file.Schema == wakeSelfUpgradeAttemptSchemaV1 && len(file.Attempts) != 0 {
		return nil, true, errors.New("wake self-upgrade attempt schema 1 contains a ledger")
	}
	attempts := file.attempts()
	if file.Schema == wakeSelfUpgradeAttemptSchemaV1 && len(attempts) == 0 {
		return nil, true, errors.New("wake self-upgrade attempt schema 1 is missing an attempt")
	}
	if file.Schema == wakeSelfUpgradeAttemptSchema &&
		(file.Status != "" || file.Candidate != nil || file.UnixTime != 0) {
		return nil, true, errors.New("wake self-upgrade attempt schema 2 contains legacy fields")
	}
	if err := selfupgrade.ValidateAttempts(attempts); err != nil {
		return nil, true, err
	}
	return attempts, true, nil
}

func writeWakeSelfUpgradeAttemptAt(
	dirfd int,
	agentDir *wakeAgentDir,
	attempts []selfupgrade.Attempt,
) error {
	if err := selfupgrade.ValidateAttempts(attempts); err != nil {
		return err
	}
	data, err := json.MarshalIndent(wakeSelfUpgradeAttemptFileFromAttempts(attempts), "", "  ")
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
	err := wakeUnlinkAt(dirfd, wakeSelfUpgradeAttemptFileName, 0)
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
	return removeWakeSelfUpgradeDiagnosticAt(dirfd)
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
		attempts, exists, err := readWakeSelfUpgradeAttemptAt(dirfd, agentDir)
		if err != nil || !exists {
			return err
		}
		pruned := selfupgrade.PruneExpiredAttempts(attempts, wakeSelfUpgradeNow())
		if len(pruned) != len(attempts) {
			if err := writeWakeSelfUpgradeAttemptAt(dirfd, agentDir, pruned); err != nil {
				return err
			}
		}
		state.attempts = pruned
		for _, attempt := range pruned {
			if attempt.Status == selfupgrade.AttemptStatusAttempt && attempt.IsFutureUncertain(wakeSelfUpgradeNow()) {
				state.Eligible = false
				state.Reason = "self-upgrade unavailable: replacement attempt timestamp is uncertain"
				continue
			}
			if attempt.Status == selfupgrade.AttemptStatusAttempt &&
				attempt.IsFresh(wakeSelfUpgradeNow()) && attempt.Matches(running) {
				state.startupRefusalReason = fmt.Sprintf(
					"refused unsettled wake self-upgrade image while the attempt is fresh relative to its recorded timestamp under the current wall clock (candidate=%s)",
					wakeSelfUpgradeEvidenceIdentityString(running),
				)
				break
			}
		}
		return nil
	})
}

func persistWakeSelfUpgradeAttemptAtBoundary(
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	record wakeRestartRecord,
) ([]selfupgrade.Attempt, error) {
	if record.Source != wakeRestartSourceSelf {
		return nil, errors.New("wake self-upgrade attempt requires a self restart record")
	}
	var installedAttempts []selfupgrade.Attempt
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
		if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
			return fmt.Errorf("wake changed before self-upgrade attempt publication")
		}
		currentRecord, exists, err := readWakeRestartRecordAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists || record.Status != wakeRestartPending || currentRecord.Status != wakeRestartPending ||
			!sameWakeRestartAttemptIdentity(record, currentRecord) {
			return fmt.Errorf("wake restart request changed before self-upgrade attempt publication")
		}
		currentAttempts, exists, err := readWakeSelfUpgradeAttemptAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists {
			currentAttempts = nil
		}
		now := wakeSelfUpgradeNow()
		for _, existing := range currentAttempts {
			if existing.Status == selfupgrade.AttemptStatusAttempt && existing.IsFutureUncertain(now) {
				return fmt.Errorf("%w: refusing to modify the wake attempt ledger", errWakeSelfUpgradeAttemptTimestampUncertain)
			}
		}
		for _, existing := range currentAttempts {
			if !existing.Matches(record.Candidate) {
				continue
			}
			switch existing.Status {
			case selfupgrade.AttemptStatusSettled:
				installedAttempts = append([]selfupgrade.Attempt(nil), currentAttempts...)
				return nil
			case selfupgrade.AttemptStatusAttempt:
				if existing.IsFresh(now) {
					return wakeSelfUpgradeAttemptRefusalError{attempt: existing}
				}
			}
		}
		attempt := selfupgrade.NewAttempt(record.Candidate, now)
		if err := selfupgrade.ValidateAttempt(attempt); err != nil {
			return err
		}
		if !attempt.IsFresh(now) {
			return fmt.Errorf("%w: refusing to publish an expired attempt", errWakeSelfUpgradeAttemptNotFresh)
		}
		installedAttempts, err = selfupgrade.AddAttempt(currentAttempts, attempt, now)
		if err != nil {
			return err
		}
		if err := writeWakeSelfUpgradeAttemptAt(dirfd, agentDir, installedAttempts); err != nil {
			return err
		}
		installed, installedExists, err := readWakeSelfUpgradeAttemptAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !installedExists || !sameSelfUpgradeAttempts(installed, installedAttempts) {
			return fmt.Errorf("wake self-upgrade attempt changed after publication")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return installedAttempts, nil
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
	var settled []selfupgrade.Attempt
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
		if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
			return fmt.Errorf("wake changed before self-upgrade attempt settlement")
		}
		attempts, exists, err := readWakeSelfUpgradeAttemptAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		now := wakeSelfUpgradeNow()
		for _, attempt := range attempts {
			if attempt.Status == selfupgrade.AttemptStatusAttempt && attempt.IsFutureUncertain(now) {
				state.Eligible = false
				state.Reason = "self-upgrade unavailable: replacement attempt timestamp is uncertain"
				settled = append([]selfupgrade.Attempt(nil), attempts...)
				return nil
			}
		}
		originalAttempts := append([]selfupgrade.Attempt(nil), attempts...)
		attempts = selfupgrade.PruneExpiredAttempts(attempts, now)
		for index := range attempts {
			if attempts[index].Status == selfupgrade.AttemptStatusAttempt &&
				attempts[index].Matches(running) {
				attempts[index].Status = selfupgrade.AttemptStatusSettled
			}
		}
		if !sameSelfUpgradeAttempts(originalAttempts, attempts) {
			if err := writeWakeSelfUpgradeAttemptAt(dirfd, agentDir, attempts); err != nil {
				return err
			}
			installed, installedExists, err := readWakeSelfUpgradeAttemptAt(dirfd, agentDir)
			if err != nil {
				return err
			}
			if !installedExists || !sameSelfUpgradeAttempts(installed, attempts) {
				return fmt.Errorf("settled wake self-upgrade attempt changed after publication")
			}
		}
		settled = attempts
		return nil
	})
	if err != nil {
		return err
	}
	if settled != nil {
		state.attempts = settled
	}
	return nil
}

func sameSelfUpgradeAttempts(first, second []selfupgrade.Attempt) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}
