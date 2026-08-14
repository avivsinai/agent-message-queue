package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const LauncherAuto = "auto"

type ConversationDisposition string

const (
	DispositionResumed         ConversationDisposition = "resumed"
	DispositionFresh           ConversationDisposition = "fresh"
	DispositionFreshAfterStale ConversationDisposition = "fresh_after_stale"
	DispositionDisabled        ConversationDisposition = "disabled"
	DispositionUnsupported     ConversationDisposition = "unsupported"
	DispositionDegraded        ConversationDisposition = "degraded"
	DispositionActionRequired  ConversationDisposition = "action_required"
)

const (
	ReasonNoSavedConversation    = "no_saved_conversation"
	ReasonPriorLaunchNotExecuted = "prior_launch_not_executed"
	ReasonStaleConversation      = "stale_conversation"
)

type RebindDisposition string

const (
	RebindClose RebindDisposition = "close"
	RebindLeave RebindDisposition = "leave"
)

type ConfirmTrustFunc func(Plan, string) (bool, error)
type ConfirmRebindFunc func(BindingRecord, bool) (RebindDisposition, bool, error)

type ReconcileRequest struct {
	Context            context.Context
	ProjectRoot        string
	Session            string
	AMQPath            string
	Root               *fsq.DeliveryRoot
	Config             ProjectConfig
	Launcher           string
	Preferences        []string
	Backends           map[string]Backend
	Adapters           map[string]HarnessAdapter
	TrustStore         *TrustStore
	ConfirmTrust       ConfirmTrustFunc
	ConfirmRebind      ConfirmRebindFunc
	Fresh              bool
	AllowFreshFallback bool
	ResumeOnly         bool
	Rebind             bool
	HostIdentity       string
	CrashHook          func(string) error
}

type AgentReconcileResult struct {
	Handle                  string                  `json:"handle"`
	Code                    int                     `json:"code"`
	ConversationDisposition ConversationDisposition `json:"conversation_disposition"`
	Reason                  string                  `json:"reason"`
}

type ReconcileResult struct {
	Session        string                 `json:"session"`
	Backend        string                 `json:"backend"`
	Outcome        Outcome                `json:"outcome"`
	AggregateCode  int                    `json:"aggregate_code"`
	Reason         string                 `json:"reason"`
	Agents         []AgentReconcileResult `json:"agents"`
	Commands       []EmittedCommand       `json:"commands"`
	Plan           *Plan                  `json:"plan"`
	SemanticDigest string                 `json:"semantic_digest"`
	Recovery       *RecoveryReport        `json:"recovery"`
}

type RecoveryReport struct {
	Status    ReclaimStatus      `json:"status"`
	Evidence  string             `json:"evidence"`
	Resources []ResourceIdentity `json:"resources"`
}

type plannedAgent struct {
	plan            AgentPlan
	record          ConversationRecord
	write           bool
	index           int
	adapter         HarnessAdapter
	providerVersion string
}

