//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

const (
	envWakeResumeBootstrap     = "AMQ_WAKE_RESUME_BOOTSTRAP"
	envWakeResumePreflight     = "AMQ_WAKE_RESUME_PREFLIGHT"
	wakeResumePreflightFlag    = "wake-resume-preflight"
	wakeResumePreflightOK      = "wake-resume-preflight-ok"
	wakeResumePreflightTimeout = 5 * time.Second
	wakeRestartFileName        = ".wake.restart"
	wakeRestartSchemaV1        = 1
	wakeRestartSchemaV2        = 2
	wakeRestartPending         = "pending"
	wakeRestartRefused         = "refused"
	wakeRestartWaitTimeout     = 15 * time.Second
)

type wakeRestartRecord struct {
	Schema              int                  `json:"schema"`
	RequestID           string               `json:"request_id"`
	Status              string               `json:"status"`
	Root                string               `json:"root"`
	Agent               string               `json:"agent"`
	Generation          string               `json:"generation"`
	SuccessorGeneration string               `json:"successor_generation,omitempty"`
	Owner               wakeOwner            `json:"owner"`
	Candidate           wakeImageEvidenceV1  `json:"candidate"`
	StagePath           string               `json:"stage_path,omitempty"`
	BoundImage          *wakeImageEvidenceV1 `json:"bound_image,omitempty"`
	PreviousBoundImage  *wakeImageEvidenceV1 `json:"previous_bound_image,omitempty"`
	Reason              string               `json:"reason,omitempty"`
}

type wakeResumeBootstrap struct {
	Schema             int                  `json:"schema"`
	RequestID          string               `json:"request_id"`
	Generation         string               `json:"generation"`
	BoundImage         *wakeImageEvidenceV1 `json:"bound_image,omitempty"`
	PreviousBoundImage *wakeImageEvidenceV1 `json:"previous_bound_image,omitempty"`
}

type wakeRestartResult struct {
	Status             string `json:"status"`
	Agent              string `json:"agent"`
	Root               string `json:"root"`
	PID                int    `json:"pid,omitempty"`
	PreviousGeneration string `json:"previous_generation,omitempty"`
	Generation         string `json:"generation,omitempty"`
	Reason             string `json:"reason,omitempty"`
}

type wakeRestartReadiness struct {
	Prepared     bool
	Record       wakeRestartRecord
	RecordExists bool
}

var (
	errWakeRestartSchemaTooNew = errors.New("wake restart schema is newer than supported")

	wakeRestartNow            = time.Now
	wakeRestartSleep          = time.Sleep
	wakeRestartNotify         = notifyWakeRestartPlatform
	wakeRestartExec           = syscall.Exec
	wakeRestartPreflight      = preflightWakeRestartCandidate
	wakeRestartBind           = bindWakeRestartCandidateForRecord
	wakeRestartBoundPreflight = preflightBoundWakeRestartCandidate
)

