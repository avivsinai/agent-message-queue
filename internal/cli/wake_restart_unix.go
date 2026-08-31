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
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
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
	wakeRestartSourceForeign   = "foreign"
	wakeRestartSourceSelf      = "self"
	wakeRestartWaitTimeout     = 15 * time.Second
)

type wakeRestartRecord struct {
	Schema              int                               `json:"schema"`
	Source              string                            `json:"source,omitempty"`
	RequestID           string                            `json:"request_id"`
	Status              string                            `json:"status"`
	Root                string                            `json:"root"`
	Agent               string                            `json:"agent"`
	Generation          string                            `json:"generation"`
	SuccessorGeneration string                            `json:"successor_generation,omitempty"`
	Owner               wakeOwner                         `json:"owner"`
	Candidate           wakeImageEvidenceV1               `json:"candidate"`
	StagePath           string                            `json:"stage_path,omitempty"`
	BoundImage          *wakeImageEvidenceV1              `json:"bound_image,omitempty"`
	PreviousBoundImage  *wakeImageEvidenceV1              `json:"previous_bound_image,omitempty"`
	RefusedCandidates   []wakeSelfUpgradeRefusedCandidate `json:"refused_candidates,omitempty"`
	Reason              string                            `json:"reason,omitempty"`
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
	errWakeImageRefused        = errors.New("wake restart image refused")

	wakeRestartNow            = time.Now
	wakeRestartSleep          = time.Sleep
	wakeRestartNotify         = notifyWakeRestartPlatform
	wakeRestartExec           = syscall.Exec
	wakeRestartPreflight      = preflightWakeRestartCandidate
	wakeRestartBind           = bindWakeRestartCandidateForRecord
	wakeRestartBoundPreflight = preflightBoundWakeRestartCandidate
	wakeRestartIgnore         = signal.Ignore
	wakeRestartSignalNotify   = signal.Notify
	// Test seams for the readiness/record readers in the requestWakeRestart
	// poll loop. Production wires them to the real readers; tests swap them to
	// inject transient wakeSnapshotReadChangedError and refused records.
	wakeRestartObserveReadiness = observeWakeRestartReadinessInDir
	wakeRestartReadRecord       = readWakeRestartRecordAt
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
	record := wakeRestartRecord{
		Schema:    wakeRestartSchemaV1,
		RequestID: requestID,
		Status:    wakeRestartPending,
		Root:      root,
		Agent:     me,
		Owner:     *owner,
		Candidate: candidate,
	}
	stagePath, err := planWakeRestartStageForRecordPlatform(record)
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	record.StagePath = stagePath

	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	defer func() { _ = agentDir.Close() }()

	state := wakeRestartPublicationState{
		root:        root,
		me:          me,
		owner:       *owner,
		agentDir:    agentDir,
		record:      record,
		requestID:   requestID,
		candidate:   candidate,
		needsNotify: true,
	}
	// Candidate preflight execs a child and can wait up to
	// wakeResumePreflightTimeout. Hold the exclusive lifecycle guard only for
	// snapshot and later exact lock-record CAS; never across the child wait.
	var needPreflight bool
	err = withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		var recErr error
		needPreflight, recErr = reconcileWakeRestartPublicationAt(scope, &state, false)
		return recErr
	})
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	if needPreflight {
		bootstrap := wakeResumeBootstrap{
			Schema:             wakeRestartSchemaV1,
			RequestID:          state.record.RequestID,
			Generation:         state.record.Generation,
			PreviousBoundImage: state.record.PreviousBoundImage,
		}
		if err := wakeRestartPreflight(state.candidate, state.expected.Lock.Args, bootstrap); err != nil {
			err = fmt.Errorf("wake restart candidate preflight failed: %w", err)
			result.Reason = err.Error()
			return result, err
		}
		err = withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
			_, recErr := reconcileWakeRestartPublicationAt(scope, &state, true)
			return recErr
		})
		if err != nil {
			result.Reason = err.Error()
			return result, err
		}
	}
	expected := state.expected
	record = state.record
	requestID = state.requestID
	candidate = state.candidate
	needsNotify := state.needsNotify
	result.PID = expected.PID
	result.PreviousGeneration = record.Generation

	if needsNotify {
		if err := wakeRestartNotify(agentDir, expected, record); err != nil {
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
			readiness, readinessErr := wakeRestartObserveReadiness(
				agentDir,
				root,
				me,
				current,
			)
			if readinessErr != nil {
				// The owner rewrites the restart record by atomic rename during
				// handoff, so the snapshot identity check (Dev+Ino+Ctimespec on
				// Darwin) can transiently observe a different inode between the
				// reader's two opens. That is a retryable race in the read path,
				// not corruption: keep polling until the owner publishes a stable
				// restarted generation. The snapshot check itself stays strict.
				var changed *wakeSnapshotReadChangedError
				if errors.As(readinessErr, &changed) {
					result.Reason = readinessErr.Error()
					wakeRestartSleep(25 * time.Millisecond)
					continue
				}
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
			observed, exists, err = wakeRestartReadRecord(dirfd, agentDir)
			return err
		})
		if readErr != nil {
			// Same transient race as the readiness read above: the record file
			// is rewritten by rename during handoff and the double-open identity
			// check can observe a changed inode. Retry; refused and vanished
			// records still terminate below.
			var changed *wakeSnapshotReadChangedError
			if errors.As(readErr, &changed) {
				result.Reason = readErr.Error()
				wakeRestartSleep(25 * time.Millisecond)
				continue
			}
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

type wakeRestartPublicationState struct {
	root        string
	me          string
	owner       wakeOwner
	agentDir    *wakeAgentDir
	record      wakeRestartRecord
	requestID   string
	candidate   wakeImageEvidenceV1
	expected    wakeLockInspection
	adopted     bool
	needsNotify bool
}

func reconcileWakeRestartPublicationAt(
	scope *wakeMutationScope,
	state *wakeRestartPublicationState,
	publish bool,
) (needPreflight bool, err error) {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return false, err
	}
	state.agentDir = agentDir
	current := inspectWakeLockAt(dirfd, state.agentDir, state.root, state.me)
	if publish {
		if !sameWakeLockInspection(state.expected, current) || !current.IdentityConfirmed {
			return false, fmt.Errorf("wake changed while publishing restart request")
		}
	} else {
		state.expected = current
		if err := validateWakeRestartIncumbent(state.expected, state.root, state.me, state.owner); err != nil {
			return false, err
		}
	}

	existing, exists, readErr := readWakeRestartRecordSnapshotAt(dirfd, state.agentDir)
	if readErr != nil {
		if !exists || existing.Object.FileInfo == nil {
			return false, readErr
		}
		if errors.Is(readErr, errWakeRestartSchemaTooNew) {
			return false, fmt.Errorf(
				"future-schema wake restart request is preserved at %s; retry with a newer AMQ: %w",
				wakeRestartFileName,
				readErr,
			)
		}
		quarantined, quarantineErr := quarantineWakeRestartRecordAt(dirfd, state.agentDir, existing)
		if quarantineErr != nil {
			return false, errors.Join(readErr, quarantineErr)
		}
		return false, fmt.Errorf(
			"invalid wake restart request was preserved as %s; retry restart: %w",
			quarantined,
			readErr,
		)
	}

	adopted := false
	needsNotify := true
	record := state.record
	requestID := state.requestID
	candidate := state.candidate
	if exists && existing.Record.Status == wakeRestartPending {
		disposition := classifyPendingWakeRestart(
			existing.Record,
			state.expected,
			state.root,
			state.me,
			state.owner,
		)
		if disposition == wakeRestartPendingPreserve {
			return false, fmt.Errorf(
				"pending wake restart for predecessor generation %s is preserved because it does not match live generation %s",
				existing.Record.Generation,
				state.expected.Lock.Generation,
			)
		}
		if disposition == wakeRestartPendingClaimUnstable {
			return false, fmt.Errorf(
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
			return false, fmt.Errorf("reclaim refused wake restart stage before retry: %w", err)
		}
		if _, err := quarantineWakeRestartRecordAt(dirfd, state.agentDir, existing); err != nil {
			return false, fmt.Errorf("quarantine refused wake restart request before retry: %w", err)
		}
	}
	if needsNotify {
		if err := validateWakeRestartArgv(state.expected.Lock.Args, state.root, state.me); err != nil {
			return false, err
		}
	}
	if !adopted {
		record.Generation = state.expected.Lock.Generation
		record.PreviousBoundImage = previousDarwinWakeRestartStageForLock(state.expected.Lock)
		if !publish {
			state.record = record
			state.requestID = requestID
			state.candidate = candidate
			state.adopted = false
			state.needsNotify = needsNotify
			return true, nil
		}
		if err := writeWakeRestartRecordAt(scope, record); err != nil {
			return false, err
		}
		createdSnapshot, created, readErr := readWakeRestartRecordSnapshotAt(dirfd, state.agentDir)
		if readErr != nil {
			return false, readErr
		}
		if !created || !sameWakeRestartRecord(createdSnapshot.Record, record) {
			return false, fmt.Errorf("wake restart request changed after creation")
		}
	}
	current = inspectWakeLockAt(dirfd, state.agentDir, state.root, state.me)
	if !sameWakeLockInspection(state.expected, current) || !current.IdentityConfirmed {
		return false, fmt.Errorf("wake changed while publishing restart request")
	}
	state.record = record
	state.requestID = requestID
	state.candidate = candidate
	state.adopted = adopted
	state.needsNotify = needsNotify
	return false, nil
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
	if !sameWakeSelfUpgradeRefusedCandidates(first.RefusedCandidates, second.RefusedCandidates) {
		return false
	}
	if !sameOptionalWakeImageEvidence(first.BoundImage, second.BoundImage) {
		return false
	}
	if !sameOptionalWakeImageEvidence(first.PreviousBoundImage, second.PreviousBoundImage) {
		return false
	}
	first.RefusedCandidates = nil
	second.RefusedCandidates = nil
	first.BoundImage = nil
	second.BoundImage = nil
	first.PreviousBoundImage = nil
	second.PreviousBoundImage = nil
	return reflect.DeepEqual(first, second)
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
	// Validation accepts any structurally-valid record so that a record written
	// by a different AMQ version can be read without turning into quarantine or
	// data loss. Not every accepted combination is emitted by this version:
	//
	//   Emitted by this version:
	//     - schema 1, pending, source ""       (operator `amq wake restart`)
	//     - schema 1, pending, source "self"   (self-upgrade candidate)
	//     - schema 1, refused, source ""       (operator restart refused)
	//     - schema 1, refused, source "self"   (self-upgrade refusal)
	//     - schema 2, pending (claimed handoff, source is the incumbent)
	//
	//   Reserved (accepted on read, never emitted by this version):
	//     - schema 2, refused                 (refuseWakeRestartRecordAt only
	//                                          operates on schema 1 records)
	//     - source "foreign"                  (no emitter; accepted so a record
	//                                          another version wrote survives)
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
	if record.Source != "" && record.Source != wakeRestartSourceForeign && record.Source != wakeRestartSourceSelf {
		return fmt.Errorf("wake restart source is invalid")
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
	if err := validateWakeSelfUpgradeRefusedCandidates(record); err != nil {
		return err
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

func writeWakeRestartRecordAt(scope *wakeMutationScope, record wakeRestartRecord) error {
	if err := validateWakeRestartRecord(record); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeWakeMutationMetadataAt(
		scope,
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
	scope *wakeMutationScope,
	expected wakeRestartRecordSnapshot,
	successorGeneration string,
) (wakeRestartRecord, error) {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return wakeRestartRecord{}, err
	}
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
	if err := writeWakeRestartRecordAt(scope, claimed); err != nil {
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
	scope *wakeMutationScope,
	expected wakeRestartRecordSnapshot,
	description string,
) error {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return err
	}
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
	return withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		return refuseWakeRestartRecordAt(scope, expected, reason)
	})
}

func refuseWakeRestartRecordAt(
	scope *wakeMutationScope,
	expected wakeRestartRecord,
	reason string,
) error {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "restart refused"
	}
	current, exists, err := readWakeRestartRecordAt(dirfd, agentDir)
	if err != nil || !exists {
		return err
	}
	if expected.Schema != wakeRestartSchemaV1 || expected.Status != wakeRestartPending ||
		current.Schema != wakeRestartSchemaV1 || current.Status != wakeRestartPending ||
		!sameWakeRestartAttemptIdentity(expected, current) {
		return fmt.Errorf("wake restart request changed before refusal")
	}
	if current.Source == wakeRestartSourceSelf {
		current.RefusedCandidates = rememberWakeSelfUpgradeRefusal(
			current.RefusedCandidates,
			current.Candidate,
		)
	}
	current.Status = wakeRestartRefused
	current.Reason = wakeRestartReasonWithRemedy(reason, current.Root, current.Agent)
	return writeWakeRestartRecordAt(scope, current)
}

func sameWakeRestartAttemptIdentity(expected, current wakeRestartRecord) bool {
	return expected.Schema == current.Schema &&
		expected.RequestID == current.RequestID &&
		expected.Source == current.Source &&
		expected.Root == current.Root &&
		expected.Agent == current.Agent &&
		expected.Generation == current.Generation &&
		sameWakeOwner(&expected.Owner, &current.Owner) &&
		sameWakeSelfUpgradeCandidateIdentity(expected.Candidate, current.Candidate) &&
		expected.StagePath == current.StagePath &&
		sameWakeSelfUpgradeAttemptRefusalMemory(expected, current) &&
		sameOptionalWakeImageEvidence(expected.PreviousBoundImage, current.PreviousBoundImage)
}

func sameWakeSelfUpgradeAttemptRefusalMemory(expected, current wakeRestartRecord) bool {
	if sameWakeSelfUpgradeRefusedCandidates(
		expected.RefusedCandidates,
		current.RefusedCandidates,
	) {
		return true
	}
	if expected.Source != wakeRestartSourceSelf ||
		expected.Status != wakeRestartPending ||
		current.Status != wakeRestartRefused {
		return false
	}
	return sameWakeSelfUpgradeRefusedCandidates(
		rememberWakeSelfUpgradeRefusal(expected.RefusedCandidates, expected.Candidate),
		current.RefusedCandidates,
	)
}

func retryWakeSelfUpgradeRefusal(
	cfg *wakeConfig,
	agentDir *wakeAgentDir,
	pending wakeSelfUpgradeRefusalPending,
) (resolved, persisted, continueObservation bool, returnErr error) {
	returnErr = withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
		currentLock := wakeSelfUpgradeInspectLockAt(
			dirfd,
			agentDir,
			canonicalWakeRoot(cfg.root),
			cfg.me,
		)
		if (!currentLock.Exists && currentLock.Status == wakeLockMissing) ||
			currentLock.Status == wakeLockStale {
			resolved = true
			return nil
		}
		if currentLock.Status != wakeLockValid || !currentLock.IdentityConfirmed {
			return fmt.Errorf(
				"wake lock authority is inconclusive while refusal persistence is pending: %s",
				currentLock.Reason,
			)
		}
		if cfg.wakeOwner == nil {
			return fmt.Errorf("wake owner authority is unavailable while refusal persistence is pending")
		}
		if currentLock.PID != os.Getpid() ||
			currentLock.Lock.Generation != cfg.terminalGeneration ||
			currentLock.Lock.ResumeOwner == nil ||
			!sameWakeOwner(cfg.wakeOwner, currentLock.Lock.ResumeOwner) {
			resolved = true
			return nil
		}

		current, exists, err := readWakeRestartRecordAt(dirfd, agentDir)
		if err != nil {
			return err
		}
		if !exists || !sameWakeRestartAttemptIdentity(pending.Record, current) {
			resolved = true
			continueObservation = true
			return nil
		}
		if current.Status == wakeRestartRefused {
			resolved = true
			persisted = true
			return nil
		}
		if current.Status != wakeRestartPending {
			resolved = true
			continueObservation = true
			return nil
		}

		persistErr := refuseWakeRestartRecordAt(
			scope,
			pending.Record,
			pending.Reason,
		)
		if persistErr == nil {
			resolved = true
			persisted = true
			return nil
		}

		// The metadata writer can report a post-rename sync or verification
		// failure after the refusal is already authoritative. Re-observe under
		// the same lifecycle guard before deciding whether refusal debt remains.
		installed, installedExists, readErr := readWakeRestartRecordAt(dirfd, agentDir)
		if readErr != nil {
			return errors.Join(persistErr, readErr)
		}
		if !installedExists || !sameWakeRestartAttemptIdentity(pending.Record, installed) {
			resolved = true
			continueObservation = true
			return nil
		}
		if installed.Status == wakeRestartRefused {
			resolved = true
			persisted = true
			return nil
		}
		return persistErr
	})
	return resolved, persisted, continueObservation, returnErr
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
	err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
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
			scope,
			recordSnapshot,
			lock.Generation,
		)
		if err != nil {
			return err
		}
		if err := replaceWakeLockForResumeAt(scope, current, lock); err != nil {
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
		if err := withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
			return cleanupGenericWakeGenerationAt(scope, root, me, created, options)
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
	return withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
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
			scope,
			recordSnapshot,
			"ready wake restart request",
		)
	})
}

