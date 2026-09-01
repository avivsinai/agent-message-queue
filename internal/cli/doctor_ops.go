package cli

import (
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

type doctorOpsResult struct {
	Root           opsRoot           `json:"root"`
	Agents         []opsAgent        `json:"agents"`
	OperatorGate   *opsOperatorGate  `json:"operator_gate,omitempty"`
	WakeLocks      []opsWakeLock     `json:"wake_locks,omitempty"`
	WakeQuarantine opsWakeQuarantine `json:"wake_quarantine"`
	Hints          []opsHint         `json:"hints"`
}

type opsWakeQuarantine struct {
	Count            int      `json:"count"`
	NewestAgeSeconds *float64 `json:"newest_age_seconds"`
	Error            string   `json:"error,omitempty"`
}

type opsRoot struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

type opsAgent struct {
	Handle                 string                   `json:"handle"`
	UnreadCount            int                      `json:"unread_count"`
	OldestUnreadAgeSeconds float64                  `json:"oldest_unread_age_seconds"`
	DLQCount               int                      `json:"dlq_count"`
	OldestDLQAgeSeconds    float64                  `json:"oldest_dlq_age_seconds"`
	PresenceStatus         string                   `json:"presence_status"`
	PresenceAgeSeconds     float64                  `json:"presence_age_seconds"`
	PresenceSource         string                   `json:"presence_source,omitempty"`
	NotifierStatus         string                   `json:"notifier_status,omitempty"`
	NotifierMode           string                   `json:"notifier_mode,omitempty"`
	NotifierReason         string                   `json:"notifier_reason,omitempty"`
	DoorbellParked         bool                     `json:"doorbell_parked,omitempty"`
	DoorbellAttempts       uint                     `json:"doorbell_attempts,omitempty"`
	NotificationAttempts   []opsNotificationAttempt `json:"notification_attempts,omitempty"`
}

type opsNotificationAttempt struct {
	AttemptID    string   `json:"attempt_id"`
	State        string   `json:"state"`
	MessageIDs   []string `json:"message_ids"`
	Mode         string   `json:"mode"`
	RecordedAt   string   `json:"recorded_at"`
	Sequence     uint64   `json:"sequence,omitempty"`
	Detail       string   `json:"detail,omitempty"`
	RetryPending bool     `json:"retry_pending,omitempty"`
}

type opsOperatorGate struct {
	OpenCount            int     `json:"open_count"`
	OldestGateAgeSeconds float64 `json:"oldest_gate_age_seconds"`
}

type opsHint struct {
	Code                    string                          `json:"code"`
	Status                  string                          `json:"status"`
	Message                 string                          `json:"message"`
	Backlog                 *opsBacklog                     `json:"backlog,omitempty"`
	WakeBinary              *opsWakeBinaryHint              `json:"wake_binary,omitempty"`
	UnreadBacklogNoNotifier *opsUnreadBacklogNoNotifierHint `json:"unread_backlog_no_notifier,omitempty"`
}

type opsBacklog struct {
	Root           string `json:"root"`
	CurrentSession string `json:"current_session"`
	Agent          string `json:"agent"`
	Pending        int    `json:"pending"`
	Command        string `json:"command"`
}

type opsWakeBinaryHint struct {
	Agent  string `json:"agent"`
	PID    int    `json:"pid"`
	Reason string `json:"reason,omitempty"`
	Remedy string `json:"remedy"`
}

type opsUnreadBacklogNoNotifierHint struct {
	Agent                  string  `json:"agent"`
	UnreadCount            int     `json:"unread_count"`
	OldestUnreadAgeSeconds float64 `json:"oldest_unread_age_seconds"`
	Command                string  `json:"command"`
	Remedy                 string  `json:"remedy"`
}

type opsWakeLock struct {
	Status                   string `json:"status"`
	Agent                    string `json:"agent"`
	Root                     string `json:"root"`
	Lock                     string `json:"lock"`
	PID                      int    `json:"pid,omitempty"`
	TTY                      string `json:"tty,omitempty"`
	Started                  string `json:"started,omitempty"`
	Reason                   string `json:"reason,omitempty"`
	Fix                      string `json:"fix,omitempty"`
	Removed                  bool   `json:"removed,omitempty"`
	Target                   string `json:"target,omitempty"`
	TargetPresent            bool   `json:"target_present,omitempty"`
	TargetReason             string `json:"target_reason,omitempty"`
	Repair                   string `json:"repair,omitempty"`
	RepairAvailable          bool   `json:"repair_available,omitempty"`
	RepairReason             string `json:"repair_reason,omitempty"`
	CanStartHere             bool   `json:"can_start_here"`
	StartMode                string `json:"start_mode,omitempty"`
	StartReason              string `json:"start_reason,omitempty"`
	RunningImagePath         string `json:"running_image_path,omitempty"`
	RunningVersion           string `json:"running_version,omitempty"`
	CurrentImagePath         string `json:"current_image_path,omitempty"`
	CurrentVersion           string `json:"current_version,omitempty"`
	ImageStatus              string `json:"image_status,omitempty"`
	RestartCapability        string `json:"restart_capability,omitempty"`
	RestartStagePath         string `json:"restart_stage_path,omitempty"`
	RestartStageStatus       string `json:"restart_stage_status,omitempty"`
	RestartStageReason       string `json:"restart_stage_reason,omitempty"`
	OperatorTerminalRequired bool   `json:"operator_terminal_required"`
	NextAction               string `json:"next_action,omitempty"`
	InspectionError          string `json:"inspection_error,omitempty"`
	CurrentTerminal          bool   `json:"-"`
	NotifierAbsent           bool   `json:"-"`

	WakeCheckDecision *wakeCheckDecision `json:"-"`
	Mutation          *opsWakeMutation   `json:"-"`
}

type opsWakeMutation struct {
	Status  string
	Reason  string
	Removed bool
}

func runOpsChecks(root string, rootSource string, fixWakeLocks bool, explicitBaseRoot ...string) *doctorOpsResult {
	return runOpsChecksWithSchema(
		root,
		rootSource,
		fixWakeLocks,
		wakeCheckSchemaV1,
		explicitBaseRoot...,
	)
}

func runOpsChecksWithSchema(
	root string,
	rootSource string,
	fixWakeLocks bool,
	jsonSchema int,
	explicitBaseRoot ...string,
) *doctorOpsResult {
	result := &doctorOpsResult{}
	now := time.Now()

	result.Root = opsRoot{
		Path:   root,
		Source: rootSource,
	}
	var quarantineErr error
	result.WakeQuarantine, quarantineErr = checkWakeQuarantine(root, now)
	if quarantineErr != nil {
		result.WakeQuarantine.Error = quarantineErr.Error()
		result.Hints = append(result.Hints, opsHint{
			Code:    "wake_quarantine_scan_error",
			Status:  "error",
			Message: fmt.Sprintf("Cannot scan wake quarantine: %v", quarantineErr),
		})
	}
	result.OperatorGate = checkOperatorGate(root, now)
	result.Hints = append(result.Hints, checkLinkedWorktreeLocalHint(root, rootSource)...)

	// Load the active root's config, falling back to the base config for normal
	// session layouts where coop init owns the single config.json.
	agents, err := loadOpsAgents(root, fixWakeLocks, explicitBaseRoot...)
	if err != nil {
		result.Hints = append(result.Hints, opsHint{
			Code:    "config_error",
			Status:  "error",
			Message: fmt.Sprintf("Cannot load config: %v", err),
		})
		var wakeHints []opsHint
		result.WakeLocks, wakeHints = checkWakeLocksWithHintsSchema(
			root,
			discoveredWakeLockAgents(root, nil),
			fixWakeLocks,
			jsonSchema,
		)
		result.Hints = append(result.Hints, wakeHints...)
		return result
	}

	validatedAgents := make([]string, 0, len(agents))
	for _, handle := range agents {
		// Configured handles are untrusted input. Validate before deriving any
		// mailbox/presence paths so traversal-like values cannot escape root.
		if err := fsq.ValidateHandle(handle); err != nil {
			result.Hints = append(result.Hints, opsHint{
				Code:    "config_error",
				Status:  "error",
				Message: fmt.Sprintf("Ignoring invalid configured agent handle %q: %v", handle, err),
			})
			continue
		}
		if err := validateWakeLockAgent(root, handle); err != nil {
			result.Hints = append(result.Hints, opsHint{Code: "config_error", Status: "error", Message: fmt.Sprintf("Ignoring unsafe configured agent handle %q: %v", handle, err)})
			continue
		}
		validatedAgents = append(validatedAgents, handle)
		agent := opsAgent{Handle: handle}

		// Unread count + oldest
		inboxNew := fsq.AgentInboxNew(root, handle)
		entries, err := os.ReadDir(inboxNew)
		if err == nil {
			agent.UnreadCount = len(entries)
			for _, e := range entries {
				info, err := e.Info()
				if err == nil {
					age := now.Sub(info.ModTime()).Seconds()
					if age > agent.OldestUnreadAgeSeconds {
						agent.OldestUnreadAgeSeconds = age
					}
				}
			}
		}

		// DLQ count + oldest
		dlqNew := fsq.AgentDLQNew(root, handle)
		dlqEntries, err := os.ReadDir(dlqNew)
		if err == nil {
			agent.DLQCount = len(dlqEntries)
			for _, e := range dlqEntries {
				info, err := e.Info()
				if err == nil {
					age := now.Sub(info.ModTime()).Seconds()
					if age > agent.OldestDLQAgeSeconds {
						agent.OldestDLQAgeSeconds = age
					}
				}
			}
		}

		// Presence
		recentActivity := false
		p, err := presence.Read(root, handle)
		if err == nil {
			agent.PresenceStatus = p.Status
			agent.NotifierStatus = p.NotifierStatus
			agent.NotifierMode = p.NotifierMode
			agent.NotifierReason = p.NotifierReason
			agent.DoorbellParked = p.DoorbellParked
			agent.DoorbellAttempts = p.DoorbellAttempts
			if t, err := time.Parse(time.RFC3339Nano, p.LastSeen); err == nil {
				agent.PresenceAgeSeconds = now.Sub(t).Seconds()
				recentActivity = agent.PresenceAgeSeconds < (10 * time.Minute).Seconds()
			}
		} else {
			agent.PresenceStatus = "unknown"
		}
		agent.PresenceSource = resolvePresenceSource(root, handle, recentActivity)
		agent.NotificationAttempts, err = readOpsNotificationAttempts(root, handle)
		if err != nil {
			result.Hints = append(result.Hints, opsHint{
				Code:    "notification_attempt_ledger_error",
				Status:  "warn",
				Message: fmt.Sprintf("Cannot read notification attempt ledger for %s: %v", handle, err),
			})
		}

		// Round to reasonable precision
		agent.OldestUnreadAgeSeconds = math.Round(agent.OldestUnreadAgeSeconds)
		agent.OldestDLQAgeSeconds = math.Round(agent.OldestDLQAgeSeconds)
		agent.PresenceAgeSeconds = math.Round(agent.PresenceAgeSeconds)

		result.Agents = append(result.Agents, agent)
		if agent.DoorbellParked && agent.UnreadCount > 0 &&
			agent.PresenceSource == presenceSourceNotifierLive {
			result.Hints = append(result.Hints, opsHint{
				Code:   "doorbell_parked",
				Status: "warn",
				Message: fmt.Sprintf(
					"Agent %s doorbell is parked after %d attempts with unread messages (oldest unread %.0fs); input may be stranded at the agent prompt, and a manual Enter in its pane can recover it",
					agent.Handle,
					agent.DoorbellAttempts,
					agent.OldestUnreadAgeSeconds,
				),
			})
		}
		for _, attempt := range agent.NotificationAttempts {
			if !attempt.RetryPending {
				continue
			}
			result.Hints = append(result.Hints, opsHint{
				Code:   "notification_deferred",
				Status: "warn",
				Message: fmt.Sprintf(
					"Agent %s notification attempt %s is %s; the unread cohort is retained and the existing wake retry loop will try again",
					agent.Handle,
					attempt.AttemptID,
					attempt.State,
				),
			})
		}
	}

	// Configured live agents require canonical handles. Discovery additionally
	// retains legacy-safe on-disk handles for read-only wake diagnostics;
	// checkWakeLocks repeats that inspection boundary for untrusted callers.
	var wakeHints []opsHint
	result.WakeLocks, wakeHints = checkWakeLocksWithHintsSchema(
		root,
		discoveredWakeLockAgents(root, validatedAgents),
		fixWakeLocks,
		jsonSchema,
	)

	// Operational and integration hints
	result.Hints = append(result.Hints, wakeHints...)
	result.Hints = append(result.Hints, checkUnreadBacklogNoNotifierHints(root, result.Agents, result.WakeLocks)...)
	result.Hints = append(result.Hints, checkSiblingBacklogHints(root, agents)...)
	result.Hints = append(result.Hints, checkBaseBacklogHints(root, agents)...)
	result.Hints = append(result.Hints, checkWorktreeDivergenceHints(root, agents)...)
	result.Hints = append(result.Hints, checkGlobalRootHint()...)
	result.Hints = append(result.Hints, checkKanbanHint()...)
	result.Hints = append(result.Hints, checkSymphonyHint()...)

	return result
}

func readOpsNotificationAttempts(root, agent string) ([]opsNotificationAttempt, error) {
	attempts, err := notificationattempt.List(root, agent, "")
	if err != nil {
		return nil, err
	}
	if len(attempts) == 0 {
		return nil, nil
	}
	result := make([]opsNotificationAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		recordedAt := attempt.Prepared.RecordedAt
		sequence := attempt.Prepared.Sequence
		detail := ""
		if attempt.Result != nil {
			recordedAt = attempt.Result.RecordedAt
			sequence = attempt.Result.Sequence
			detail = attempt.Result.Detail
		}
		result = append(result, opsNotificationAttempt{
			AttemptID:  attempt.Prepared.AttemptID,
			State:      attempt.State,
			MessageIDs: append([]string{}, attempt.Prepared.MessageIDs...),
			Mode:       attempt.Prepared.Mode,
			RecordedAt: recordedAt,
			Sequence:   sequence,
			Detail:     detail,
			RetryPending: attempt.State == notificationattempt.StateDeferred ||
				attempt.State == notificationattempt.StateRetried,
		})
	}
	return result, nil
}

func checkUnreadBacklogNoNotifierHints(root string, agents []opsAgent, wakeLocks []opsWakeLock) []opsHint {
	notifierAbsent := make(map[string]bool, len(wakeLocks))
	for _, lock := range wakeLocks {
		notifierAbsent[lock.Agent] = wakeLockProvesNotifierAbsent(lock)
	}

	var hints []opsHint
	for _, agent := range agents {
		if agent.UnreadCount == 0 {
			continue
		}
		absent, lockExists := notifierAbsent[agent.Handle]
		// Missing, proven-stale, and removed locks prove notifier absence.
		// Unverified and live rows stay with their existing fail-closed signals.
		if lockExists && !absent {
			continue
		}

		messageWord := "messages"
		if agent.UnreadCount == 1 {
			messageWord = "message"
		}
		command := newWakeRemedy(
			"amq", "wake", "check", "--root", root, "--me", agent.Handle,
		).String()
		remedy := "drain from the owning session"
		hints = append(hints, opsHint{
			Code:   "unread_backlog_no_notifier",
			Status: "warn",
			Message: fmt.Sprintf(
				"%s has %d unread %s, oldest %.0fs; run %s for restart capability; %s",
				agent.Handle,
				agent.UnreadCount,
				messageWord,
				agent.OldestUnreadAgeSeconds,
				command,
				remedy,
			),
			UnreadBacklogNoNotifier: &opsUnreadBacklogNoNotifierHint{
				Agent:                  agent.Handle,
				UnreadCount:            agent.UnreadCount,
				OldestUnreadAgeSeconds: agent.OldestUnreadAgeSeconds,
				Command:                command,
				Remedy:                 remedy,
			},
		})
	}
	return hints
}

func wakeLockProvesNotifierAbsent(lock opsWakeLock) bool {
	return lock.NotifierAbsent
}

func loadOpsAgents(root string, fixWakeLocks bool, explicitBaseRoot ...string) ([]string, error) {
	if len(explicitBaseRoot) > 0 && strings.TrimSpace(explicitBaseRoot[0]) != "" {
		cfg, err := config.LoadConfig(filepath.Join(explicitBaseRoot[0], "meta", "config.json"))
		if err != nil {
			return nil, err
		}
		return cfg.Agents, nil
	}
	cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json"))
	if err == nil {
		return cfg.Agents, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	if fixWakeLocks {
		return nil, err
	}
	base := baseRootOfForDisplay(root)
	if absPath(resolveRoot(base)) == absPath(resolveRoot(root)) {
		return nil, err
	}
	baseCfg, baseErr := config.LoadConfig(filepath.Join(base, "meta", "config.json"))
	if baseErr != nil {
		return nil, baseErr
	}
	return baseCfg.Agents, nil
}

func checkSiblingBacklogHints(root string, agents []string) []opsHint {
	current := siblingContext(root)
	var hints []opsHint
	for _, agent := range agents {
		for _, backlog := range findSiblingBacklogs(root, agent) {
			hints = append(hints, opsHint{
				Code:    "sibling_backlog",
				Status:  "warn",
				Message: formatSiblingBacklogHint(backlog, agent, current),
			})
		}
	}
	return hints
}

func checkBaseBacklogHints(root string, agents []string) []opsHint {
	var hints []opsHint
	for _, backlog := range findBaseBacklogs(root, agents) {
		hints = append(hints, opsHint{
			Code:    "base_backlog",
			Status:  "warn",
			Message: formatBaseBacklogHint(backlog),
			Backlog: &opsBacklog{
				Root:           backlog.Root,
				CurrentSession: backlog.Session,
				Agent:          backlog.Agent,
				Pending:        backlog.Pending,
				Command:        baseBacklogInspectionCommand(backlog),
			},
		})
	}
	return hints
}

func checkOperatorGate(root string, now time.Time) *opsOperatorGate {
	inboxNew := fsq.AgentInboxNew(root, reservedHumanHandle)
	entries, err := os.ReadDir(inboxNew)
	if err != nil {
		return nil
	}
	gate := &opsOperatorGate{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		gate.OpenCount++
		info, err := entry.Info()
		if err != nil {
			continue
		}
		age := now.Sub(info.ModTime()).Seconds()
		if age > gate.OldestGateAgeSeconds {
			gate.OldestGateAgeSeconds = age
		}
	}
	gate.OldestGateAgeSeconds = math.Round(gate.OldestGateAgeSeconds)
	return gate
}

func discoveredWakeLockAgents(root string, configured []string) []string {
	seen := make(map[string]struct{}, len(configured))
	agents := make([]string, 0, len(configured))
	for _, agent := range configured {
		if validateWakeLockAgentForInspection(root, agent) != nil {
			continue
		}
		if _, ok := seen[agent]; ok {
			continue
		}
		seen[agent] = struct{}{}
		agents = append(agents, agent)
	}

	discovered, err := fsq.ListAgents(root)
	if err != nil {
		return agents
	}
	for _, agent := range discovered {
		if _, ok := seen[agent]; ok {
			continue
		}
		if err := validateWakeLockAgentForInspection(root, agent); err != nil {
			continue
		}
		seen[agent] = struct{}{}
		agents = append(agents, agent)
	}
	return agents
}

func checkWakeLocks(root string, agents []string, fix bool) []opsWakeLock {
	locks, _ := checkWakeLocksWithHints(root, agents, fix)
	return locks
}

type wakeRepairAssessment struct {
	TargetPresent   bool
	TargetReason    string
	RepairAvailable bool
	RepairReason    string
	Repair          string
}

type wakeTargetReader func() (wakeTarget, bool, error)
type wakeRepairFloorValidator func(wakeTarget) error

func assessWakeRepair(
	root, agent string,
	inspection wakeLockInspection,
	readTarget wakeTargetReader,
	validateFloor wakeRepairFloorValidator,
) wakeRepairAssessment {
	var assessment wakeRepairAssessment
	target, exists, targetErr := readTarget()
	assessment.TargetPresent = exists
	if targetErr != nil {
		assessment.TargetReason = targetErr.Error()
	} else if exists {
		if err := validateWakeTarget(target, root, agent); err != nil {
			assessment.TargetReason = err.Error()
		} else if err := validateWakeTargetMatchesLock(inspection.Lock, target); err != nil {
			assessment.TargetReason = err.Error()
		}
	}

	ownerBound := classifyWakeClaimForGenericTransition(inspection) == wakeClaimAuthoritative
	if inspection.Status != wakeLockStale || ownerBound ||
		!assessment.TargetPresent || assessment.TargetReason != "" ||
		validateWakeLockRepairable(inspection) != nil {
		return assessment
	}
	if err := validateFloor(target); err != nil {
		assessment.RepairReason = err.Error()
		return assessment
	}
	assessment.RepairAvailable = true
	assessment.Repair = wakeRepairCommand(root, agent)
	return assessment
}

func checkWakeLocksWithHints(root string, agents []string, fix bool) ([]opsWakeLock, []opsHint) {
	return checkWakeLocksWithHintsSchema(root, agents, fix, wakeCheckSchemaV1)
}

func checkWakeLocksWithHintsSchema(
	root string,
	agents []string,
	fix bool,
	jsonSchema int,
) ([]opsWakeLock, []opsHint) {
	var locks []opsWakeLock
	var hints []opsHint
	appendLock := func(
		agent string,
		lock opsWakeLock,
		inspection wakeLockInspection,
		staleBinary bool,
		stage wakeRestartStageDiagnostic,
	) {
		fixCommand := lock.Fix
		decorateOpsWakeLockWithWakeCheck(
			root,
			agent,
			&lock,
			inspection,
			staleBinary,
			jsonSchema == wakeCheckSchemaV2,
		)
		lock.RestartStagePath = stage.Path
		lock.RestartStageStatus = stage.Status
		lock.RestartStageReason = stage.Reason
		applyWakeBinaryDirGoneOpsReason(&lock, inspection, stage)
		if fixCommand != "" && (stage.Status != "" || lock.Reason == wakeReasonBinaryDirGone) {
			lock.Fix = fixCommand
		}
		if lock.WakeCheckDecision != nil && lock.WakeCheckDecision.Platform.WakeSupported {
			status := lock.WakeCheckDecision.Wake.Status
			lock.NotifierAbsent = status == string(wakeLockMissing) || status == string(wakeLockStale)
		} else {
			lock.NotifierAbsent = !inspection.Exists || inspection.Status == wakeLockStale
		}
		locks = append(locks, lock)
	}
	for _, agent := range agents {
		if validateWakeLockAgentForInspection(root, agent) != nil {
			continue
		}
		mutationAuthorized := fsq.ValidateHandle(agent) == nil
		inspection := inspectWakeLock(root, agent)
		stage := diagnoseWakeRestartStage(root, agent, inspection)
		if !inspection.Exists {
			if stage.Status == "" {
				continue
			}
			lock := opsWakeLock{
				Status: string(wakeLockMissing),
				Agent:  agent,
				Root:   inspection.Root,
				Lock:   inspection.LockPath,
				Reason: inspection.Reason,
				Fix:    doctorRootCommandForOS(root, "", runtime.GOOS, "--ops", "--fix-wake-locks"),
			}
			if fix {
				fixErr := fixWakeRestartResidueWithoutLock(root, agent)
				inspection = inspectWakeLock(root, agent)
				stage = diagnoseWakeRestartStage(root, agent, inspection)
				if fixErr != nil {
					lock.Status = "error"
					lock.Reason = fixErr.Error()
				} else {
					lock.Fix = ""
					lock.Status = "fixed"
					lock.Reason = ""
					lock.Removed = true
				}
				lock.Mutation = &opsWakeMutation{
					Status:  lock.Status,
					Reason:  lock.Reason,
					Removed: lock.Removed,
				}
			}
			appendLock(agent, lock, inspection, false, stage)
			continue
		}
		staleBinary := false
		wakeBinaryInspectionError := ""
		if hint, ok, err := checkStaleWakeBinaryHint(inspection); err != nil {
			wakeBinaryInspectionError = err.Error()
		} else if ok {
			hints = append(hints, hint)
			staleBinary = true
		}

		lock := opsWakeLock{
			Status:          string(inspection.Status),
			Agent:           agent,
			Root:            inspection.Root,
			Lock:            inspection.LockPath,
			PID:             inspection.PID,
			TTY:             strings.TrimSpace(inspection.Lock.TTY),
			Started:         strings.TrimSpace(inspection.Lock.Started),
			Reason:          inspection.Reason,
			InspectionError: wakeBinaryInspectionError,
			CurrentTerminal: doctorWakeLockOnCurrentTerminal(inspection),
		}
		ownerBound := classifyWakeClaimForGenericTransition(inspection) == wakeClaimAuthoritative
		if ownerBound && mutationAuthorized {
			lock.Fix = wakeRecoverOwnerCommand(root, agent)
		}
		if isLiveRawOrphan(inspection) {
			lock.Status = "live-raw-orphan"
			lock.Reason = "live raw wake orphan; stop the owning terminal or launchd supervisor"
		}
		if !mutationAuthorized {
			appendLock(agent, lock, inspection, staleBinary, stage)
			continue
		}
		assessment := assessWakeRepair(
			root,
			agent,
			inspection,
			func() (wakeTarget, bool, error) {
				return readWakeTargetFromStateForInspection(root, agent, inspection)
			},
			func(target wakeTarget) error {
				return validateWakeRepairFloorAvailable(root, agent, inspection, target)
			},
		)
		lock.TargetReason = assessment.TargetReason
		if assessment.TargetPresent {
			lock.Target = wakeTargetPath(root, agent)
			lock.TargetPresent = true
		}
		if inspection.Status == wakeLockStale {
			if ownerBound {
				appendLock(agent, lock, inspection, staleBinary, stage)
				continue
			}
			lock.Fix = doctorRootCommandForOS(root, "", runtime.GOOS, "--ops", "--fix-wake-locks")
			lock.RepairAvailable = assessment.RepairAvailable
			lock.RepairReason = assessment.RepairReason
			lock.Repair = assessment.Repair
			if fix {
				guardErr := fixStaleWakeLockForDoctor(root, agent, &inspection, &lock)
				// The mutation helper may retain an agent-directory capability across
				// a pathname replacement. Keep its mutation outcome, but classify the
				// notifier from a fresh canonical inspection.
				inspection = inspectWakeLock(root, agent)
				staleBinary = false
				lock.RepairAvailable = false
				lock.Repair = ""
				lock.RepairReason = ""
				if guardErr != nil {
					lock.Status = "error"
					lock.Reason = guardErr.Error()
				}
				lock.Mutation = &opsWakeMutation{
					Status:  lock.Status,
					Reason:  lock.Reason,
					Removed: lock.Removed,
				}
			}
		}
		if fix {
			stage = diagnoseWakeRestartStage(root, agent, inspection)
		}
		appendLock(agent, lock, inspection, staleBinary, stage)
	}
	return locks, hints
}

// validateWakeLockAgent ensures diagnostics cannot follow an agent directory
// symlink (including one pointing outside this AMQ root). Missing directories
// remain valid for configured agents: inspectWakeLock will simply report no
// lock, preserving diagnostics for partially initialized roots.
func validateWakeLockAgent(root, agent string) error {
	if err := fsq.ValidateHandle(agent); err != nil {
		return err
	}
	return validateWakeLockAgentPath(root, agent)
}

func validateWakeLockAgentForInspection(root, agent string) error {
	if err := fsq.ValidateLegacyHandleForInspection(agent); err != nil {
		return err
	}
	return validateWakeLockAgentPath(root, agent)
}

func validateWakeLockAgentPath(root, agent string) error {
	path := fsq.AgentBase(root, agent)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("agent directory is a symlink")
	}
	if !info.IsDir() {
		return fmt.Errorf("agent path is not a directory")
	}
	return nil
}

func wakeRepairCommand(root, agent string) string {
	return wakeRepairRemedy(root, agent).String()
}

func wakeRecoverOwnerCommand(root, agent string) string {
	return wakeRecoverOwnerRemedy(root, agent).String()
}

func shellQuoteArg(value string) string {
	if value == "" {
		return "''"
	}
	if strings.IndexFunc(value, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= 'A' && r <= 'Z':
			return false
		case r >= '0' && r <= '9':
			return false
		case strings.ContainsRune("@%_+=:,./-", r):
			return false
		default:
			return true
		}
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func checkGlobalRootHint() []opsHint {
	_, err := loadGlobalAmqrc()
	if err != nil {
		globalEnv := os.Getenv(envGlobalRoot)
		if globalEnv == "" {
			return []opsHint{{
				Code:    "global_root_missing",
				Status:  "warn",
				Message: "No global AMQ config found (~/.amqrc or AMQ_GLOBAL_ROOT). Agents spawned by external orchestrators may not find AMQ.",
			}}
		}
		return []opsHint{{
			Code:    "global_root_configured",
			Status:  "ok",
			Message: fmt.Sprintf("Global root configured via AMQ_GLOBAL_ROOT=%s", globalEnv),
		}}
	}
	return []opsHint{{
		Code:    "global_root_configured",
		Status:  "ok",
		Message: "Global root configured via ~/.amqrc",
	}}
}

func checkKanbanHint() []opsHint {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:3484", 500*time.Millisecond)
	if err != nil {
		return nil // Kanban not running, no hint needed
	}
	_ = conn.Close()
	return []opsHint{{
		Code:    "kanban_detected",
		Status:  "warn",
		Message: "Cline Kanban appears to be running on 127.0.0.1:3484. Use the experimental 'amq integration kanban bridge' adapter to connect.",
	}}
}

func checkSymphonyHint() []opsHint {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	workflowPath := filepath.Join(cwd, "WORKFLOW.md")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		return nil // No WORKFLOW.md
	}
	content := string(data)
	if strings.Contains(content, "BEGIN AMQ MANAGED") {
		return []opsHint{{
			Code:    "symphony_hooks_installed",
			Status:  "ok",
			Message: "WORKFLOW.md has AMQ integration hooks installed",
		}}
	}
	return []opsHint{{
		Code:    "symphony_workflow_detected",
		Status:  "warn",
		Message: "WORKFLOW.md found but no AMQ hooks. Use 'amq integration symphony init' to install.",
	}}
}
