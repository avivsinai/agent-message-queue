package launch

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const LifecycleOutcomeInspected = "inspected"

type LifecycleRequest struct{ Target PrepareTarget }

type LifecycleDependencies struct{ Backends map[string]Backend }

type LifecycleResult struct {
	Outcome       string
	ReasonCode    string
	Backend       string
	Profile       string
	State         string
	Observations  []PrepareObservation
	Evidence      []EvidenceRef
	CallerContext map[string]string
}

var beforeLifecycleBackendMutationForTest func()

func InspectLifecycle(ctx context.Context, request LifecycleRequest, dependencies LifecycleDependencies) (LifecycleResult, error) {
	state, err := openLifecycleTarget(ctx, request.Target)
	if err != nil {
		return LifecycleResult{}, err
	}
	defer state.close()
	if state.sessionRoot == nil {
		return LifecycleResult{Outcome: LifecycleOutcomeInspected, State: string(InspectAbsent)}, nil
	}
	binding, err := LoadBinding(state.sessionRoot)
	if errors.Is(err, os.ErrNotExist) {
		return LifecycleResult{Outcome: LifecycleOutcomeInspected, State: string(InspectAbsent), ReasonCode: "binding_missing"}, nil
	}
	if err != nil {
		var contextError *CallerContextValidationError
		if errors.As(err, &contextError) {
			return LifecycleResult{Outcome: string(OutcomeActionRequired), State: string(InspectUnknown), ReasonCode: "caller_context_corrupt"}, nil
		}
		return LifecycleResult{}, err
	}
	backend, detect, refusal, err := validateLifecycleBinding(binding, dependencies, CapInspect)
	if err != nil {
		return LifecycleResult{}, err
	}
	if refusal != "" {
		return lifecycleRefusal(binding, detect, refusal), nil
	}
	inspection, err := backend.Inspect(InspectRequest{Binding: binding, Root: state.sessionRoot})
	if err != nil {
		return LifecycleResult{}, err
	}
	result, err := lifecycleResultFromBinding(state.sessionRoot, binding)
	if err != nil {
		var corrupt *EvidenceCorruptError
		if errors.As(err, &corrupt) {
			return lifecycleRefusal(binding, detect, "evidence_corrupt"), nil
		}
		return LifecycleResult{}, err
	}
	result.Outcome, result.State = LifecycleOutcomeInspected, string(inspection.Status)
	if inspection.Status == InspectUnknown || inspection.ActionRequired {
		result.Outcome, result.ReasonCode = string(OutcomeActionRequired), "inspect_unknown"
	}
	return result, nil
}

func FocusLifecycle(ctx context.Context, request LifecycleRequest, dependencies LifecycleDependencies) (LifecycleResult, error) {
	return mutateLifecycle(ctx, request, dependencies, CapFocus, func(backend Backend, binding BindingRecord, root *fsq.DeliveryRoot) (Outcome, string, error) {
		focuser, ok := backend.(BackendFocuser)
		if !ok {
			return OutcomeUnsupported, "focus_not_supported", nil
		}
		result, err := focuser.Focus(FocusRequest{Binding: binding, Root: root})
		return result.Outcome, result.Reason, err
	})
}

func CloseLifecycle(ctx context.Context, request LifecycleRequest, dependencies LifecycleDependencies) (LifecycleResult, error) {
	return mutateLifecycle(ctx, request, dependencies, CapClose, func(backend Backend, binding BindingRecord, root *fsq.DeliveryRoot) (Outcome, string, error) {
		result, err := backend.Close(CloseRequest{Binding: binding, Root: root})
		return result.Outcome, result.Reason, err
	})
}