func runWakeRestart(args []string) error {
	fs := flag.NewFlagSet("wake restart", flag.ContinueOnError)
	common := addCommonFlags(fs)
	usage := usageWithFlags(
		fs,
		"amq wake restart --me <agent> [options]",
		"Ask a live owner-bound wake to replace itself without a caller TTY.",
		"",
		"The existing wake keeps its terminal and PID, validates a fresh AMQ image,",
		"then execs that image only from a quiescent delivery boundary.",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	root := canonicalWakeRoot(resolveRoot(common.Root))
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	result, restartErr := requestWakeRestart(root, me)
	if common.JSON {
		if err := writeJSON(os.Stdout, result); err != nil {
			return err
		}
		return restartErr
	}
	line := fmt.Sprintf("wake restart: %s agent=%s root=%s", result.Status, result.Agent, result.Root)
	if result.PID > 0 {
		line += fmt.Sprintf(" pid=%d", result.PID)
	}
	if result.Generation != "" {
		line += " generation=" + result.Generation
	}
	if result.Reason != "" {
		line += " reason=" + result.Reason
	}
	if err := writeStdoutLine(line); err != nil {
		return err
	}
	return restartErr
}

func requestWakeRestart(root, me string) (result wakeRestartResult, returnErr error) {
	result = wakeRestartResult{Status: "refused", Agent: me, Root: root}
	defer func() {
		if returnErr != nil {
			result.Reason = wakeRestartReasonWithRemedy(result.Reason, root, me)
		}
	}()
	owner, err := wakeOwnerFromEnv()
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	if owner == nil {
		err := fmt.Errorf("wake restart requires the exact coop owner environment")
		result.Reason = err.Error()
		return result, err
	}
	candidate, err := captureCurrentWakeImageEvidence()
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	requestID, err := newWakeRestartRequestID()
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	stagePath, err := planWakeRestartStagePlatform(candidate, requestID)
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}

	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	defer func() { _ = agentDir.Close() }()

	var expected wakeLockInspection
	var creatorSnapshot wakeRestartRecordSnapshot
	adopted := false
	needsNotify := true
	record := wakeRestartRecord{
		Schema:    wakeRestartSchemaV1,
		RequestID: requestID,
		Status:    wakeRestartPending,
		Root:      root,
		Agent:     me,
		Owner:     *owner,
		Candidate: candidate,
		StagePath: stagePath,
	}
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		expected = inspectWakeLockAt(dirfd, agentDir, root, me)
		if err := validateWakeRestartIncumbent(expected, root, me, *owner); err != nil {
			return err
		}
		existing, exists, readErr := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
		if readErr != nil {
			if !exists || existing.Object.FileInfo == nil {
				return readErr
			}
			if errors.Is(readErr, errWakeRestartSchemaTooNew) {
				return fmt.Errorf(
					"future-schema wake restart request is preserved at %s; retry with a newer AMQ: %w",
					wakeRestartFileName,
					readErr,
				)
			}
			quarantined, quarantineErr := quarantineWakeRestartRecordAt(dirfd, agentDir, existing)
			if quarantineErr != nil {
				return errors.Join(readErr, quarantineErr)
			}
			return fmt.Errorf(
				"invalid wake restart request was preserved as %s; retry restart: %w",
				quarantined,
				readErr,
			)
		}
		if exists && existing.Record.Status == wakeRestartPending {
			disposition := classifyPendingWakeRestart(
				existing.Record,
				expected,
				root,
				me,
				*owner,
			)
			if disposition == wakeRestartPendingPreserve {
				return fmt.Errorf(
					"pending wake restart for predecessor generation %s is preserved because it does not match live generation %s",
					existing.Record.Generation,
					expected.Lock.Generation,
				)
			}
			if disposition == wakeRestartPendingClaimUnstable {
				return fmt.Errorf(
					"pending wake restart claim from generation %s to %s is preserved before successor publication",
					existing.Record.Generation,
					existing.Record.SuccessorGeneration,
				)
			}
			record = existing.Record
			requestID = record.RequestID
			candidate = record.Candidate
			adopted = true
			needsNotify = disposition == wakeRestartPendingAdoptNotify
		}
		if exists && existing.Record.Status == wakeRestartRefused {
			if err := reclaimWakeRestartStagePlatform(existing.Record); err != nil {
				return fmt.Errorf("reclaim refused wake restart stage before retry: %w", err)
			}
			if _, err := quarantineWakeRestartRecordAt(dirfd, agentDir, existing); err != nil {
				return fmt.Errorf("quarantine refused wake restart request before retry: %w", err)
			}
		}
		if needsNotify {
			if err := validateWakeRestartArgv(expected.Lock.Args, root, me); err != nil {
				return err
			}
		}
		if !adopted {
			record.Generation = expected.Lock.Generation
			record.PreviousBoundImage = previousDarwinWakeRestartStageForLock(expected.Lock)
			bootstrap := wakeResumeBootstrap{
				Schema:             wakeRestartSchemaV1,
				RequestID:          record.RequestID,
				Generation:         record.Generation,
				PreviousBoundImage: record.PreviousBoundImage,
			}
			if err := wakeRestartPreflight(candidate, expected.Lock.Args, bootstrap); err != nil {
				return fmt.Errorf("wake restart candidate preflight failed: %w", err)
			}
			if err := writeWakeRestartRecordAt(dirfd, agentDir, record); err != nil {
				return err
			}
			createdSnapshot, created, readErr := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
			if readErr != nil {
				return readErr
			}
			if !created || !sameWakeRestartRecord(createdSnapshot.Record, record) {
				return fmt.Errorf("wake restart request changed after creation")
			}
			creatorSnapshot = createdSnapshot
		}
		current := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
			return fmt.Errorf("wake changed while publishing restart request")
		}
		return nil
	})
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	result.PID = expected.PID
	result.PreviousGeneration = record.Generation

	if needsNotify {
		if err := wakeRestartNotify(agentDir, expected, record); err != nil {
			if !adopted {
				_ = refuseWakeRestartCreatorSnapshot(agentDir, creatorSnapshot, err.Error())
			}
			result.Reason = err.Error()
			return result, err
		}
	}

	deadline := wakeRestartNow().Add(wakeRestartWaitTimeout)
	for wakeRestartNow().Before(deadline) {
		current := inspectWakeLock(root, me)
		if current.Exists && current.Status == wakeLockValid && current.IdentityConfirmed &&
			current.PID == expected.PID && current.Lock.Generation != record.Generation {
			if current.Lock.RunningImageEvidence == nil ||
				!sameRequestedAndBoundWakeImageEvidence(candidate, *current.Lock.RunningImageEvidence) ||
				current.Lock.ImagePath != current.Lock.RunningImageEvidence.ExecutionPath ||
				current.Lock.ImageVersion != candidate.EmbeddedVersion {
				err := fmt.Errorf("restarted wake generation does not match the requested candidate")
				result.Reason = err.Error()
				return result, err
			}
			readiness, readinessErr := observeWakeRestartReadinessInDir(
				agentDir,
				root,
				me,
				current,
			)
			if readinessErr != nil {
				result.Reason = readinessErr.Error()
				return result, readinessErr
			}
			if readiness.Prepared && !readiness.RecordExists {
				result.Status = "restarted"
				result.Generation = current.Lock.Generation
				result.Reason = ""
				return result, nil
			}
		}
		var observed wakeRestartRecord
		var exists bool
		readErr := agentDir.withFD(func(dirfd int) error {
			var err error
			observed, exists, err = readWakeRestartRecordAt(dirfd, agentDir)
			return err
		})
		if readErr != nil {
			result.Reason = readErr.Error()
			return result, readErr
		}
		if exists && observed.RequestID == requestID && observed.Status == wakeRestartRefused {
			err := fmt.Errorf("wake restart refused: %s", observed.Reason)
			result.Reason = observed.Reason
			return result, err
		}
		if (!exists || observed.RequestID != requestID) &&
			(!current.Exists || current.Lock.Generation == record.Generation) {
			err := fmt.Errorf("wake restart request disappeared before generation change")
			result.Reason = err.Error()
			return result, err
		}
		wakeRestartSleep(25 * time.Millisecond)
	}
	err = fmt.Errorf("wake restart did not complete within %s", wakeRestartWaitTimeout)
	result.Reason = err.Error()
	return result, err
}

func newWakeRestartRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate wake restart request id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func wakeRestartCheckCommand(root, me string) string {
	return fmt.Sprintf(
		"amq wake check --root %s --me %s --json --json-schema=2",
		shellQuoteArg(root),
		shellQuoteArg(me),
	)
}

