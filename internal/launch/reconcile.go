package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	Reason                  string                  `json:"reason,omitempty"`
}

type ReconcileResult struct {
	Session        string                 `json:"session"`
	Backend        string                 `json:"backend,omitempty"`
	Outcome        Outcome                `json:"outcome,omitempty"`
	AggregateCode  int                    `json:"aggregate_code"`
	Reason         string                 `json:"reason,omitempty"`
	Agents         []AgentReconcileResult `json:"agents"`
	Commands       []EmittedCommand       `json:"commands,omitempty"`
	Plan           *Plan                  `json:"plan,omitempty"`
	SemanticDigest string                 `json:"semantic_digest,omitempty"`
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
	result = ReconcileResult{Session: request.Session, Agents: []AgentReconcileResult{}}
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
	nonce, err := generateNonce()
	if err != nil {
		return result, err
	}
	planned, err := buildReconcilePlan(request, nonce, &result)
	if err != nil {
		return result, err
	}
	if len(planned) == 0 {
		result.AggregateCode = aggregateReconcileCode(result.Agents)
		return result, nil
	}
	plan := Plan{Version: PlanVersion, Agents: make([]AgentPlan, 0, len(planned))}
	for _, agent := range planned {
		plan.Agents = append(plan.Agents, agent.plan)
	}
	digest, err := plan.SemanticDigest()
	if err != nil {
		return result, err
	}
	result.Plan, result.SemanticDigest = &plan, digest
	trusted, err := ensurePlanTrust(request, plan, digest)
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
	attachSafe := true
	for _, agent := range planned {
		attachSafe = attachSafe && !agent.write
	}
	backendName, backend, detect, proceed, err := chooseReconcileBackend(request, binding, hasBinding, attachSafe, &result)
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
	result.Backend = backendName

	// Commands is the only Wave A backend and Create only emits an idempotent
	// plan. Before Wave B adds its first managed backend, this boundary needs a
	// durable recovery journal for a resource created before its binding write.
	create, err := backend.Create(CreateRequest{Session: request.Session, Plan: plan, AMQPath: request.AMQPath, Root: request.Root})
	if err != nil {
		code := reconcileErrorCode(err)
		markPlannedAgents(&result, planned, code, "backend_create_failed")
		result.AggregateCode, result.Reason = code, err.Error()
		return result, nil
	}
	result.Outcome, result.Commands = create.Outcome, create.Commands
	if err := callCrashHook(request.CrashHook, "backend_created"); err != nil {
		return result, err
	}
	for i := range planned {
		agent := &planned[i]
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
		if agent.write {
			if err := WriteConversation(request.Root, lease, agent.record); err != nil {
				return result, err
			}
		}
	}
	if err := callCrashHook(request.CrashHook, "conversations_written"); err != nil {
		return result, err
	}
	if create.Outcome == OutcomeCreated {
		candidate := create.Binding
		if candidate.Backend != backendName || candidate.Profile != detect.Profile.Identity() || candidate.LaunchNonce != nonce {
			return result, fmt.Errorf("backend returned a binding outside the selected launch generation")
		}
		if err := WriteBinding(request.Root, lease, candidate); err != nil {
			return result, err
		}
	}
	if create.ActionRequired || create.Outcome == OutcomeCommandsEmitted || create.Outcome == OutcomeActionRequired {
		for _, agent := range planned {
			if result.Agents[agent.index].Code == 0 {
				result.Agents[agent.index].Code = 6
				result.Agents[agent.index].Reason = "commands_emitted"
			}
		}
		result.AggregateCode = 6
		result.Reason = "execute the emitted commands to complete launch"
		return result, nil
	}
	result.AggregateCode = aggregateReconcileCode(result.Agents)
	return result, nil
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
		if !forceFresh && hasConversation && conversation.State == CaptureReady {
			agentPlan, err = adapter.PlanResume(ResumeRequest{PlanRequest: base, Conversation: conversation.Identity})
			if err == nil {
				disposition = DispositionResumed
			}
		} else if !forceFresh && (request.ResumeOnly || hasConversation) {
			err = fmt.Errorf("conversation identity is %s", conversation.State)
		}
		if err != nil && !forceFresh {
			if !request.AllowFreshFallback {
				item.Code, item.ConversationDisposition, item.Reason = 6, DispositionDegraded, "stale_conversation"
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
		item.ConversationDisposition = disposition
		result.Agents = append(result.Agents, item)
		record := ConversationRecord{
			Version: ConversationVersion, Handle: cfg.Handle, State: CapturePending,
			ProviderVersion: capabilities.ProviderVersion, LaunchNonce: nonce,
		}
		if agentPlan.AdapterMode == AdapterModeMint {
			record.State = CaptureReady
			record.Identity = ConversationIdentity{Provider: adapter.Name(), ID: agentPlan.ConversationID}
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