func Reconcile(request ReconcileRequest) (result ReconcileResult, returnErr error) {
	result = ReconcileResult{Session: request.Session, Agents: []AgentReconcileResult{}, Commands: []EmittedCommand{}}
	if request.Context == nil {
		request.Context = context.Background()
	}
	if request.Root == nil {
		return result, fmt.Errorf("missing pinned session root")
	}
	if strings.TrimSpace(request.ProjectRoot) == "" || strings.TrimSpace(request.Session) == "" {
		return result, fmt.Errorf("project root and session are required")
	}
	if err := request.Config.Validate(); err != nil {
		return result, err
	}
	journal, hasJournal, err := loadOptionalJournal(request.Root)
	if err != nil {
		result.AggregateCode, result.Outcome, result.Reason = 6, OutcomeActionRequired, "launch_journal_unreadable"
		return result, nil
	}
	var nonce string
	var planned []plannedAgent
	if hasJournal {
		if err := journal.ValidateRequest(request); err != nil {
			result.AggregateCode, result.Outcome, result.Reason = 6, OutcomeActionRequired, "launch_journal_context_mismatch"
			return result, nil
		}
		nonce = journal.LaunchNonce
		planned, err = plannedFromJournal(request, journal, &result)
		if err != nil {
			result.AggregateCode, result.Outcome, result.Reason = 6, OutcomeActionRequired, "launch_journal_plan_unavailable"
			return result, nil
		}
	} else {
		nonce, err = generateNonce()
		if err == nil {
			planned, err = buildReconcilePlan(request, nonce, &result)
		}
	}
	if err != nil {
		return result, err
	}
	if len(planned) == 0 {
		result.AggregateCode = aggregateReconcileCode(result.Agents)
		if result.AggregateCode == 6 {
			result.Outcome = OutcomeActionRequired
			result.Reason = firstReconcileReason(result.Agents, 6)
		}
		return result, nil
	}
	plan := journal.Plan
	if !hasJournal {
		plan = Plan{Version: PlanVersion, Agents: make([]AgentPlan, 0, len(planned))}
		for _, agent := range planned {
			plan.Agents = append(plan.Agents, agent.plan)
		}
	}
	planDigest, err := plan.SemanticDigest()
	if err != nil {
		return result, err
	}
	trustDigest, err := ExecutionTrustDigest(plan, request.Session, request.Root)
	if err != nil {
		return result, err
	}
	result.Plan, result.SemanticDigest = &plan, trustDigest
	trusted, err := ensurePlanTrust(request, plan, trustDigest)
	if err != nil {
		markPlannedAgents(&result, planned, 6, "trust_state_unreadable")
		result.AggregateCode, result.Outcome, result.Reason = 6, OutcomeActionRequired, err.Error()
		return result, nil
	}
	if !trusted {
		markPlannedAgents(&result, planned, 6, "untrusted_config_digest")
		result.AggregateCode = 6
		result.Outcome = OutcomeActionRequired
		result.Reason = "launch plan requires local trust confirmation"
		return result, nil
	}

	lease, err := AcquireLease(request.Root, nonce)
	if err != nil {
		markPlannedAgents(&result, planned, 6, "launch_lease_unavailable")
		result.AggregateCode = 6
		result.Outcome = OutcomeActionRequired
		result.Reason = err.Error()
		return result, nil
	}
	defer func() {
		if err := lease.Release(); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	handles := make([]string, 0, len(planned))
	for _, agent := range planned {
		handles = append(handles, agent.plan.Handle)
	}
	if err := lease.LockHandles(handles...); err != nil {
		return result, err
	}
	if err := callCrashHook(request.CrashHook, "lease_acquired"); err != nil {
		return result, err
	}

	binding, hasBinding, err := loadOptionalBinding(request.Root)
	if err != nil {
		markPlannedAgents(&result, planned, 6, "binding_unreadable")
		result.AggregateCode, result.Outcome, result.Reason = 6, OutcomeActionRequired, err.Error()
		return result, nil
	}
	var backendName string
	var backend Backend
	var detect DetectResult
	var create CreateResult
	recoveredCreate := false
	journalActive := hasJournal
	if hasJournal {
		current, present, loadErr := loadOptionalJournal(request.Root)
		if loadErr != nil || !present || !reflect.DeepEqual(current, journal) {
			result.AggregateCode, result.Outcome, result.Reason = 6, OutcomeActionRequired, "launch_journal_changed"
			return result, nil
		}
		journal = current
		backendName, backend, detect, create, recoveredCreate, err = recoverJournal(request, journal, binding, hasBinding, &result)
		if err != nil {
			return result, err
		}
		if result.Outcome == OutcomeActionRequired {
			markPlannedAgents(&result, planned, 6, result.Reason)
			result.AggregateCode = 6
			return result, nil
		}
		if hasBinding && journalMatchesBinding(journal, binding) {
			if journal.Phase != JournalCreated {
				result.AggregateCode, result.Outcome, result.Reason = 6, OutcomeActionRequired, "launch_journal_incomplete"
				return result, nil
			}
			if err := commitRecoveredJournal(request, lease, journal); err != nil {
				return result, err
			}
			result.Backend, result.Outcome = journal.Backend, OutcomeCreated
			result.AggregateCode = aggregateReconcileCode(result.Agents)
			return result, nil
		}
		if !recoveredCreate {
			// Reclaim proved the old generation absent. Recreate the exact trusted
			// plan under the existing resume policy and journal generation.
			if err := ClearJournal(request.Root, lease, journal); err != nil {
				return result, err
			}
			journalActive = false
		}
	} else {
		attachSafe := true
		for _, agent := range planned {
			attachSafe = attachSafe && !agent.write
		}
		var proceed bool
		backendName, backend, detect, proceed, err = chooseReconcileBackend(request, binding, hasBinding, attachSafe, &result)
		if err != nil || !proceed {
			if err != nil {
				code := reconcileErrorCode(err)
				markPlannedAgents(&result, planned, code, "backend_reconciliation_failed")
				result.AggregateCode, result.Reason = code, err.Error()
				return result, nil
			}
			if result.Outcome == OutcomeAttached {
				result.AggregateCode = aggregateReconcileCode(result.Agents)
				return result, nil
			}
			markPlannedAgents(&result, planned, 6, result.Reason)
			result.AggregateCode = 6
			return result, nil
		}
	}
	result.Backend = backendName

	if !recoveredCreate {
		if detect.Profile.Has(CapCreate) {
			journal, err = NewLaunchJournal(request, backendName, detect, plan, planDigest, nonce, result.Agents, plannedConversations(planned), time.Now())
			if err != nil {
				return result, err
			}
			if err := WriteJournal(request.Root, lease, journal); err != nil {
				return result, err
			}
			journalActive = true
			if err := callCrashHook(request.CrashHook, "journal_written"); err != nil {
				return result, err
			}
		}
		create, err = backend.Create(CreateRequest{Session: request.Session, Plan: plan, AMQPath: request.AMQPath, Root: request.Root})
		if err != nil {
			var definite *DefinitePreCreateError
			if journalActive && errors.As(err, &definite) {
				if clearErr := ClearJournal(request.Root, lease, journal); clearErr != nil {
					return result, errors.Join(err, clearErr)
				}
			}
			code := reconcileErrorCode(err)
			markPlannedAgents(&result, planned, code, "backend_create_failed")
			result.AggregateCode, result.Reason = code, err.Error()
			return result, nil
		}
		if err := callCrashHook(request.CrashHook, "backend_created"); err != nil {
			return result, err
		}
	}
	result.Outcome, result.Commands = create.Outcome, create.Commands
	if create.Outcome == OutcomeCommandsEmitted {
		if err := writeExecutionTickets(request, lease, planned); err != nil {
			return result, err
		}
	}
	var candidate *BindingRecord
	if create.Outcome == OutcomeCreated {
		if journalActive && journal.Phase == JournalCreated {
			candidate = journal.Binding
		} else {
			candidate, err = finalizeCreatedState(create, backendName, detect, nonce, planned, &result)
			if err != nil {
				return result, err
			}
		}
	}
	if journalActive {
		if candidate == nil {
			result.AggregateCode, result.Outcome, result.Reason = 6, OutcomeActionRequired, "managed_create_not_completed"
			return result, nil
		}
		journal.Phase, journal.Binding = JournalCreated, candidate
		journal.Agents, journal.Conversations = slices.Clone(result.Agents), plannedConversations(planned)
		if err := WriteJournal(request.Root, lease, journal); err != nil {
			return result, err
		}
		if err := callCrashHook(request.CrashHook, "journal_created"); err != nil {
			return result, err
		}
	}
	for i := range planned {
		if planned[i].write {
			if err := WriteConversation(request.Root, lease, planned[i].record); err != nil {
				return result, err
			}
		}
	}
	if err := callCrashHook(request.CrashHook, "conversations_written"); err != nil {
		return result, err
	}
	if candidate != nil {
		if recoveredCreate {
			if err := revalidateAdoption(request, backend, journal, *candidate); err != nil {
				result.AggregateCode, result.Outcome, result.Reason = 6, OutcomeActionRequired, "launch_recovery_changed"
				return result, nil
			}
		}
		if err := WriteBinding(request.Root, lease, *candidate); err != nil {
			return result, err
		}
		if err := callCrashHook(request.CrashHook, "binding_written"); err != nil {
			return result, err
		}
	}
	if journalActive {
		if err := ClearJournal(request.Root, lease, journal); err != nil {
			return result, err
		}
		if err := callCrashHook(request.CrashHook, "journal_cleared"); err != nil {
			return result, err
		}
	}
	if create.ActionRequired || create.Outcome == OutcomeCommandsEmitted || create.Outcome == OutcomeActionRequired {
		for _, agent := range planned {
			if result.Agents[agent.index].Code == 0 {
				result.Agents[agent.index].Code = 6
				if result.Agents[agent.index].Reason == "" {
					result.Agents[agent.index].Reason = "commands_emitted"
				}
			}
		}
		result.AggregateCode = 6
		result.Reason = "execute the emitted commands to complete launch"
		return result, nil
	}
	result.AggregateCode = aggregateReconcileCode(result.Agents)
	if result.AggregateCode != 0 && result.Reason == "" {
		result.Reason = firstReconcileReason(result.Agents, result.AggregateCode)
	}
	return result, nil
}

const reclaimTimeout = 5 * time.Second

func plannedFromJournal(request ReconcileRequest, journal LaunchJournal, result *ReconcileResult) ([]plannedAgent, error) {
	result.Agents = slices.Clone(journal.Agents)
	planned := make([]plannedAgent, len(journal.Plan.Agents))
	for i, plan := range journal.Plan.Agents {
		var cfg ProjectAgentConfig
		foundConfig := false
		for _, candidate := range request.Config.Agents {
			if candidate.Handle == plan.Handle {
				cfg, foundConfig = candidate, true
				break
			}
		}
		if !foundConfig {
			return nil, fmt.Errorf("launch journal handle %q does not match current roster", plan.Handle)
		}
		adapter := request.Adapters[cfg.Adapter]
		if adapter == nil || adapter.Mode() != plan.AdapterMode {
			return nil, fmt.Errorf("launch journal adapter for %q is unavailable or changed", plan.Handle)
		}
		capabilities := adapter.Capabilities(request.Context)
		if err := ValidateAdapterCapabilities(adapter, capabilities); err != nil {
			return nil, err
		}
		if !capabilities.Available {
			return nil, fmt.Errorf("launch journal adapter for %q is unavailable", plan.Handle)
		}
		resultIndex := -1
		for j, agentResult := range journal.Agents {
			if agentResult.Handle == plan.Handle {
				resultIndex = j
				break
			}
		}
		if resultIndex < 0 {
			return nil, fmt.Errorf("launch journal handle %q has no result", plan.Handle)
		}
		write := journal.Agents[resultIndex].ConversationDisposition != DispositionResumed
		planned[i] = plannedAgent{
			plan: plan, record: journal.Conversations[i], write: write, index: resultIndex,
			adapter: adapter, providerVersion: journal.Conversations[i].ProviderVersion,
		}
	}
	return planned, nil
}

func recoverJournal(request ReconcileRequest, journal LaunchJournal, binding BindingRecord, hasBinding bool, result *ReconcileResult) (string, Backend, DetectResult, CreateResult, bool, error) {
	requested := strings.TrimSpace(request.Launcher)
	if requested != "" && requested != LauncherAuto && requested != journal.Backend {
		result.Outcome, result.Reason = OutcomeActionRequired, "launch_journal_backend_conflict"
		return journal.Backend, nil, DetectResult{}, CreateResult{}, false, nil
	}
	backend := request.Backends[journal.Backend]
	if backend == nil {
		result.Outcome, result.Reason = OutcomeActionRequired, "journaled_launcher_not_available"
		return journal.Backend, nil, DetectResult{}, CreateResult{}, false, nil
	}
	detect := backend.Detect()
	if err := detect.Validate(); err != nil {
		return journal.Backend, backend, detect, CreateResult{}, false, err
	}
	if !detect.Available || detect.Profile.Identity() != journal.Profile || detect.HostIdentity != journal.HostIdentity || detect.InstanceIdentity != journal.InstanceIdentity {
		result.Outcome, result.Reason = OutcomeActionRequired, "launch_journal_context_mismatch"
		return journal.Backend, backend, detect, CreateResult{}, false, nil
	}
	if request.HostIdentity != "" && request.HostIdentity != journal.HostIdentity {
		result.Outcome, result.Reason = OutcomeActionRequired, "launch_journal_context_mismatch"
		return journal.Backend, backend, detect, CreateResult{}, false, nil
	}
	if hasBinding {
		if !journalMatchesBinding(journal, binding) {
			result.Outcome, result.Reason = OutcomeActionRequired, "launch_journal_binding_conflict"
		}
		return journal.Backend, backend, detect, CreateResult{}, false, nil
	}
	reclaimer, ok := backend.(BackendReclaimer)
	if !ok || !detect.Profile.Has(CapReclaim) {
		result.Outcome, result.Reason = OutcomeActionRequired, "launch_recovery_not_supported"
		return journal.Backend, backend, detect, CreateResult{}, false, nil
	}
	reclaim, err := callReclaim(request.Context, reclaimer, journal, request.Root)
	if err != nil {
		return journal.Backend, backend, detect, CreateResult{}, false, err
	}
	result.Recovery = &RecoveryReport{Status: reclaim.Status, Evidence: reclaim.Evidence, Resources: slices.Clone(reclaim.Resources)}
	if strings.TrimSpace(reclaim.Evidence) == "" {
		return journal.Backend, backend, detect, CreateResult{}, false, fmt.Errorf("backend reclaim returned no evidence")
	}
	switch reclaim.Status {
	case ReclaimAbsent:
		return journal.Backend, backend, detect, CreateResult{}, false, nil
	case ReclaimAdoptable:
		if err := reclaim.Binding.Validate(); err != nil || !journalMatchesBinding(journal, reclaim.Binding) {
			result.Outcome, result.Reason = OutcomeActionRequired, "launch_recovery_identity_mismatch"
			return journal.Backend, backend, detect, CreateResult{}, false, nil
		}
		if journal.Phase == JournalCreated && (journal.Binding == nil || !reflect.DeepEqual(*journal.Binding, reclaim.Binding)) {
			result.Outcome, result.Reason = OutcomeActionRequired, "launch_recovery_identity_mismatch"
			return journal.Backend, backend, detect, CreateResult{}, false, nil
		}
		create := CreateResult{
			Outcome: OutcomeCreated, Profile: journal.Profile, Binding: reclaim.Binding,
			CaptureEvidence: reclaim.CaptureEvidence,
		}
		return journal.Backend, backend, detect, create, true, nil
	case ReclaimIncomplete:
		result.Outcome, result.Reason = OutcomeActionRequired, "launch_recovery_incomplete"
	case ReclaimForeign:
		result.Outcome, result.Reason = OutcomeActionRequired, "launch_recovery_foreign"
	case ReclaimUnknown:
		result.Outcome, result.Reason = OutcomeActionRequired, "launch_recovery_unknown"
	default:
		return journal.Backend, backend, detect, CreateResult{}, false, fmt.Errorf("backend reclaim returned invalid status %q", reclaim.Status)
	}
	return journal.Backend, backend, detect, CreateResult{}, false, nil
}

func callReclaim(parent context.Context, backend BackendReclaimer, journal LaunchJournal, root *fsq.DeliveryRoot) (ReclaimResult, error) {
	ctx, cancel := context.WithTimeout(parent, reclaimTimeout)
	defer cancel()
	return backend.Reclaim(ReclaimRequest{Context: ctx, Journal: journal, Root: root})
}

func revalidateAdoption(request ReconcileRequest, backend Backend, journal LaunchJournal, candidate BindingRecord) error {
	reclaimer, ok := backend.(BackendReclaimer)
	if !ok {
		return fmt.Errorf("backend no longer supports reclaim")
	}
	reclaim, err := callReclaim(request.Context, reclaimer, journal, request.Root)
	if err != nil {
		return err
	}
	if reclaim.Status != ReclaimAdoptable || !reflect.DeepEqual(reclaim.Binding, candidate) {
		return fmt.Errorf("journaled resource changed before binding commit")
	}
	return nil
}

func finalizeCreatedState(create CreateResult, backendName string, detect DetectResult, nonce string, planned []plannedAgent, result *ReconcileResult) (*BindingRecord, error) {
	created := create.Binding
	if created.Backend != backendName || created.Profile != detect.Profile.Identity() || created.LaunchNonce != nonce ||
		created.HostIdentity != detect.HostIdentity || created.InstanceIdentity != detect.InstanceIdentity {
		return nil, fmt.Errorf("backend returned a binding outside the selected launch generation")
	}
	if err := created.Validate(); err != nil {
		return nil, fmt.Errorf("backend returned invalid binding: %w", err)
	}
	executionEvidence := ConversationExecutionEvidence{
		Backend: backendName, Profile: detect.Profile.Identity(), Outcome: OutcomeCreated, LaunchNonce: nonce,
	}
	for i := range planned {
		agent := &planned[i]
		if agent.write {
			evidence := executionEvidence
			agent.record.ExecutionEvidence = &evidence
		}
		if agent.plan.AdapterMode == AdapterModeMint && agent.write {
			agent.record.State = CaptureReady
			agent.record.Identity = ConversationIdentity{Provider: agent.adapter.Name(), ID: agent.plan.ConversationID}
		}
		if agent.plan.AdapterMode == AdapterModeCapture && agent.write {
			capture := agent.adapter.CaptureIdentity(CaptureRequest{
				LaunchNonce: agent.plan.LaunchNonce, ExpectedProviderVersion: agent.providerVersion,
				Final: true, Evidence: create.CaptureEvidence[agent.plan.Handle],
			})
			agent.record.State, agent.record.Identity, agent.record.Reason = capture.State, capture.Identity, capture.Reason
			if !capture.CanPersist() {
				result.Agents[agent.index].Code = 6
				result.Agents[agent.index].ConversationDisposition = DispositionDegraded
				if capture.State == CaptureUnsupported && !capture.Degraded {
					result.Agents[agent.index].Code = 0
					result.Agents[agent.index].ConversationDisposition = DispositionUnsupported
				}
				result.Agents[agent.index].Reason = string(capture.Reason)
			}
		}
		if agent.record.State == CaptureReady && agent.record.ExecutionEvidence != nil {
			agent.record.ExecutionEvidence.ConversationID = agent.record.Identity.ID
		}
	}
	return &created, nil
}

func plannedConversations(planned []plannedAgent) []ConversationRecord {
	records := make([]ConversationRecord, len(planned))
	for i := range planned {
		records[i] = planned[i].record
	}
	return records
}

func writeExecutionTickets(request ReconcileRequest, lease *Lease, planned []plannedAgent) error {
	amqPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current amq executable: %w", err)
	}
	for _, agent := range planned {
		ticket, err := NewExecutionTicket(ExecutionTicketRequest{
			Handle: agent.plan.Handle, LaunchNonce: agent.plan.LaunchNonce,
			Mode: agent.plan.AdapterMode, Provider: agent.adapter.Name(), ConversationID: agent.plan.ConversationID,
			ProjectRoot: request.ProjectRoot, SessionRoot: request.Root.Base(), Cwd: agent.plan.Cwd,
			ProviderExecutable: agent.plan.Argv[0], AMQExecutable: amqPath,
			TargetArgv: agent.plan.Argv, TargetEnv: agent.plan.EnvOverlay,
		})
		if err != nil {
			return fmt.Errorf("build execution ticket for %s: %w", agent.plan.Handle, err)
		}
		if err := WriteExecutionTicket(request.Root, lease, ticket); err != nil {
			return fmt.Errorf("write execution ticket for %s: %w", agent.plan.Handle, err)
		}
	}
	return nil
}