func wakeRestartReasonWithRemedy(reason, root, me string) string {
	reason = strings.TrimSpace(reason)
	remedy := wakeRestartCheckCommand(root, me)
	if strings.Contains(reason, remedy) {
		return reason
	}
	if reason == "" {
		return "inspect restart state with " + remedy
	}
	return reason + "; inspect restart state with " + remedy
}

func previousDarwinWakeRestartStage(evidence *wakeImageEvidenceV1) *wakeImageEvidenceV1 {
	if evidence == nil || !validPreviousWakeRestartBoundImagePlatform(*evidence) {
		return nil
	}
	value := *evidence
	return &value
}

func previousDarwinWakeRestartStageForLock(lock wakeLock) *wakeImageEvidenceV1 {
	if lock.RunningImageEvidence == nil ||
		lock.ImagePath != lock.RunningImageEvidence.ExecutionPath {
		return nil
	}
	return previousDarwinWakeRestartStage(lock.RunningImageEvidence)
}

func sameOptionalWakeImageEvidence(first, second *wakeImageEvidenceV1) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return *first == *second
}

func sameWakeRestartRecord(first, second wakeRestartRecord) bool {
	if !sameOptionalWakeImageEvidence(first.BoundImage, second.BoundImage) {
		return false
	}
	if !sameOptionalWakeImageEvidence(first.PreviousBoundImage, second.PreviousBoundImage) {
		return false
	}
	first.BoundImage = nil
	second.BoundImage = nil
	first.PreviousBoundImage = nil
	second.PreviousBoundImage = nil
	return first == second
}

type wakeRestartPendingDisposition uint8

const (
	wakeRestartPendingPreserve wakeRestartPendingDisposition = iota
	wakeRestartPendingAdoptNotify
	wakeRestartPendingJoinWait
	wakeRestartPendingClaimUnstable
)

func classifyPendingWakeRestart(
	record wakeRestartRecord,
	incumbent wakeLockInspection,
	root, me string,
	owner wakeOwner,
) wakeRestartPendingDisposition {
	if record.Status != wakeRestartPending || record.Root != root || record.Agent != me ||
		!sameWakeOwner(&record.Owner, &owner) {
		return wakeRestartPendingPreserve
	}
	switch record.Schema {
	case wakeRestartSchemaV1:
		if record.Generation == incumbent.Lock.Generation {
			return wakeRestartPendingAdoptNotify
		}
	case wakeRestartSchemaV2:
		if record.SuccessorGeneration == incumbent.Lock.Generation {
			return wakeRestartPendingJoinWait
		}
		if record.Generation == incumbent.Lock.Generation {
			return wakeRestartPendingClaimUnstable
		}
	}
	return wakeRestartPendingPreserve
}

func validateWakeRestartIncumbent(
	inspection wakeLockInspection,
	root, me string,
	owner wakeOwner,
) error {
	if !inspection.Exists || inspection.Status != wakeLockValid || !inspection.IdentityConfirmed ||
		!inspection.Process.Running {
		return fmt.Errorf("wake is not a live identity-confirmed process")
	}
	if inspection.Root != root || inspection.Agent != me {
		return fmt.Errorf("wake restart scope does not match")
	}
	if err := validateWakeResumeAdvertisement(inspection.Lock, root, me); err != nil {
		return fmt.Errorf("wake does not advertise restart support: %w", err)
	}
	if err := validateWakeRestartTransportPlatform(inspection.Lock, root, me); err != nil {
		return fmt.Errorf("wake does not advertise a safe restart transport: %w", err)
	}
	if inspection.Lock.ResumeOwner == nil || !sameWakeOwner(inspection.Lock.ResumeOwner, &owner) {
		return fmt.Errorf("wake restart caller is not the exact coop owner")
	}
	if err := wakeOwnerHealthCheck(owner); err != nil {
		return err
	}
	return nil
}

func validateWakeRestartArgv(argv []string, root, me string) error {
	if len(argv) < 2 || !processArgsLookLikeWake(argv) || !wakeArgsMatchRootAgent(argv, root, me) {
		return fmt.Errorf("wake restart cannot recover the exact original wake argv")
	}
	for _, arg := range argv {
		switch {
		case arg == "--repair-lineage" || strings.HasPrefix(arg, "--repair-lineage="),
			arg == "--inject-via" || strings.HasPrefix(arg, "--inject-via="),
			arg == "--inject-cmd" || strings.HasPrefix(arg, "--inject-cmd="),
			arg == "--"+wakeResumePreflightFlag || strings.HasPrefix(arg, "--"+wakeResumePreflightFlag+"="),
			arg == "--interrupt-cmd=ctrl-c":
			return fmt.Errorf("wake restart refuses non-ordinary wake argv")
		}
	}
	return nil
}

func preflightWakeRestartCandidate(
	candidate wakeImageEvidenceV1,
	argv []string,
	bootstrap wakeResumeBootstrap,
) error {
	before, err := captureWakeImageEvidence(candidate.ExecutionPath, candidate.EmbeddedVersion)
	if err != nil {
		return err
	}
	if !sameWakeImageEvidence(before, candidate) {
		return fmt.Errorf("wake restart candidate changed before preflight")
	}
	if len(argv) == 0 {
		return fmt.Errorf("wake restart argv is empty")
	}
	encodedBootstrap, err := encodeWakeResumeBootstrap(bootstrap)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wakeResumePreflightTimeout)
	defer cancel()
	preflightArgs := append(append([]string(nil), argv[1:]...), "--"+wakeResumePreflightFlag)
	cmd := exec.CommandContext(ctx, candidate.ExecutionPath, preflightArgs...)
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
	after, err := captureWakeImageEvidence(candidate.ExecutionPath, candidate.EmbeddedVersion)
	if err != nil {
		return err
	}
	if !sameWakeImageEvidence(after, candidate) {
		return fmt.Errorf("wake restart candidate changed during preflight")
	}
	return nil
}

