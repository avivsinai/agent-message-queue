package launchapi

import (
	"context"
	"slices"

	internallaunch "github.com/avivsinai/agent-message-queue/internal/launch"
)

// Apply revalidates a prior Prepare subject under launch authority, consumes
// request-local decisions, provisions the participant roster, and runs the
// existing managed reconciliation crash contract.
func Apply(ctx context.Context, request ApplyRequestV1) (ApplyResultV1, error) {
	if err := request.Validate(); err != nil {
		return ApplyResultV1{}, err
	}
	prepared, dependencies, err := prepareInputs(request.Prepare)
	if err != nil {
		return ApplyResultV1{}, err
	}
	decisions := make([]internallaunch.ApplyDecision, 0, len(request.Decisions))
	for _, decision := range request.Decisions {
		decisions = append(decisions, internallaunch.ApplyDecision{ActionID: decision.ActionID, Choice: string(decision.Choice)})
	}
	result, err := internallaunch.Apply(ctx, internallaunch.ApplyRequest{
		Prepare: prepared, SubjectDigest: request.SubjectDigest, Decisions: decisions,
	}, internallaunch.ApplyDependencies{PrepareDependencies: dependencies})
	if err != nil {
		return ApplyResultV1{}, err
	}
	return fromInternalApplyResult(result), nil
}

func fromInternalApplyResult(result internallaunch.ApplyResult) ApplyResultV1 {
	public := ApplyResultV1{
		ResultVersion: ResultVersionV1, Outcome: result.Outcome, ReasonCode: result.ReasonCode, FailureDetail: result.FailureDetail,
		SubjectDigest: result.SubjectDigest, PlanDigest: result.PlanDigest, TrustDigest: result.TrustDigest,
		SemanticDigest: result.TrustDigest, Backend: result.Backend, Profile: result.Profile,
		Roster: RosterDriftV1{
			Desired: slices.Clone(result.Roster.Desired), Present: slices.Clone(result.Roster.Present),
			Missing: slices.Clone(result.Roster.Missing), Extra: slices.Clone(result.Roster.Extra),
		},
		Observations: make([]ParticipantObservationV1, 0, len(result.Observations)),
		Commands:     make([]CommandV1, 0, len(result.Commands)),
		FollowUps:    make([]RequiredActionV1, 0, len(result.RequiredActions)),
		Evidence:     make([]EvidenceRefV1, 0, len(result.Evidence)),
	}
	for _, evidence := range result.Evidence {
		public.Evidence = append(public.Evidence, fromInternalEvidenceRef(evidence))
	}
	for _, observation := range result.Observations {
		public.Observations = append(public.Observations, ParticipantObservationV1{
			Handle: observation.Handle, Mailbox: observation.Mailbox, Runnable: observation.Runnable,
			Conversation: observation.Conversation, Execution: observation.Execution,
			Resource: observation.Resource, ReasonCode: observation.ReasonCode,
		})
	}
	for _, command := range result.Commands {
		public.Commands = append(public.Commands, CommandV1{
			Argv: slices.Clone(command.Argv), Cwd: command.Cwd, EnvOverlay: cloneStringMap(command.Env),
		})
	}
	for _, action := range result.RequiredActions {
		choices := make([]DecisionChoiceV1, len(action.AllowedDecisions))
		for i, choice := range action.AllowedDecisions {
			choices[i] = DecisionChoiceV1(choice)
		}
		public.FollowUps = append(public.FollowUps, RequiredActionV1{
			ActionID: action.ActionID, Kind: RequiredActionKindV1(action.Kind), Handles: slices.Clone(action.Handles),
			Resources: slices.Clone(action.Resources), AllowedDecisions: choices, ReasonCode: action.ReasonCode,
		})
	}
	return public
}