func commitRecoveredJournal(request ReconcileRequest, lease *Lease, journal LaunchJournal) error {
	for _, record := range journal.Conversations {
		if err := WriteConversation(request.Root, lease, record); err != nil {
			return err
		}
	}
	return ClearJournal(request.Root, lease, journal)
}

func buildReconcilePlan(request ReconcileRequest, nonce string, result *ReconcileResult) ([]plannedAgent, error) {
	planned := make([]plannedAgent, 0, len(request.Config.Agents))
	for _, cfg := range request.Config.Agents {
		item := AgentReconcileResult{Handle: cfg.Handle}
		adapter := request.Adapters[cfg.Adapter]
		if adapter == nil {
			item.ConversationDisposition, item.Reason = DispositionUnsupported, "adapter_not_registered"
			result.Agents = append(result.Agents, item)
			continue
		}
		capabilities := adapter.Capabilities(request.Context)
		if err := ValidateAdapterCapabilities(adapter, capabilities); err != nil {
			item.Code, item.ConversationDisposition, item.Reason = 1, DispositionDegraded, "invalid_adapter_capabilities"
			result.Agents = append(result.Agents, item)
			continue
		}
		if !capabilities.Available {
			item.ConversationDisposition, item.Reason = DispositionUnsupported, capabilities.Reason
			result.Agents = append(result.Agents, item)
			continue
		}
		cwd := cfg.Cwd
		if strings.TrimSpace(cwd) == "" {
			cwd = request.ProjectRoot
		} else if !filepath.IsAbs(cwd) {
			cwd = filepath.Join(request.ProjectRoot, cwd)
		}
		resolvedCwd, resolveErr := resolvedPath(cwd)
		if resolveErr != nil {
			item.Code, item.ConversationDisposition, item.Reason = 6, DispositionDegraded, "working_directory_unavailable"
			result.Agents = append(result.Agents, item)
			continue
		}
		cwd = resolvedCwd
		policy := cfg.ResumePolicy
		base := PlanRequest{
			Handle: cfg.Handle, ProjectRoot: request.ProjectRoot, Cwd: cwd,
			LaunchNonce: nonce, ResumePolicy: policy, CommittedArgs: cfg.Command[1:], EnvOverlay: cfg.Env,
		}
		conversation, loadErr := LoadConversation(request.Root, cfg.Handle)
		hasConversation := loadErr == nil
		if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
			item.Code, item.ConversationDisposition, item.Reason = 6, DispositionDegraded, "conversation_state_unreadable"
			result.Agents = append(result.Agents, item)
			continue
		}
		forceFresh := request.Fresh || policy == ResumeFresh || policy == ResumeDisabled
		disposition := DispositionFresh
		if policy == ResumeDisabled {
			disposition = DispositionDisabled
		}
		var agentPlan AgentPlan
		var err error
		planReason := ""
		if request.ResumeOnly && !hasConversation {
			item.Code, item.ConversationDisposition, item.Reason = 6, DispositionActionRequired, ReasonNoSavedConversation
			result.Agents = append(result.Agents, item)
			continue
		} else if request.ResumeOnly {
			if conversation.State == CaptureReady {
				agentPlan, err = adapter.PlanResume(ResumeRequest{PlanRequest: base, Conversation: conversation.Identity})
				if err == nil {
					disposition = DispositionResumed
				}
			} else {
				err = fmt.Errorf("conversation identity is %s", conversation.State)
			}
		} else if !forceFresh && hasConversation && conversation.State == CaptureReady {
			agentPlan, err = adapter.PlanResume(ResumeRequest{PlanRequest: base, Conversation: conversation.Identity})
			if err == nil {
				disposition = DispositionResumed
			}
		} else if !forceFresh && hasConversation && conversation.State == CapturePending && adapter.Mode() == AdapterModeMint {
			agentPlan, err = adapter.PlanFresh(base)
			disposition, planReason = DispositionFresh, ReasonPriorLaunchNotExecuted
		} else if !forceFresh && hasConversation {
			err = fmt.Errorf("conversation identity is %s", conversation.State)
		}
		if err != nil && !forceFresh {
			if !request.AllowFreshFallback {
				item.Code, item.ConversationDisposition, item.Reason = 6, DispositionDegraded, ReasonStaleConversation
				result.Agents = append(result.Agents, item)
				continue
			}
			base.ResumePolicy = ResumeFresh
			agentPlan, err = adapter.PlanFresh(base)
			disposition = DispositionFreshAfterStale
		} else if forceFresh || !hasConversation {
			if request.Fresh {
				base.ResumePolicy = ResumeFresh
			}
			agentPlan, err = adapter.PlanFresh(base)
		}
		if err != nil {
			item.Code, item.ConversationDisposition, item.Reason = 6, DispositionDegraded, err.Error()
			result.Agents = append(result.Agents, item)
			continue
		}
		if err := ValidateAdapterPlan(adapter, agentPlan); err != nil {
			item.Code, item.ConversationDisposition, item.Reason = 6, DispositionDegraded, err.Error()
			result.Agents = append(result.Agents, item)
			continue
		}
		item.ConversationDisposition, item.Reason = disposition, planReason
		result.Agents = append(result.Agents, item)
		record := ConversationRecord{
			Version: ConversationVersion, Handle: cfg.Handle, State: CapturePending,
			ProviderVersion: capabilities.ProviderVersion, LaunchNonce: nonce,
		}
		if disposition == DispositionResumed {
			record = conversation
		}
		planned = append(planned, plannedAgent{
			plan: agentPlan, record: record, write: disposition != DispositionResumed,
			index: len(result.Agents) - 1, adapter: adapter, providerVersion: capabilities.ProviderVersion,
		})
	}
	return planned, nil
}