func sameWakeImageEvidence(first, second wakeImageEvidenceV1) bool {
	return first == second
}

func validateWakeRestartRecord(record wakeRestartRecord) error {
	if record.Schema > wakeRestartSchemaV2 {
		return fmt.Errorf("%w: wake restart schema %d unsupported", errWakeRestartSchemaTooNew, record.Schema)
	}
	if record.Schema != wakeRestartSchemaV1 && record.Schema != wakeRestartSchemaV2 {
		return fmt.Errorf("wake restart schema %d unsupported", record.Schema)
	}
	if !validWakeReloadTransportGeneration(record.RequestID) ||
		!validWakeReloadTransportGeneration(record.Generation) {
		return fmt.Errorf("wake restart request or generation is malformed")
	}
	if record.Status != wakeRestartPending && record.Status != wakeRestartRefused {
		return fmt.Errorf("wake restart status is invalid")
	}
	if record.Status == wakeRestartPending && record.Reason != "" {
		return fmt.Errorf("pending wake restart contains a refusal reason")
	}
	if record.Status == wakeRestartRefused && strings.TrimSpace(record.Reason) == "" {
		return fmt.Errorf("refused wake restart has no reason")
	}
	switch record.Schema {
	case wakeRestartSchemaV1:
		if record.SuccessorGeneration != "" {
			return fmt.Errorf("schema-1 wake restart contains a successor generation")
		}
	case wakeRestartSchemaV2:
		if !validWakeReloadTransportGeneration(record.SuccessorGeneration) ||
			record.SuccessorGeneration == record.Generation {
			return fmt.Errorf("claimed wake restart successor generation is invalid")
		}
	}
	if record.Root == "" || !filepath.IsAbs(record.Root) || canonicalWakeRoot(record.Root) != record.Root {
		return fmt.Errorf("wake restart root is invalid")
	}
	if err := fsq.ValidateHandle(record.Agent); err != nil {
		return fmt.Errorf("wake restart agent is invalid: %w", err)
	}
	if err := validateAuthoritativeWakeOwner(record.Owner); err != nil {
		return fmt.Errorf("wake restart owner is invalid: %w", err)
	}
	if err := validateWakeImageEvidence(record.Candidate); err != nil {
		return fmt.Errorf("wake restart candidate is invalid: %w", err)
	}
	if err := validateWakeRestartStageStatePlatform(record); err != nil {
		return fmt.Errorf("wake restart stage state is invalid: %w", err)
	}
	if record.PreviousBoundImage != nil {
		if err := validateWakeImageEvidence(*record.PreviousBoundImage); err != nil {
			return fmt.Errorf("previous bound wake image is invalid: %w", err)
		}
		if previousDarwinWakeRestartStage(record.PreviousBoundImage) == nil {
			return fmt.Errorf("previous bound wake image is not a Darwin restart stage")
		}
	}
	return nil
}

func readWakeRestartRecordAt(
	dirfd int,
	agentDir *wakeAgentDir,
) (wakeRestartRecord, bool, error) {
	snapshot, exists, err := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
	return snapshot.Record, exists, err
}

type wakeRestartRecordSnapshot struct {
	Record wakeRestartRecord
	Object wakeQuarantineSnapshot
}

func readWakeRestartRecordSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
) (wakeRestartRecordSnapshot, bool, error) {
	object, exists, err := readWakeQuarantineSnapshotAt(
		dirfd,
		agentDir,
		wakeRestartFileName,
		"wake restart request",
	)
	snapshot := wakeRestartRecordSnapshot{Object: object}
	if err != nil || !exists {
		return snapshot, exists, err
	}
	var envelope struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(object.Raw, &envelope); err != nil {
		return snapshot, true, fmt.Errorf("parse wake restart request: %w", err)
	}
	if envelope.Schema > wakeRestartSchemaV2 {
		return snapshot, true, fmt.Errorf(
			"%w: wake restart schema %d unsupported",
			errWakeRestartSchemaTooNew,
			envelope.Schema,
		)
	}
	if err := json.Unmarshal(object.Raw, &snapshot.Record); err != nil {
		return snapshot, true, fmt.Errorf("parse wake restart request: %w", err)
	}
	if err := validateWakeRestartRecord(snapshot.Record); err != nil {
		return snapshot, true, err
	}
	return snapshot, true, nil
}

func observeWakeRestartReadinessInDir(
	agentDir *wakeAgentDir,
	root, me string,
	expected wakeLockInspection,
) (wakeRestartReadiness, error) {
	var readiness wakeRestartReadiness
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		var err error
		readiness, err = readWakeRestartReadinessAt(dirfd, agentDir, root, me, expected)
		return err
	})
	return readiness, err
}

func readWakeRestartReadinessAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root, me string,
	expected wakeLockInspection,
) (wakeRestartReadiness, error) {
	current := inspectWakeLockAt(dirfd, agentDir, root, me)
	if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
		return wakeRestartReadiness{}, fmt.Errorf("wake changed while observing restart readiness")
	}
	var readiness wakeRestartReadiness
	var err error
	readiness.Record, readiness.RecordExists, err = readWakeRestartRecordAt(dirfd, agentDir)
	if err != nil {
		return wakeRestartReadiness{}, err
	}
	readiness.Prepared, err = validateWakePreparedFileAgainstInspectionAt(
		dirfd,
		agentDir,
		root,
		me,
		current,
	)
	if err != nil {
		return wakeRestartReadiness{}, err
	}
	confirmedRecord, confirmedExists, err := readWakeRestartRecordAt(dirfd, agentDir)
	if err != nil {
		return wakeRestartReadiness{}, err
	}
	confirmed := inspectWakeLockAt(dirfd, agentDir, root, me)
	if !sameWakeLockInspection(expected, confirmed) || !confirmed.IdentityConfirmed ||
		readiness.RecordExists != confirmedExists ||
		!sameWakeRestartRecord(readiness.Record, confirmedRecord) {
		return wakeRestartReadiness{}, fmt.Errorf("wake restart readiness changed while observing it")
	}
	return readiness, nil
}