func mutateLifecycle(ctx context.Context, request LifecycleRequest, dependencies LifecycleDependencies, capability Capability, operation func(Backend, BindingRecord, *fsq.DeliveryRoot) (Outcome, string, error)) (result LifecycleResult, returnErr error) {
	state, err := openLifecycleTarget(ctx, request.Target)
	if err != nil {
		return LifecycleResult{}, err
	}
	defer state.close()
	if state.sessionRoot == nil {
		return LifecycleResult{Outcome: string(OutcomeActionRequired), State: string(InspectAbsent), ReasonCode: "binding_missing"}, nil
	}
	lease, err := AcquireLease(state.sessionRoot, "")
	if err != nil {
		return LifecycleResult{Outcome: string(OutcomeActionRequired), State: string(InspectUnknown), ReasonCode: "launch_lease_unavailable"}, nil
	}
	defer func() { returnErr = errors.Join(returnErr, lease.Release()) }()
	binding, err := LoadBinding(state.sessionRoot)
	if errors.Is(err, os.ErrNotExist) {
		return LifecycleResult{Outcome: string(OutcomeActionRequired), State: string(InspectAbsent), ReasonCode: "binding_missing"}, nil
	}
	if err != nil {
		var contextError *CallerContextValidationError
		if errors.As(err, &contextError) {
			return LifecycleResult{Outcome: string(OutcomeActionRequired), State: string(InspectUnknown), ReasonCode: "caller_context_corrupt"}, nil
		}
		return LifecycleResult{}, err
	}
	backend, detect, refusal, err := validateLifecycleBinding(binding, dependencies, capability)
	if err != nil {
		return LifecycleResult{}, err
	}
	if refusal != "" {
		return lifecycleRefusal(binding, detect, refusal), nil
	}
	inspection, err := backend.Inspect(InspectRequest{Binding: binding, Root: state.sessionRoot})
	if err != nil {
		return LifecycleResult{}, err
	}
	if inspection.Status != InspectPresent && (capability != CapClose || inspection.Status != InspectAbsent) {
		refusal := "inspect_unknown"
		if inspection.Status == InspectAbsent {
			refusal = "resource_absent"
		}
		return lifecycleRefusal(binding, detect, refusal), nil
	}
	if beforeLifecycleBackendMutationForTest != nil {
		beforeLifecycleBackendMutationForTest()
	}
	if err := state.verify(); err != nil {
		return lifecycleRefusal(binding, detect, "root_changed"), nil
	}
	current, err := LoadBinding(state.sessionRoot)
	if err != nil || !reflect.DeepEqual(current, binding) {
		return lifecycleRefusal(binding, detect, "binding_changed"), nil
	}
	currentBackend, currentDetect, refusal, err := validateLifecycleBinding(current, dependencies, capability)
	if err != nil {
		return LifecycleResult{}, err
	}
	if refusal != "" || currentBackend != backend || !reflect.DeepEqual(currentDetect, detect) {
		if refusal == "" {
			refusal = "backend_changed"
		}
		return lifecycleRefusal(current, currentDetect, refusal), nil
	}
	result, err = lifecycleResultFromBinding(state.sessionRoot, current)
	if err != nil {
		var corrupt *EvidenceCorruptError
		if errors.As(err, &corrupt) {
			return lifecycleRefusal(current, currentDetect, "evidence_corrupt"), nil
		}
		return LifecycleResult{}, err
	}
	outcome, _, err := operation(backend, current, state.sessionRoot)
	if err != nil {
		return LifecycleResult{}, err
	}
	if outcome == OutcomeActionRequired || outcome == OutcomeUnsupported {
		return lifecycleRefusal(current, currentDetect, "backend_refused"), nil
	}
	postMutation, err := backend.Inspect(InspectRequest{Binding: current, Root: state.sessionRoot})
	if err != nil {
		return LifecycleResult{}, err
	}
	result.Outcome = string(outcome)
	result.State = string(postMutation.Status)
	expectedState := InspectPresent
	if capability == CapClose {
		expectedState = InspectAbsent
	}
	if postMutation.Status != expectedState || postMutation.ActionRequired {
		result.Outcome, result.ReasonCode = string(OutcomeActionRequired), "post_mutation_state_unexpected"
	}
	return result, nil
}

func openLifecycleTarget(ctx context.Context, target PrepareTarget) (*prepareTargetState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return openPrepareTarget(target)
}

func validateLifecycleBinding(binding BindingRecord, dependencies LifecycleDependencies, capability Capability) (Backend, DetectResult, string, error) {
	backend := dependencies.Backends[binding.Backend]
	if backend == nil {
		return nil, DetectResult{}, "backend_unknown", nil
	}
	detect := backend.Detect()
	if err := detect.Validate(); err != nil {
		return nil, detect, "backend_profile_invalid", nil
	}
	if binding.Backend != detect.Profile.Backend {
		return backend, detect, "backend_mismatch", nil
	}
	if binding.Profile != detect.Profile.Identity() {
		return backend, detect, "profile_mismatch", nil
	}
	if binding.HostIdentity != detect.HostIdentity || binding.InstanceIdentity != detect.InstanceIdentity {
		return backend, detect, "foreign_binding", nil
	}
	if !detect.Available || !slices.Contains(detect.Effective, capability) {
		return backend, detect, "capability_unavailable", nil
	}
	return backend, detect, "", nil
}

func lifecycleRefusal(binding BindingRecord, detect DetectResult, reason string) LifecycleResult {
	profile := binding.Profile
	if detect.Profile.Backend != "" {
		profile = detect.Profile.Identity()
	}
	return LifecycleResult{Outcome: string(OutcomeActionRequired), ReasonCode: reason, Backend: binding.Backend, Profile: profile, State: string(InspectUnknown)}
}

func lifecycleResultFromBinding(root *fsq.DeliveryRoot, binding BindingRecord) (LifecycleResult, error) {
	resources := make(map[string]string)
	for _, resource := range binding.Resources.Resources {
		if resource.Agent != "" {
			resources[resource.Agent] = resource.OpaqueID
		}
	}
	handles := make([]string, 0, len(resources))
	for handle := range resources {
		handles = append(handles, handle)
	}
	slices.Sort(handles)
	participants := make([]PrepareParticipant, 0, len(handles))
	for _, handle := range handles {
		participants = append(participants, PrepareParticipant{Handle: handle})
	}
	_, mailboxes, err := inspectPrepareRoster(root, nil, participants)
	if err != nil {
		return LifecycleResult{}, err
	}
	observations := make([]PrepareObservation, 0, len(handles))
	for _, handle := range handles {
		observation := PrepareObservation{
			Handle: handle, Mailbox: mailboxes[handle], Conversation: "none", Execution: "none", Resource: resources[handle],
			ConversationIdentityDigest: "absent", ExecutionIdentityDigest: "absent",
		}
		if conversation, err := LoadConversation(root, handle); err == nil {
			observation.Conversation = string(conversation.State)
			observation.ConversationIdentityDigest, err = digestCanonical(conversation)
			if err != nil {
				return LifecycleResult{}, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return LifecycleResult{}, err
		}
		if ticket, err := LoadExecutionTicket(root, handle); err == nil {
			observation.Execution = string(ticket.State)
			observation.ExecutionIdentityDigest, err = digestCanonical(ticket)
			if err != nil {
				return LifecycleResult{}, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return LifecycleResult{}, err
		}
		observations = append(observations, observation)
	}
	evidence, err := CollectEvidenceRefs(root, handles)
	if err != nil {
		return LifecycleResult{}, err
	}
	return LifecycleResult{Backend: binding.Backend, Profile: binding.Profile, Observations: observations, Evidence: evidence, CallerContext: cloneCallerContext(binding.CallerContext)}, nil
}
