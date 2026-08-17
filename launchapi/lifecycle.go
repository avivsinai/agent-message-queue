package launchapi

import (
	"context"

	internallaunch "github.com/avivsinai/agent-message-queue/internal/launch"
)

func Inspect(ctx context.Context, request InspectRequestV1) (InspectResultV1, error) {
	if err := request.validate(); err != nil {
		return InspectResultV1{}, err
	}
	result, err := internallaunch.InspectLifecycle(ctx, lifecycleRequest(request), lifecycleDependencies())
	if err != nil {
		return InspectResultV1{}, err
	}
	return lifecycleResult(result), nil
}

func Focus(ctx context.Context, request FocusRequestV1) (FocusResultV1, error) {
	if err := request.validate(); err != nil {
		return FocusResultV1{}, err
	}
	result, err := internallaunch.FocusLifecycle(ctx, lifecycleRequest(request), lifecycleDependencies())
	if err != nil {
		return FocusResultV1{}, err
	}
	return lifecycleResult(result), nil
}

func Close(ctx context.Context, request CloseRequestV1) (CloseResultV1, error) {
	if err := request.validate(); err != nil {
		return CloseResultV1{}, err
	}
	result, err := internallaunch.CloseLifecycle(ctx, lifecycleRequest(request), lifecycleDependencies())
	if err != nil {
		return CloseResultV1{}, err
	}
	return lifecycleResult(result), nil
}

func lifecycleRequest(request InspectRequestV1) internallaunch.LifecycleRequest {
	return internallaunch.LifecycleRequest{Target: internallaunch.PrepareTarget{
		ProjectRoot: request.Target.ProjectRoot, SessionRoot: request.Target.SessionRoot, Session: request.Target.Session,
	}}
}

func lifecycleDependencies() internallaunch.LifecycleDependencies {
	return internallaunch.LifecycleDependencies{Backends: lifecycleBackends()}
}

var lifecycleBackends = internallaunch.DefaultBackends

func lifecycleResult(result internallaunch.LifecycleResult) LifecycleResultV1 {
	public := LifecycleResultV1{
		ResultVersion: ResultVersionV1, Outcome: result.Outcome, ReasonCode: result.ReasonCode,
		Backend: result.Backend, Profile: result.Profile, State: result.State,
		Observations: make([]ParticipantObservationV1, 0, len(result.Observations)),
		Evidence:     make([]EvidenceRefV1, 0, len(result.Evidence)),
	}
	for _, observation := range result.Observations {
		public.Observations = append(public.Observations, ParticipantObservationV1{
			Handle: observation.Handle, Mailbox: observation.Mailbox, Runnable: observation.Runnable,
			Conversation: observation.Conversation, Execution: observation.Execution,
			Resource: observation.Resource, ReasonCode: observation.ReasonCode,
		})
	}
	for _, evidence := range result.Evidence {
		public.Evidence = append(public.Evidence, fromInternalEvidenceRef(evidence))
	}
	return public
}

func fromInternalEvidenceRef(evidence internallaunch.EvidenceRef) EvidenceRefV1 {
	return EvidenceRefV1{
		EvidenceVersion: evidence.EvidenceVersion, ID: evidence.ID, Kind: string(evidence.Kind),
		SHA256: evidence.SHA256, ObservedAt: evidence.ObservedAt,
	}
}