func observeWakeRestartReadiness(
	root, me string,
	expected wakeLockInspection,
) (wakeRestartReadiness, error) {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return wakeRestartReadiness{}, err
	}
	defer func() { _ = agentDir.Close() }()
	var readiness wakeRestartReadiness
	err = agentDir.withFD(func(dirfd int) error {
		var readErr error
		readiness, readErr = readWakeRestartReadinessAt(dirfd, agentDir, root, me, expected)
		return readErr
	})
	return readiness, err
}

func writeWakeRestartRecordAt(dirfd int, agentDir *wakeAgentDir, record wakeRestartRecord) error {
	if err := validateWakeRestartRecord(record); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeWakeRepairMetadataAt(
		dirfd,
		agentDir,
		wakeRestartFileName,
		"wake restart request",
		append(raw, '\n'),
		maxWakeMetadataFileBytes,
	)
}

func sameWakeRestartObjectSnapshot(first, second wakeRestartRecordSnapshot) bool {
	return first.Object.FileInfo != nil && second.Object.FileInfo != nil &&
		sameWakeFileIdentity(first.Object.FileInfo, second.Object.FileInfo) &&
		bytes.Equal(first.Object.Raw, second.Object.Raw) &&
		first.Record.RequestID == second.Record.RequestID
}

func claimWakeRestartSuccessorAt(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeRestartRecordSnapshot,
	successorGeneration string,
) (wakeRestartRecord, error) {
	if expected.Record.Schema != wakeRestartSchemaV1 ||
		expected.Record.Status != wakeRestartPending ||
		!validWakeReloadTransportGeneration(successorGeneration) ||
		successorGeneration == expected.Record.Generation {
		return wakeRestartRecord{}, fmt.Errorf("wake restart successor claim is invalid")
	}
	current, exists, err := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
	if err != nil {
		return wakeRestartRecord{}, err
	}
	if !exists || !sameWakeRestartObjectSnapshot(expected, current) ||
		!sameWakeRestartRecord(expected.Record, current.Record) {
		return wakeRestartRecord{}, newWakeSnapshotReadChangedError(
			fmt.Errorf("wake restart request changed before successor claim"),
		)
	}
	claimed := current.Record
	claimed.Schema = wakeRestartSchemaV2
	claimed.SuccessorGeneration = successorGeneration
	if err := writeWakeRestartRecordAt(dirfd, agentDir, claimed); err != nil {
		return wakeRestartRecord{}, err
	}
	installed, installedExists, err := readWakeRestartRecordAt(dirfd, agentDir)
	if err != nil {
		return wakeRestartRecord{}, err
	}
	if !installedExists || !sameWakeRestartRecord(claimed, installed) {
		return wakeRestartRecord{}, fmt.Errorf("wake restart successor claim was not installed")
	}
	return claimed, nil
}

func removeWakeRestartRecordSnapshotAt(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeRestartRecordSnapshot,
	description string,
) error {
	current, exists, err := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
	if err != nil {
		return err
	}
	if !exists || !sameWakeRestartObjectSnapshot(expected, current) ||
		!sameWakeRestartRecord(expected.Record, current.Record) {
		return newWakeSnapshotReadChangedError(
			fmt.Errorf("%s changed before removal", description),
		)
	}
	if err := unix.Unlinkat(dirfd, wakeRestartFileName, 0); err != nil {
		return fmt.Errorf("remove %s: %w", description, err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync %s removal: %w", description, err)
	}
	return nil
}

func refuseWakeRestartRecord(agentDir *wakeAgentDir, expected wakeRestartRecord, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "restart refused"
	}
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current, exists, err := readWakeRestartRecordAt(dirfd, agentDir)
		if err != nil || !exists {
			return err
		}
		if expected.Schema != wakeRestartSchemaV1 || expected.Status != wakeRestartPending ||
			current.Schema != wakeRestartSchemaV1 || current.Status != wakeRestartPending ||
			current.RequestID != expected.RequestID || current.Generation != expected.Generation {
			return fmt.Errorf("wake restart request changed before refusal")
		}
		current.Status = wakeRestartRefused
		current.Reason = wakeRestartReasonWithRemedy(reason, current.Root, current.Agent)
		return writeWakeRestartRecordAt(dirfd, agentDir, current)
	})
}

func refuseWakeRestartCreatorSnapshot(
	agentDir *wakeAgentDir,
	expected wakeRestartRecordSnapshot,
	reason string,
) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "restart refused"
	}
	if expected.Record.Schema != wakeRestartSchemaV1 ||
		expected.Record.Status != wakeRestartPending {
		return fmt.Errorf("created wake restart snapshot is not pending schema 1")
	}
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current, exists, err := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
		if err != nil || !exists {
			return err
		}
		if current.Record.Schema != wakeRestartSchemaV1 ||
			current.Record.Status != wakeRestartPending ||
			!sameWakeRestartObjectSnapshot(expected, current) ||
			!sameWakeRestartRecord(expected.Record, current.Record) {
			return fmt.Errorf("created wake restart request changed before refusal")
		}
		refused := current.Record
		refused.Status = wakeRestartRefused
		refused.Reason = wakeRestartReasonWithRemedy(reason, refused.Root, refused.Agent)
		return writeWakeRestartRecordAt(dirfd, agentDir, refused)
	})
}