func ensurePlanTrust(request ReconcileRequest, plan Plan, digest string) (bool, error) {
	if request.TrustStore == nil {
		return false, nil
	}
	_, trusted, err := request.TrustStore.LoadForDigest(digest)
	if err != nil {
		return false, err
	}
	if trusted {
		return true, nil
	}
	if request.ConfirmTrust == nil {
		return false, nil
	}
	confirmed, err := request.ConfirmTrust(plan, digest)
	if err != nil || !confirmed {
		return false, err
	}
	return true, request.TrustStore.Replace(TrustRecord{SemanticDigest: digest})
}

func chooseReconcileBackend(request ReconcileRequest, binding BindingRecord, hasBinding, attachSafe bool, result *ReconcileResult) (string, Backend, DetectResult, bool, error) {
	requested := strings.TrimSpace(request.Launcher)
	if requested == "" {
		requested = LauncherAuto
	}
	selected := requested
	if requested == LauncherAuto && hasBinding {
		selected = binding.Backend
	}
	if selected == LauncherAuto {
		preferred, _, _, ok, err := selectPreferredBackend(request)
		if err != nil {
			return "", nil, DetectResult{}, false, err
		}
		if !ok {
			result.Outcome, result.Reason = OutcomeActionRequired, "launcher_not_available"
			return "", nil, DetectResult{}, false, nil
		}
		selected = preferred
	}
	backend := request.Backends[selected]
	if backend == nil {
		result.Outcome, result.Reason = OutcomeActionRequired, "launcher_not_available"
		return selected, nil, DetectResult{}, false, nil
	}
	detect := backend.Detect()
	if err := detect.Validate(); err != nil {
		return selected, nil, detect, false, err
	}
	if !detect.Available {
		result.Outcome, result.Reason = OutcomeActionRequired, "launcher_not_available"
		return selected, backend, detect, false, nil
	}
	if !hasBinding {
		return selected, backend, detect, true, nil
	}
	boundBackend := request.Backends[binding.Backend]
	if boundBackend == nil {
		result.Outcome, result.Reason = OutcomeActionRequired, "bound_launcher_not_available"
		return selected, backend, detect, false, nil
	}
	boundDetect := boundBackend.Detect()
	if err := boundDetect.Validate(); err != nil {
		return selected, backend, detect, false, err
	}
	currentHost := request.HostIdentity
	if currentHost == "" {
		currentHost = boundDetect.HostIdentity
	}
	foreign := currentHost != "" && binding.HostIdentity != currentHost
	if boundDetect.InstanceIdentity != "" && binding.InstanceIdentity != boundDetect.InstanceIdentity {
		foreign = true
	}
	if foreign {
		if !request.Rebind || request.ConfirmRebind == nil {
			result.Outcome, result.Reason = OutcomeActionRequired, "foreign_binding"
			return selected, backend, detect, false, nil
		}
		disposition, confirmed, err := request.ConfirmRebind(binding, true)
		if err != nil {
			return selected, backend, detect, false, err
		}
		if !confirmed || disposition != RebindLeave {
			result.Outcome, result.Reason = OutcomeActionRequired, "foreign_rebind_requires_leave"
			return selected, backend, detect, false, nil
		}
		return selected, backend, detect, true, nil
	}
	inspection, err := boundBackend.Inspect(InspectRequest{Binding: binding, Root: request.Root})
	if err != nil {
		return selected, backend, detect, false, err
	}
	switch inspection.Status {
	case InspectUnknown:
		result.Outcome, result.Reason = OutcomeActionRequired, "inspect_unknown"
		return selected, backend, detect, false, nil
	case InspectPresent:
		if binding.Backend == selected && binding.Profile == detect.Profile.Identity() {
			if !attachSafe {
				result.Outcome, result.Reason = OutcomeActionRequired, "binding_present_without_resumable_conversation"
				return selected, backend, detect, false, nil
			}
			focuser, ok := backend.(BackendFocuser)
			if !ok || !detect.Profile.Has(CapFocus) {
				result.Outcome, result.Reason = OutcomeActionRequired, "bound_launcher_cannot_focus"
				return selected, backend, detect, false, nil
			}
			focused, err := focuser.Focus(FocusRequest{Binding: binding, Root: request.Root})
			if err != nil {
				return selected, backend, detect, false, err
			}
			if focused.Outcome != OutcomeAttached {
				result.Outcome, result.Reason = OutcomeActionRequired, "bound_resource_not_focused"
				return selected, backend, detect, false, nil
			}
			result.Backend, result.Outcome, result.AggregateCode = selected, OutcomeAttached, 0
			return selected, backend, detect, false, nil
		}
		if !request.Rebind || request.ConfirmRebind == nil {
			result.Outcome, result.Reason = OutcomeActionRequired, "present_binding_incompatible"
			return selected, backend, detect, false, nil
		}
		disposition, confirmed, err := request.ConfirmRebind(binding, false)
		if err != nil {
			return selected, backend, detect, false, err
		}
		if !confirmed || (disposition != RebindClose && disposition != RebindLeave) {
			result.Outcome, result.Reason = OutcomeActionRequired, "rebind_not_confirmed"
			return selected, backend, detect, false, nil
		}
		if disposition == RebindClose {
			closed, err := boundBackend.Close(CloseRequest{Binding: binding, Root: request.Root})
			if err != nil {
				return selected, backend, detect, false, err
			}
			if closed.Outcome == OutcomeUnsupported || closed.Outcome == OutcomeActionRequired {
				result.Outcome, result.Reason = OutcomeActionRequired, "bound_resource_not_closed"
				return selected, backend, detect, false, nil
			}
		}
		return selected, backend, detect, true, nil
	case InspectAbsent:
		if requested == LauncherAuto {
			preferredName, preferredBackend, preferredDetect, ok, err := selectPreferredBackend(request)
			if err != nil {
				return selected, backend, detect, false, err
			}
			if !ok {
				result.Outcome, result.Reason = OutcomeActionRequired, "launcher_not_available"
				return selected, backend, detect, false, nil
			}
			selected, backend, detect = preferredName, preferredBackend, preferredDetect
		}
		if requested != LauncherAuto && binding.Backend != selected && !request.Rebind {
			result.Outcome, result.Reason = OutcomeActionRequired, "explicit_rebind_required"
			return selected, backend, detect, false, nil
		}
		if binding.Backend != selected && request.Rebind {
			if request.ConfirmRebind == nil {
				result.Outcome, result.Reason = OutcomeActionRequired, "rebind_confirmation_required"
				return selected, backend, detect, false, nil
			}
			disposition, confirmed, err := request.ConfirmRebind(binding, false)
			if err != nil {
				return selected, backend, detect, false, err
			}
			if !confirmed || (disposition != RebindClose && disposition != RebindLeave) {
				result.Outcome, result.Reason = OutcomeActionRequired, "rebind_not_confirmed"
				return selected, backend, detect, false, nil
			}
		}
		return selected, backend, detect, true, nil
	default:
		return selected, backend, detect, false, fmt.Errorf("backend returned invalid Inspect status %q", inspection.Status)
	}
}