func lockProcessInfo(lock wakeLock) wakeProcessInfo {
	return wakeProcessInfo{BootID: lock.BootID}
}

func replaceWakeLockForResumeAt(
	scope *wakeMutationScope,
	expected wakeLockInspection,
	replacement wakeLock,
) error {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return err
	}
	if replacement.Generation == "" || replacement.Generation == expected.Lock.Generation {
		return fmt.Errorf("wake resume replacement generation is invalid")
	}
	raw, err := json.Marshal(replacement)
	if err != nil {
		return err
	}
	temp, err := writeWakeOwnerTempAt(scope, "wake-resume-lock", raw, 0o600)
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
	if err := unix.Renameat(dirfd, temp, dirfd, wakeLockFileName); err != nil {
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
	err = withWakeMutationScopeInDir(agentDir, func(scope *wakeMutationScope) error {
		dirfd, scopedAgentDir, err := scope.location()
		if err != nil {
			return err
		}
		agentDir = scopedAgentDir
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
		if err := writeWakeRestartRecordAt(scope, persisted); err != nil {
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

func executeWakeRestart(
	record wakeRestartRecord,
	argv []string,
	incumbentVersion string,
	restartSignals chan os.Signal,
	armSelfUpgradeAttempt func(wakeRestartRecord) error,
) (returnErr error) {
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
	if record.Source == wakeRestartSourceSelf {
		if err := probeBoundWakeSelfUpgradeVersion(bound, record, incumbentVersion); err != nil {
			return fmt.Errorf("authorize bound wake self-upgrade: %w", err)
		}
	}
	if err := wakeRestartBoundPreflight(bound, argv, bootstrapValue); err != nil {
		return err
	}
	bootstrap, err := encodeWakeResumeBootstrap(bootstrapValue)
	if err != nil {
		return err
	}
	env := setEnvVar(unsetEnvVar(os.Environ(), envWakeResumeBootstrap), envWakeResumeBootstrap, bootstrap)
	if err := verifyWakeRestartBoundImagePlatform(bound); err != nil {
		return fmt.Errorf("%w: verify wake restart image signature: %w", errWakeImageRefused, err)
	}
	// An ignored disposition survives exec, unlike a caught disposition. The
	// bootstrap installs Notify before it rotates and advertises the successor
	// generation. Keep the expensive platform probe outside the ignored window;
	// only the final bound-image revalidation runs before exec. If exec fails,
	// restore delivery to the incumbent loop.
	wakeRestartIgnore(syscall.SIGUSR1)
	if err := revalidateBoundWakeRestartImagePlatform(bound); err != nil {
		if restartSignals != nil {
			wakeRestartSignalNotify(restartSignals, syscall.SIGUSR1)
		}
		return fmt.Errorf("%w: revalidate wake restart image: %w", errWakeImageRefused, err)
	}
	// Bind/hash failures must not arm durable crash-loop debt. Arm only after
	// bound-image validation completes, immediately before process replacement.
	if armSelfUpgradeAttempt != nil {
		if err := armSelfUpgradeAttempt(record); err != nil {
			if restartSignals != nil {
				wakeRestartSignalNotify(restartSignals, syscall.SIGUSR1)
			}
			return fmt.Errorf("arm wake self-upgrade attempt: %w", err)
		}
	}
	err = wakeRestartExec(bound.executionPath, append([]string(nil), argv...), env)
	if restartSignals != nil {
		wakeRestartSignalNotify(restartSignals, syscall.SIGUSR1)
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
		if record.Schema != wakeRestartSchemaV1 || record.SuccessorGeneration != "" ||
			record.Status != wakeRestartPending || record.Root != root || record.Agent != me ||
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

func pendingWakeSelfUpgradeForProcess(
	cfg *wakeConfig,
	agentDir *wakeAgentDir,
) (wakeRestartRecord, bool, error) {
	root := canonicalWakeRoot(cfg.root)
	var record wakeRestartRecord
	var adopt bool
	err := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		current, exists, err := readWakeRestartRecordAt(dirfd, agentDir)
		if err != nil || !exists {
			return err
		}
		if current.Schema != wakeRestartSchemaV1 || current.Status != wakeRestartPending ||
			current.Source != wakeRestartSourceSelf || current.Root != root ||
			current.Agent != cfg.me || current.Generation != cfg.terminalGeneration ||
			cfg.wakeOwner == nil || !sameWakeOwner(cfg.wakeOwner, &current.Owner) {
			return nil
		}
		incumbent := wakeSelfUpgradeInspectLockAt(dirfd, agentDir, root, cfg.me)
		if !incumbent.Exists || incumbent.Status != wakeLockValid ||
			!incumbent.IdentityConfirmed || incumbent.PID != os.Getpid() ||
			incumbent.Lock.Generation != cfg.terminalGeneration ||
			incumbent.Lock.ResumeOwner == nil ||
			!sameWakeOwner(cfg.wakeOwner, incumbent.Lock.ResumeOwner) {
			return fmt.Errorf("wake self-upgrade incumbent changed before pending-record reconciliation")
		}
		if !sameOptionalWakeImageEvidence(
			current.PreviousBoundImage,
			previousDarwinWakeRestartStageForLock(incumbent.Lock),
		) {
			return nil
		}
		record = current
		adopt = true
		return nil
	})
	return record, adopt, err
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
	if pending := cfg.selfUpgrade.refusalPending; pending != nil {
		resolved, persisted, continueObservation, retryErr := retryWakeSelfUpgradeRefusal(
			cfg,
			agentDir,
			*pending,
		)
		if resolved {
			cfg.selfUpgrade.refusalPending = nil
			cfg.selfUpgrade.restartPending = false
		}
		if (persisted || !resolved) && pending.Record.Source == wakeRestartSourceSelf &&
			cfg.inspectTerminalGeneration != nil {
			action := wakeSelfUpgradeActionRefusalPending
			reason := pending.Reason
			if persisted {
				action = wakeSelfUpgradeActionRefused
			} else if retryErr != nil {
				reason = fmt.Sprintf("%s; refusal persistence pending: %v", reason, retryErr)
			}
			_ = recordWakeSelfUpgradeDecision(
				agentDir,
				cfg.inspectTerminalGeneration(),
				cfg.selfUpgrade,
				wakeSelfUpgradeDecision{
					Action:    action,
					Reason:    reason,
					Candidate: wakeSelfUpgradeCandidateFromEvidence(pending.Record.Candidate),
				},
			)
		}
		if persisted || !resolved || !continueObservation {
			return
		}
		// A conclusively replaced or removed record retires the old refusal debt.
		// Continue this observation so a replacement request does not lose the
		// only restart signal that announced it.
	}
	record, exists, err := pendingWakeRestartForProcess(
		agentDir,
		canonicalWakeRoot(cfg.root),
		cfg.me,
		cfg.terminalGeneration,
		cfg.wakeOwner,
	)
	if err != nil {
		return
	}
	if !exists {
		cfg.selfUpgrade.restartPending = false
		return
	}
	cfg.selfUpgrade.restartPending = record.Source == wakeRestartSourceSelf
	recordSelfUpgradeDecision := func(action, reason string) {
		if record.Source != wakeRestartSourceSelf || cfg.inspectTerminalGeneration == nil {
			return
		}
		_ = recordWakeSelfUpgradeDecision(
			agentDir,
			cfg.inspectTerminalGeneration(),
			cfg.selfUpgrade,
			wakeSelfUpgradeDecision{
				Action:    action,
				Reason:    reason,
				Candidate: wakeSelfUpgradeCandidateFromEvidence(record.Candidate),
			},
		)
	}
	decision := classifyWakeRestartAtLoopBoundary(cfg, watcher, terminalRetry, scanRetry)
	if decision.Disposition != wakeResumeProceed {
		if record.Source == wakeRestartSourceSelf {
			recordSelfUpgradeDecision(wakeSelfUpgradeActionDeferred, decision.Reason)
			return
		}
		reason := wakeRestartReasonWithRemedy(decision.Reason, cfg.root, cfg.me)
		_ = refuseWakeRestartRecord(
			agentDir,
			record,
			reason,
		)
		recordSelfUpgradeDecision(wakeSelfUpgradeActionRefused, reason)
		return
	}
	if record.Source == wakeRestartSourceSelf &&
		(!cfg.selfUpgrade.Enabled || !cfg.selfUpgrade.Eligible) {
		reason := cfg.selfUpgrade.Reason
		if reason == "" {
			reason = "self-upgrade is unavailable"
		}
		recordSelfUpgradeDecision(wakeSelfUpgradeActionIneligible, reason)
		return
	}
	var armSelfUpgradeAttempt func(wakeRestartRecord) error
	if record.Source == wakeRestartSourceSelf {
		armSelfUpgradeAttempt = func(boundRecord wakeRestartRecord) error {
			attempts, err := persistWakeSelfUpgradeAttemptAtBoundary(
				agentDir,
				cfg.inspectTerminalGeneration(),
				boundRecord,
			)
			if err == nil {
				cfg.selfUpgrade.attempts = attempts
			}
			return err
		}
	}
	if err := executeWakeRestart(
		record,
		os.Args,
		cfg.terminalImageVersion,
		cfg.restartSignals,
		armSelfUpgradeAttempt,
	); err != nil {
		if record.Source == wakeRestartSourceSelf {
			var refusal wakeSelfUpgradeAttemptRefusalError
			if errors.As(err, &refusal) {
				reason := wakeRestartReasonWithRemedy(refusal.Error(), cfg.root, cfg.me)
				refuseErr := refuseWakeRestartRecord(agentDir, record, reason)
				if refuseErr == nil {
					cfg.selfUpgrade.refusalPending = nil
					cfg.selfUpgrade.restartPending = false
					recordSelfUpgradeDecision(wakeSelfUpgradeActionRefused, reason)
				} else {
					cfg.selfUpgrade.refusalPending = &wakeSelfUpgradeRefusalPending{
						Record: record,
						Reason: reason,
					}
					recordSelfUpgradeDecision(
						wakeSelfUpgradeActionRefusalPending,
						fmt.Sprintf("%s; refusal persistence pending: %v", reason, refuseErr),
					)
				}
				return
			}
			if errors.Is(err, errWakeSelfUpgradeAttemptTimestampUncertain) {
				cfg.selfUpgrade.Eligible = false
				cfg.selfUpgrade.Reason = "self-upgrade unavailable: replacement attempt timestamp is uncertain"
				recordSelfUpgradeDecision(wakeSelfUpgradeActionIneligible, cfg.selfUpgrade.Reason)
				return
			}
			if errors.Is(err, errWakeSelfUpgradeAttemptNotFresh) {
				cfg.selfUpgrade.Eligible = false
				cfg.selfUpgrade.Reason = "self-upgrade unavailable: replacement attempt is not fresh"
				recordSelfUpgradeDecision(wakeSelfUpgradeActionIneligible, cfg.selfUpgrade.Reason)
				return
			}
		}
		if errors.Is(err, errWakeImageChangedWhileHashing) && record.Source == wakeRestartSourceSelf {
			recordSelfUpgradeDecision(
				wakeSelfUpgradeActionDeferred,
				"candidate image changed while hashing (ctime); retrying at the next maintenance tick",
			)
			return
		}
		reason := wakeRestartReasonWithRemedy(err.Error(), cfg.root, cfg.me)
		if record.Source == wakeRestartSourceSelf {
			reason += "; candidate=" + wakeSelfUpgradeEvidenceIdentityString(record.Candidate)
			cfg.selfUpgrade.refusalPending = &wakeSelfUpgradeRefusalPending{
				Record: record,
				Reason: reason,
			}
		}
		refuseErr := refuseWakeRestartRecord(
			agentDir,
			record,
			reason,
		)
		if record.Source == wakeRestartSourceSelf {
			if refuseErr == nil {
				cfg.selfUpgrade.refusalPending = nil
				cfg.selfUpgrade.restartPending = false
				recordSelfUpgradeDecision(wakeSelfUpgradeActionRefused, reason)
			} else {
				recordSelfUpgradeDecision(
					wakeSelfUpgradeActionRefusalPending,
					fmt.Sprintf("%s; refusal persistence pending: %v", reason, refuseErr),
				)
			}
			return
		}
		recordSelfUpgradeDecision(wakeSelfUpgradeActionRefused, reason)
	}
}

func maintainWakeSelfUpgradeAtLoopBoundary(
	cfg *wakeConfig,
	agentDir *wakeAgentDir,
	watcher wakeAdmissionWatcher,
	terminalRetry, scanRetry bool,
) error {
	if cfg.selfUpgrade.refusalPending != nil {
		handleWakeRestartAtLoopBoundary(cfg, watcher, terminalRetry, scanRetry)
		return nil
	}
	if _, exists, pendingErr := pendingWakeSelfUpgradeForProcess(cfg, agentDir); pendingErr != nil {
		return pendingErr
	} else if exists {
		cfg.selfUpgrade.restartPending = true
		handleWakeRestartAtLoopBoundary(cfg, watcher, terminalRetry, scanRetry)
		return nil
	}
	if cfg.selfUpgrade.restartPending {
		pending, exists, pendingErr := pendingWakeRestartForProcess(
			agentDir,
			canonicalWakeRoot(cfg.root),
			cfg.me,
			cfg.terminalGeneration,
			cfg.wakeOwner,
		)
		if pendingErr != nil {
			return pendingErr
		}
		if exists && pending.Source == wakeRestartSourceSelf {
			handleWakeRestartAtLoopBoundary(cfg, watcher, terminalRetry, scanRetry)
			return nil
		}
		cfg.selfUpgrade.restartPending = false
	}
	if cfg.selfUpgrade.Enabled && cfg.selfUpgrade.Eligible {
		for _, attempt := range cfg.selfUpgrade.attempts {
			if attempt.Status == selfupgrade.AttemptStatusAttempt && attempt.IsFutureUncertain(wakeSelfUpgradeNow()) {
				cfg.selfUpgrade.Eligible = false
				cfg.selfUpgrade.Reason = "self-upgrade unavailable: replacement attempt timestamp is uncertain"
				return recordWakeSelfUpgradeDecision(
					agentDir,
					cfg.inspectTerminalGeneration(),
					cfg.selfUpgrade,
					wakeSelfUpgradeDecision{
						Action: wakeSelfUpgradeActionIneligible,
						Reason: cfg.selfUpgrade.Reason,
					},
				)
			}
		}
		attemptPending := false
		for _, attempt := range cfg.selfUpgrade.attempts {
			if attempt.Status == selfupgrade.AttemptStatusAttempt {
				attemptPending = true
				break
			}
		}
		if attemptPending {
			quiescence := classifyWakeRestartAtLoopBoundary(cfg, watcher, terminalRetry, scanRetry)
			if quiescence.Disposition != wakeResumeProceed {
				return recordWakeSelfUpgradeDecision(
					agentDir,
					cfg.inspectTerminalGeneration(),
					cfg.selfUpgrade,
					wakeSelfUpgradeDecision{
						Action: wakeSelfUpgradeActionDeferred,
						Reason: quiescence.Reason,
					},
				)
			}
			inspection := cfg.inspectTerminalGeneration()
			var running wakeImageEvidenceV1
			if inspection.Lock.RunningImageEvidence != nil {
				running = *inspection.Lock.RunningImageEvidence
			}
			if err := settleWakeSelfUpgradeAttemptAtBoundary(
				&cfg.selfUpgrade,
				agentDir,
				inspection,
				running,
			); err != nil {
				return err
			}
		}
		probe, probeErr := probeWakeSelfUpgradeLocator(cfg.selfUpgrade.Locator)
		if probeErr != nil {
			return recordWakeSelfUpgradeDecision(
				agentDir,
				cfg.inspectTerminalGeneration(),
				cfg.selfUpgrade,
				wakeSelfUpgradeDecision{
					Action: wakeSelfUpgradeActionNoCandidate,
					Reason: "stable launch locator is unavailable; retrying next maintenance tick",
				},
			)
		}
		if sameWakeSelfUpgradeProbe(cfg.selfUpgrade.lastProbe, probe) {
			return nil
		}
		if !attemptPending {
			quiescence := classifyWakeRestartAtLoopBoundary(cfg, watcher, terminalRetry, scanRetry)
			if quiescence.Disposition != wakeResumeProceed {
				return recordWakeSelfUpgradeDecision(
					agentDir,
					cfg.inspectTerminalGeneration(),
					cfg.selfUpgrade,
					wakeSelfUpgradeDecision{
						Action: wakeSelfUpgradeActionDeferred,
						Reason: quiescence.Reason,
					},
				)
			}
		}
	}
	decision, err := maintainWakeSelfUpgrade(
		&cfg.selfUpgrade,
		agentDir,
		cfg.inspectTerminalGeneration(),
	)
	if decision.Action == wakeSelfUpgradeActionPending {
		cfg.selfUpgrade.restartPending = true
		handleWakeRestartAtLoopBoundary(cfg, watcher, terminalRetry, scanRetry)
	}
	return err
}