func encodeWakeResumeBootstrap(value wakeResumeBootstrap) (string, error) {
	if value.Schema != wakeRestartSchemaV1 || !validWakeReloadTransportGeneration(value.RequestID) ||
		!validWakeReloadTransportGeneration(value.Generation) {
		return "", fmt.Errorf("wake resume bootstrap is invalid")
	}
	if value.BoundImage != nil {
		if err := validateWakeImageEvidence(*value.BoundImage); err != nil {
			return "", fmt.Errorf("wake resume bound image is invalid: %w", err)
		}
	}
	if value.PreviousBoundImage != nil {
		if err := validateWakeImageEvidence(*value.PreviousBoundImage); err != nil {
			return "", fmt.Errorf("wake resume previous bound image is invalid: %w", err)
		}
		if previousDarwinWakeRestartStage(value.PreviousBoundImage) == nil {
			return "", fmt.Errorf("wake resume previous bound image is not a Darwin restart stage")
		}
	}
	if value.BoundImage != nil && value.PreviousBoundImage != nil &&
		value.BoundImage.ExecutionPath == value.PreviousBoundImage.ExecutionPath {
		return "", fmt.Errorf("wake resume previous and replacement images use the same execution path")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func wakeResumeBootstrapFromEnv() (*wakeResumeBootstrap, error) {
	return wakeResumeBootstrapFromEnvName(envWakeResumeBootstrap)
}

func wakeResumePreflightFromEnv() (*wakeResumeBootstrap, error) {
	return wakeResumeBootstrapFromEnvName(envWakeResumePreflight)
}

func wakeResumeBootstrapFromEnvName(name string) (*wakeResumeBootstrap, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, nil
	}
	if err := os.Unsetenv(name); err != nil {
		return nil, err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode wake resume bootstrap: %w", err)
	}
	var value wakeResumeBootstrap
	if err := json.Unmarshal(decoded, &value); err != nil {
		return nil, fmt.Errorf("parse wake resume bootstrap: %w", err)
	}
	if _, err := encodeWakeResumeBootstrap(value); err != nil {
		return nil, err
	}
	return &value, nil
}

func acquireWakeLockAfterResumeInDir(
	agentDir *wakeAgentDir,
	root, me string,
	options wakeLockAcquireOptions,
	bootstrap wakeResumeBootstrap,
) (func(), error) {
	var created wakeLockInspection
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !current.Exists || current.Status != wakeLockValid || !current.IdentityConfirmed ||
			current.PID != os.Getpid() || current.Lock.Generation != bootstrap.Generation {
			return fmt.Errorf("wake resume incumbent identity changed")
		}
		if err := validateWakeResumeAdvertisement(current.Lock, root, me); err != nil {
			return err
		}
		recordSnapshot, exists, err := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		record := recordSnapshot.Record
		if !exists || record.Status != wakeRestartPending || record.RequestID != bootstrap.RequestID ||
			record.Generation != bootstrap.Generation || current.Lock.ResumeOwner == nil ||
			!sameWakeOwner(current.Lock.ResumeOwner, &record.Owner) {
			return fmt.Errorf("wake resume request does not match the incumbent")
		}
		if options.requestedOwner == nil || !sameWakeOwner(options.requestedOwner, &record.Owner) {
			return fmt.Errorf("wake resume owner environment changed")
		}
		if !sameOptionalWakeImageEvidence(record.PreviousBoundImage, bootstrap.PreviousBoundImage) ||
			!sameOptionalWakeImageEvidence(
				record.PreviousBoundImage,
				previousDarwinWakeRestartStageForLock(current.Lock),
			) {
			return fmt.Errorf("wake resume previous bound image does not match the incumbent")
		}
		if bootstrap.BoundImage == nil || options.resumeImageEvidence == nil ||
			!sameDarwinStagedWakeImageEvidence(*options.resumeImageEvidence, *bootstrap.BoundImage) ||
			!sameRequestedAndBoundWakeImageEvidence(record.Candidate, *bootstrap.BoundImage) {
			return fmt.Errorf("wake resume bound image does not match the requested candidate")
		}
		if err := validateWakeRestartPersistedBoundPlatform(record, *bootstrap.BoundImage); err != nil {
			return err
		}
		lock, err := newWakeLock(root, me, options)
		if err != nil {
			return err
		}
		if lock.PID != current.PID || lock.ProcessStart != current.Lock.ProcessStart ||
			compareWakeBootID(current.Lock.BootID, lockProcessInfo(lock)) != bootIDMatch {
			return fmt.Errorf("wake resume did not preserve process identity")
		}
		claimed, err := claimWakeRestartSuccessorAt(
			dirfd,
			agentDir,
			recordSnapshot,
			lock.Generation,
		)
		if err != nil {
			return err
		}
		if err := replaceWakeLockForResumeAt(dirfd, agentDir, current, lock); err != nil {
			return err
		}
		created = inspectWakeLockAt(dirfd, agentDir, root, me)
		if !created.Exists || created.Status != wakeLockValid || !created.IdentityConfirmed ||
			created.Lock.Generation != lock.Generation {
			return fmt.Errorf("wake resume generation was not installed")
		}
		installed, installedExists, err := readWakeRestartRecordAt(dirfd, agentDir)
		if err != nil || !installedExists || !sameWakeRestartRecord(claimed, installed) {
			return fmt.Errorf("wake resume successor claim changed during generation commit")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return func() {
		if err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
			return cleanupGenericWakeGenerationAt(dirfd, agentDir, root, me, created, options)
		}); err != nil {
			_ = writeStderr("amq wake: cleanup failed: %v\n", err)
		}
	}, nil
}