func selectPreferredBackend(request ReconcileRequest) (string, Backend, DetectResult, bool, error) {
	for _, name := range request.Preferences {
		backend := request.Backends[name]
		if backend == nil {
			continue
		}
		detect := backend.Detect()
		if err := detect.Validate(); err != nil {
			return name, backend, detect, false, err
		}
		if detect.Available {
			return name, backend, detect, true, nil
		}
	}
	return "", nil, DetectResult{}, false, nil
}

func loadOptionalBinding(root *fsq.DeliveryRoot) (BindingRecord, bool, error) {
	record, err := LoadBinding(root)
	if errors.Is(err, os.ErrNotExist) {
		return BindingRecord{}, false, nil
	}
	return record, err == nil, err
}

func markPlannedAgents(result *ReconcileResult, planned []plannedAgent, code int, reason string) {
	for _, agent := range planned {
		result.Agents[agent.index].Code = code
		result.Agents[agent.index].Reason = reason
	}
}

func aggregateReconcileCode(agents []AgentReconcileResult) int {
	best := 0
	for _, agent := range agents {
		code := agent.Code
		if agent.ConversationDisposition == DispositionDisabled || agent.ConversationDisposition == DispositionUnsupported || agent.ConversationDisposition == DispositionFresh {
			code = 0
		}
		if code == 6 {
			return 6
		}
		if code == 4 {
			best = 4
		} else if code != 0 && best == 0 {
			best = 1
		}
	}
	return best
}

func firstReconcileReason(agents []AgentReconcileResult, code int) string {
	for _, agent := range agents {
		if agent.Code == code && agent.Reason != "" {
			return agent.Reason
		}
	}
	return ""
}

func callCrashHook(hook func(string) error, stage string) error {
	if hook == nil {
		return nil
	}
	return hook(stage)
}

func reconcileErrorCode(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return 4
	}
	return 1
}