func consumeWakeRestartAfterPrepared(
	agentDir *wakeAgentDir,
	root, me string,
	current wakeLockInspection,
	bootstrap wakeResumeBootstrap,
) error {
	return withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		observed := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !sameWakeLockInspection(current, observed) || !observed.IdentityConfirmed ||
			observed.PID != os.Getpid() || observed.Lock.Generation == bootstrap.Generation {
			return fmt.Errorf("wake resume generation changed before readiness commit")
		}
		prepared, err := validateWakePreparedFileAgainstInspectionAt(
			dirfd,
			agentDir,
			root,
			me,
			observed,
		)
		if err != nil {
			return err
		}
		if !prepared {
			return fmt.Errorf("wake resume prepared proof is not current")
		}
		recordSnapshot, exists, err := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		record := recordSnapshot.Record
		if !exists || record.Status != wakeRestartPending ||
			record.Schema != wakeRestartSchemaV2 ||
			record.RequestID != bootstrap.RequestID || record.Generation != bootstrap.Generation ||
			record.SuccessorGeneration != observed.Lock.Generation {
			return fmt.Errorf("wake resume request changed before readiness commit")
		}
		return removeWakeRestartRecordSnapshotAt(
			dirfd,
			agentDir,
			recordSnapshot,
			"ready wake restart request",
		)
	})
}

func lockProcessInfo(lock wakeLock) wakeProcessInfo {
	return wakeProcessInfo{BootID: lock.BootID}
}

func replaceWakeLockForResumeAt(
	dirfd int,
	agentDir *wakeAgentDir,
	expected wakeLockInspection,
	replacement wakeLock,
) error {
	if replacement.Generation == "" || replacement.Generation == expected.Lock.Generation {
		return fmt.Errorf("wake resume replacement generation is invalid")
	}
	raw, err := json.Marshal(replacement)
	if err != nil {
		return err
	}
	temp, err := writeWakeOwnerTempAt(dirfd, "wake-resume-lock", raw, 0o600)
	if err != nil {
		return err
	}
	tempPresent := true
	defer func() {
		if tempPresent {
			_ = unix.Unlinkat(dirfd, temp, 0)
		}
	}()
	current := inspectWakeLockAt(dirfd, agentDir, expected.Root, expected.Agent)
	if !sameWakeLockInspection(expected, current) || !current.IdentityConfirmed {
		return fmt.Errorf("wake resume incumbent changed before generation commit")
	}
	if err := unix.Renameat(dirfd, temp, dirfd, ".wake.lock"); err != nil {
		return fmt.Errorf("commit wake resume generation: %w", err)
	}
	tempPresent = false
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync wake resume generation: %w", err)
	}
	installed := readWakeLockMetadataAt(dirfd, agentDir, expected.Root, expected.Agent)
	if !installed.Exists || installed.Lock.Generation != replacement.Generation ||
		!bytes.Equal(installed.raw, raw) {
		return fmt.Errorf("verify wake resume generation")
	}
	return nil
}

func persistWakeRestartBoundImage(
	record wakeRestartRecord,
	bound wakeImageEvidenceV1,
) (wakeRestartRecord, error) {
	if record.StagePath == "" {
		return record, nil
	}
	agentDir, err := openWakeAgentDir(record.Root, record.Agent)
	if err != nil {
		return wakeRestartRecord{}, err
	}
	defer func() { _ = agentDir.Close() }()
	var persisted wakeRestartRecord
	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current, exists, err := readWakeRestartRecordSnapshotAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists || !sameWakeRestartRecord(current.Record, record) {
			return newWakeSnapshotReadChangedError(
				fmt.Errorf("wake restart request changed before bound-stage publication"),
			)
		}
		persisted = current.Record
		persisted.BoundImage = &bound
		if err := writeWakeRestartRecordAt(dirfd, agentDir, persisted); err != nil {
			return err
		}
		installed, installedExists, err := readWakeRestartRecordAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !installedExists || !sameWakeRestartRecord(persisted, installed) {
			return fmt.Errorf("wake restart bound stage was not persisted")
		}
		return nil
	})
	return persisted, err
}

func executeWakeRestart(record wakeRestartRecord, argv []string, restartSignals chan os.Signal) (returnErr error) {
	bootstrapValue := wakeResumeBootstrap{
		Schema:             wakeRestartSchemaV1,
		RequestID:          record.RequestID,
		Generation:         record.Generation,
		PreviousBoundImage: record.PreviousBoundImage,
	}
	bound, err := wakeRestartBind(record)
	if err != nil {
		return fmt.Errorf("bind wake restart candidate: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, bound.close()) }()
	boundEvidence := bound.evidence
	record, err = persistWakeRestartBoundImage(record, boundEvidence)
	if err != nil {
		return fmt.Errorf("persist wake restart bound stage: %w", err)
	}
	bootstrapValue.BoundImage = &boundEvidence
	if err := wakeRestartBoundPreflight(bound, argv, bootstrapValue); err != nil {
		return err
	}
	bootstrap, err := encodeWakeResumeBootstrap(bootstrapValue)
	if err != nil {
		return err
	}
	env := setEnvVar(unsetEnvVar(os.Environ(), envWakeResumeBootstrap), envWakeResumeBootstrap, bootstrap)
	// An ignored disposition survives exec, unlike a caught disposition. The
	// bootstrap installs Notify before it rotates and advertises the successor
	// generation. If exec fails, restore delivery to the incumbent loop.
	signal.Ignore(syscall.SIGUSR1)
	err = wakeRestartExec(bound.executionPath, append([]string(nil), argv...), env)
	if restartSignals != nil {
		signal.Notify(restartSignals, syscall.SIGUSR1)
	}
	if err != nil {
		return fmt.Errorf("exec wake restart candidate: %w", err)
	}
	return fmt.Errorf("exec wake restart candidate returned without replacing the process")
}

func pendingWakeRestartForProcess(
	agentDir *wakeAgentDir,
	root, me string,
	expectedGeneration string,
	owner *wakeOwner,
) (wakeRestartRecord, bool, error) {
	root = canonicalWakeRoot(root)
	var record wakeRestartRecord
	var exists bool
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current := inspectWakeLockAt(dirfd, agentDir, root, me)
		if !current.Exists || current.Status != wakeLockValid || !current.IdentityConfirmed ||
			current.PID != os.Getpid() || current.Lock.Generation != expectedGeneration {
			return fmt.Errorf("wake restart incumbent changed")
		}
		var err error
		record, exists, err = readWakeRestartRecordAt(dirfd, agentDir)
		if err != nil || !exists {
			return err
		}
		if record.Status != wakeRestartPending || record.Root != root || record.Agent != me ||
			record.Generation != expectedGeneration || owner == nil || !sameWakeOwner(owner, &record.Owner) ||
			!sameOptionalWakeImageEvidence(
				record.PreviousBoundImage,
				previousDarwinWakeRestartStageForLock(current.Lock),
			) {
			return fmt.Errorf("wake restart request does not match this generation")
		}
		return nil
	})
	return record, exists, err
}

func classifyWakeRestartAtLoopBoundary(
	cfg *wakeConfig,
	watcher wakeAdmissionWatcher,
	terminalRetry, scanRetry bool,
) wakeResumeQuiescenceDecision {
	state := wakeResumeQuiescence{
		Lifecycle:             wakeResumeLifecycleAdmitted,
		Delivery:              cfg.inputDelivery,
		RecoveryRequired:      cfg.inputRecoveryRequired,
		RepairLineage:         cfg.baselineInherited,
		BaselineInherited:     cfg.baselineInherited,
		ArbitraryInjectCmd:    strings.TrimSpace(cfg.injectCmd) != "",
		DestructiveInterrupt:  cfg.interruptKey != "",
		TerminalSuffixRetry:   terminalRetry,
		ScanRetry:             scanRetry,
		WatcherArmed:          watcher != nil,
		ControlListenerReady:  true,
		OwnerObservation:      wakeResumeAuthorityExact,
		GenerationObservation: wakeResumeAuthorityExact,
		LockTargetObservation: wakeResumeAuthorityExact,
		CanonicalDirs:         true,
		FinalScan:             wakeResumeScanComplete,
		PendingDoorbell:       cfg.doorbell.pendingInput(),
	}
	if watcher == nil || pendingWakeWatcherError(watcher) != nil {
		state.WatcherError = true
	}
	if cfg.wakeOwner == nil || wakeOwnerHealthCheck(*cfg.wakeOwner) != nil {
		state.OwnerObservation = wakeResumeAuthorityLost
	}
	if cfg.inspectTerminalGeneration == nil {
		state.GenerationObservation = wakeResumeAuthorityInconclusive
	} else {
		current := cfg.inspectTerminalGeneration()
		if !current.Exists || current.Status != wakeLockValid || !current.IdentityConfirmed ||
			current.PID != os.Getpid() || current.Lock.Generation != cfg.terminalGeneration {
			state.GenerationObservation = wakeResumeAuthorityLost
		}
	}
	if cfg.beforeTerminalWrite != nil {
		if err := cfg.beforeTerminalWrite(); err != nil {
			state.TerminalIdentityObservation = wakeResumeAuthorityLost
		} else {
			state.TerminalIdentityObservation = wakeResumeAuthorityExact
		}
	} else if cfg.injectMode == wakeInjectModeNone {
		state.TerminalIdentityObservation = wakeResumeAuthorityExact
	} else {
		state.TerminalIdentityObservation = wakeResumeAuthorityInconclusive
	}
	if retained, ok := cfg.retainedAgent.(*wakeAgentDir); ok {
		if err := validateCanonicalWakeAgentDir(retained); err != nil {
			state.CanonicalDirs = false
		}
	}
	if retained, ok := cfg.retainedInbox.(*wakeInboxDir); ok {
		if err := retained.ValidateCanonical(); err != nil {
			state.CanonicalDirs = false
		}
	}
	if messages, err := snapshotWakeExistingMessagesForConfig(cfg); err != nil {
		state.FinalScan = wakeResumeScanTransientFailure
	} else {
		state.FinalScanMessages = len(messages)
	}
	return classifyWakeResumeQuiescence(state)
}

func handleWakeRestartAtLoopBoundary(
	cfg *wakeConfig,
	watcher wakeAdmissionWatcher,
	terminalRetry, scanRetry bool,
) {
	agentDir, ok := cfg.retainedAgent.(*wakeAgentDir)
	if !ok || agentDir == nil {
		return
	}
	record, exists, err := pendingWakeRestartForProcess(
		agentDir,
		canonicalWakeRoot(cfg.root),
		cfg.me,
		cfg.terminalGeneration,
		cfg.wakeOwner,
	)
	if err != nil || !exists {
		return
	}
	decision := classifyWakeRestartAtLoopBoundary(cfg, watcher, terminalRetry, scanRetry)
	if decision.Disposition != wakeResumeProceed {
		_ = refuseWakeRestartRecord(
			agentDir,
			record,
			wakeRestartReasonWithRemedy(decision.Reason, cfg.root, cfg.me),
		)
		return
	}
	if err := executeWakeRestart(record, os.Args, cfg.restartSignals); err != nil {
		_ = refuseWakeRestartRecord(
			agentDir,
			record,
			wakeRestartReasonWithRemedy(err.Error(), cfg.root, cfg.me),
		)
	}
}
