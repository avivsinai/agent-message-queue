package launch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	ApplyOutcomeApplied                             = "applied"
	ApplyOutcomeActionRequired                      = "action_required"
	ApplyOutcomeProvisionedNoRunnable               = "provisioned_no_runnable"
	ApplyReasonPrepareActionRequiredWithoutDecision = "prepare_action_required_without_decision"
)

type MutationDisposition string

const (
	MutationNotApplied MutationDisposition = "not_applied"
	MutationCommitted  MutationDisposition = "committed"
	MutationUncertain  MutationDisposition = "uncertain"
)

type ApplyDecision struct {
	ActionID string
	Choice   string
}

type ApplyRequest struct {
	Prepare       PrepareRequest
	SubjectDigest string
	Decisions     []ApplyDecision
}

type ApplyDependencies struct {
	PrepareDependencies
	AMQPath   string
	CrashHook func(string) error
}

type ApplyResult struct {
	Outcome           string
	ReasonCode        string
	FailureDetail     string
	SubjectSchema     int
	SubjectDigest     string
	PlanDigest        string
	TrustDigest       string
	Backend           string
	Profile           string
	Disposition       MutationDisposition
	BindingGeneration string
	Roster            PrepareRoster
	Observations      []PrepareObservation
	Commands          []EmittedCommand
	RequiredActions   []PrepareRequiredAction
	Evidence          []EvidenceRef
	CallerContext     map[string]string
}

var afterApplySessionPublishedForTest func()
var beforeApplySessionPublishForTest func()
var beforeApplyAuthorizeApplyForTest func()
var beforeApplyReconcileForTest func()
var afterApplyReconcileForTest func()
var openPrepareTargetForApply = openPrepareTarget
var beforeApplyRosterMutationForTest func()
var beforeApplyBaseRootCreateForTest func()
var afterApplyBaseRootCreatedForTest func() error
var beforeApplyFinalPrepareForTest func()
var collectApplyEvidenceRefs = CollectEvidenceRefs
var publishApplySession = func(base *fsq.DeliveryRoot, session string, initialize func(*fsq.DeliveryRoot) error) (*fsq.DeliveryRoot, error) {
	return base.PublishInitializedDirectChildExclusive(session, 0o700, initialize)
}
var writeApplyConfigFile = func(root *fsq.DeliveryRoot, data []byte) error {
	_, err := root.WriteFileAtomic("meta", "config.json", data, 0o600)
	return err
}

// Apply retains the same authority from its re-Prepare through roster and
// launch mutation. Stable authority-lock inodes are substrate; all decision,
// trust, roster, journal, ticket, binding, and backend state remains untouched
// until the exact subject and decisions are accepted.
func Apply(ctx context.Context, request ApplyRequest, dependencies ApplyDependencies) (result ApplyResult, returnErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}
	if !validDigest(request.SubjectDigest) {
		return ApplyResult{}, fmt.Errorf("invalid Apply subject digest")
	}
	state, err := openPrepareTargetForApply(request.Prepare.Target)
	if err != nil {
		return ApplyResult{}, err
	}
	defer state.close()

	if state.baseRoot == nil {
		return applyMissingBaseRoot(ctx, request, dependencies, state)
	}
	if state.sessionRoot == nil {
		err = WithSessionCreationLock(state.baseRoot, state.target.Session, func() error {
			result, err = applyUnderAuthority(ctx, request, dependencies, state, nil)
			return err
		})
		return result, err
	}

	nonce := ""
	if journal, present, loadErr := loadOptionalJournal(state.sessionRoot); loadErr != nil {
		return ApplyResult{}, loadErr
	} else if present {
		nonce = journal.LaunchNonce
	} else if !applyRequestsRebind(request) {
		binding, hasBinding, bindErr := loadOptionalBinding(state.sessionRoot)
		if bindErr != nil {
			return ApplyResult{}, bindErr
		}
		if hasBinding {
			nonce = binding.LaunchNonce
		}
	}
	lease, err := acquireExistingApplyLease(state, nonce)
	if err != nil {
		current, prepareErr := Prepare(ctx, request.Prepare, dependencies.PrepareDependencies)
		if prepareErr != nil {
			return ApplyResult{}, prepareErr
		}
		return applyActionResult(current, "launch_lease_unavailable"), nil
	}
	defer func() { returnErr = errors.Join(returnErr, lease.Release()) }()
	return applyUnderAuthority(ctx, request, dependencies, state, lease)
}

func applyUnderAuthority(ctx context.Context, request ApplyRequest, dependencies ApplyDependencies, state *prepareTargetState, lease *Lease) (result ApplyResult, returnErr error) {
	if err := state.verify(); err != nil {
		if lease != nil {
			lease.abandonCapability()
		}
		if !isPrepareAuthorityDrift(err) {
			return ApplyResult{}, err
		}
		return applyActionResult(PrepareResult{}, "subject_changed"), nil
	}
	if beforeApplyAuthorizeApplyForTest != nil {
		beforeApplyAuthorizeApplyForTest()
	}
	prepared, decisions, refusal, allowed, err := authorizeApply(ctx, request, dependencies, state)
	if err != nil || !allowed {
		if !allowed && lease != nil {
			if driftErr := state.verify(); isPrepareAuthorityDrift(driftErr) {
				lease.abandonCapability()
			}
		}
		return refusal, err
	}
	return applyPreparedUnderAuthority(ctx, request, dependencies, state, lease, prepared, decisions)
}

func authorizeApply(ctx context.Context, request ApplyRequest, dependencies ApplyDependencies, state *prepareTargetState) (PrepareResult, map[RequiredActionKind]string, ApplyResult, bool, error) {
	ctx, subjectSchema, prepareDependencies, err := validatePrepareRequest(ctx, request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		return PrepareResult{}, nil, ApplyResult{}, false, err
	}
	prepared, err := prepareAtTarget(ctx, request.Prepare, prepareDependencies, state, subjectSchema)
	if err != nil {
		if isPrepareAuthorityDrift(err) {
			refusal := applyActionResult(PrepareResult{SubjectSchema: subjectSchema}, "subject_changed")
			refusal.SubjectDigest = request.SubjectDigest
			return PrepareResult{}, nil, refusal, false, nil
		}
		return PrepareResult{}, nil, ApplyResult{}, false, err
	}
	if request.Prepare.SubjectSchema == SubjectSchemaV1 && hasRunnableParticipants(request.Prepare.Participants) {
		return prepared, nil, applyActionResult(prepared, "reprepare_required"), false, nil
	}
	if prepared.SubjectDigest != request.SubjectDigest {
		return prepared, nil, applyActionResult(prepared, "subject_changed"), false, nil
	}
	if prepared.Outcome == PrepareOutcomeUnsupported {
		return prepared, nil, applyActionResult(prepared, prepared.Reason), false, nil
	}
	if prepared.Outcome == PrepareOutcomeActionRequired && len(prepared.RequiredActions) == 0 {
		return prepared, nil, applyActionResult(prepared, ApplyReasonPrepareActionRequiredWithoutDecision), false, nil
	}
	decisions, reason := validateApplyDecisions(prepared.RequiredActions, request.Decisions)
	if reason != "" {
		return prepared, nil, applyActionResult(prepared, reason), false, nil
	}
	return prepared, decisions, ApplyResult{}, true, nil
}

func hasRunnableParticipants(participants []PrepareParticipant) bool {
	for _, participant := range participants {
		if participant.Runnable {
			return true
		}
	}
	return false
}

func applyPreparedUnderAuthority(ctx context.Context, request ApplyRequest, dependencies ApplyDependencies, state *prepareTargetState, lease *Lease, prepared PrepareResult, decisions map[RequiredActionKind]string) (result ApplyResult, returnErr error) {
	var err error
	handles := make([]string, 0, len(request.Prepare.Participants))
	for _, participant := range request.Prepare.Participants {
		handles = append(handles, participant.Handle)
	}
	slices.Sort(handles)

	root := state.sessionRoot
	if root == nil {
		var stagedLease *Lease
		root, err = publishApplySession(state.baseRoot, state.target.Session, func(staging *fsq.DeliveryRoot) error {
			if err := initializeApplySession(staging, handles); err != nil {
				return err
			}
			stagedLease, err = AcquireLease(staging, "")
			if err == nil && beforeApplySessionPublishForTest != nil {
				beforeApplySessionPublishForTest()
			}
			return err
		})
		if err != nil {
			var committed *fsq.CommittedDurabilityError
			if errors.As(err, &committed) && root != nil && stagedLease != nil {
				result := applyActionResult(prepared, "session_publication_durability_uncertain")
				result.SubjectDigest = request.SubjectDigest
				if verifyErr := root.VerifyBase(); verifyErr != nil {
					stagedLease.abandonCapability()
					_ = root.Close()
					return result, nil
				}
				defer func() { _ = root.Close() }()
				lease = stagedLease
				defer func() { returnErr = errors.Join(returnErr, lease.Release()) }()
				return result, nil
			}
			if stagedLease != nil {
				if root != nil {
					_ = stagedLease.Release()
					_ = root.Close()
				} else {
					stagedLease.abandonCapability()
				}
			}
			var exists *fsq.DirectChildExistsError
			if errors.As(err, &exists) {
				current, prepareErr := Prepare(ctx, request.Prepare, dependencies.PrepareDependencies)
				if prepareErr != nil {
					return ApplyResult{}, prepareErr
				}
				return applyActionResult(current, "subject_changed"), nil
			}
			return ApplyResult{}, err
		}
		state.sessionRoot = root
		if afterApplySessionPublishedForTest != nil {
			afterApplySessionPublishedForTest()
		}
		lease = stagedLease
		defer func() { returnErr = errors.Join(returnErr, lease.Release()) }()
	} else {
		if err := provisionExistingApplyRoster(root, lease, handles); err != nil {
			var committed *applyCommittedMutationError
			if errors.As(err, &committed) {
				current, prepareErr := Prepare(ctx, request.Prepare, dependencies.PrepareDependencies)
				if prepareErr != nil {
					return ApplyResult{}, errors.Join(err, prepareErr)
				}
				result := applyActionResult(current, committed.Reason)
				result.SubjectDigest = request.SubjectDigest
				return result, nil
			}
			return ApplyResult{}, err
		}
	}

	runnable := make([]PrepareParticipant, 0, len(request.Prepare.Participants))
	for _, participant := range request.Prepare.Participants {
		if participant.Runnable {
			runnable = append(runnable, participant)
		}
	}
	if len(runnable) == 0 {
		prepared, err = prepareApplyStatus(ctx, request.Prepare, dependencies.PrepareDependencies, state)
		if err != nil {
			if isPrepareAuthorityDrift(err) {
				return applyActionResult(prepared, "subject_changed"), nil
			}
			return ApplyResult{}, err
		}
		result := applyResultFromPrepare(prepared)
		result.SubjectDigest = request.SubjectDigest
		result.Outcome = ApplyOutcomeProvisionedNoRunnable
		result.ReasonCode = ""
		result.RequiredActions = nil
		return result, nil
	}

	reconcileRequest, err := buildApplyReconcileRequest(ctx, request.Prepare, runnable, dependencies, root, lease, decisions, prepared, state.sessionIdentity)
	if err != nil {
		return ApplyResult{}, err
	}
	if beforeApplyReconcileForTest != nil {
		beforeApplyReconcileForTest()
	}
	reconciled, err := Reconcile(reconcileRequest)
	if err != nil {
		result := applyActionResult(prepared, "launch_reconcile_failed")
		result.FailureDetail = err.Error()
		return classifyApplyMutation(result, root), nil
	}
	if beforeApplyFinalPrepareForTest != nil {
		beforeApplyFinalPrepareForTest()
	}
	if afterApplyReconcileForTest != nil {
		afterApplyReconcileForTest()
	}
	finalPrepared, err := prepareApplyStatus(ctx, request.Prepare, dependencies.PrepareDependencies, state)
	if err != nil {
		if isPrepareAuthorityDrift(err) {
			if lease != nil {
				lease.abandonCapability()
			}
			return applyActionResult(prepared, "subject_changed"), nil
		}
		return applyPostCommitFailure(applyResultFromPrepare(prepared), request, root, reconciled, "post_commit_prepare_failed", err), nil
	}
	if reconciled.AggregateCode == 0 && reconciled.Outcome != "" && reconciled.Outcome != OutcomeNoAction && !authorizedIdentitiesEqual(prepared, finalPrepared) {
		return applyActionResult(finalPrepared, "subject_changed"), nil
	}
	result = applyResultFromPrepare(finalPrepared)
	result.SubjectDigest = request.SubjectDigest
	result.RequiredActions = nil
	result.Commands = slices.Clone(reconciled.Commands)
	result.Backend, result.TrustDigest = reconciled.Backend, reconciled.SemanticDigest
	classified := classifyApplyMutation(result, root)
	result.Disposition, result.BindingGeneration = classified.Disposition, classified.BindingGeneration
	result.Evidence, err = collectApplyEvidenceRefs(root, handles)
	if err != nil {
		return applyPostCommitFailure(result, request, root, reconciled, "post_commit_evidence_failed", err), nil
	}
	if reconciled.AggregateCode == 0 && reconciled.Outcome != "" && reconciled.Outcome != OutcomeNoAction {
		result.Outcome = ApplyOutcomeApplied
		result.ReasonCode = ""
	} else {
		result.Outcome = ApplyOutcomeActionRequired
		result.ReasonCode = applyReconcileReasonCode(reconciled)
		result.FailureDetail = reconciled.Reason
	}
	stampApplySeatDispositions(&result, reconciled.Seats)
	return result, nil
}

func prepareApplyStatus(ctx context.Context, request PrepareRequest, dependencies PrepareDependencies, state *prepareTargetState) (PrepareResult, error) {
	ctx, subjectSchema, dependencies, err := validatePrepareRequest(ctx, request, dependencies)
	if err != nil {
		return PrepareResult{}, err
	}
	return prepareAtTarget(ctx, request, dependencies, state, subjectSchema)
}

func applyMissingBaseRoot(ctx context.Context, request ApplyRequest, dependencies ApplyDependencies, state *prepareTargetState) (result ApplyResult, returnErr error) {
	if state.baseAuthority == nil || !state.baseAuthority.baseMissing || state.baseAuthority.parentRoot == nil {
		return ApplyResult{}, fmt.Errorf("missing base-root creation authority")
	}
	prepared, decisions, refusal, allowed, err := authorizeApply(ctx, request, dependencies, state)
	if err != nil || !allowed {
		return refusal, err
	}
	if beforeApplyBaseRootCreateForTest != nil {
		beforeApplyBaseRootCreateForTest()
	}
	if err := state.verify(); err != nil {
		return applyActionResult(prepared, "subject_changed"), nil
	}
	baseRoot, err := state.baseAuthority.parentRoot.CreateDirectChildExclusive(state.baseAuthority.baseName, 0o700)
	if err != nil {
		var committed *fsq.CommittedDurabilityError
		if errors.As(err, &committed) && baseRoot != nil {
			state.baseRoot = baseRoot
			return applyActionResult(prepared, "base_root_creation_durability_uncertain"), nil
		}
		var exists *fsq.DirectChildExistsError
		if errors.As(err, &exists) {
			return applyActionResult(prepared, "subject_changed"), nil
		}
		return ApplyResult{}, err
	}
	state.baseRoot = baseRoot
	state.baseAuthority.baseMissing = false
	if afterApplyBaseRootCreatedForTest != nil {
		if err := afterApplyBaseRootCreatedForTest(); err != nil {
			return ApplyResult{}, err
		}
	}
	err = WithSessionCreationLock(baseRoot, state.target.Session, func() error {
		result, err = applyPreparedUnderAuthority(ctx, request, dependencies, state, nil, prepared, decisions)
		return err
	})
	return result, err
}

func applyReconcileReasonCode(result ReconcileResult) string {
	for _, agent := range result.Agents {
		if agent.Code != 0 && agent.Reason != "" {
			return agent.Reason
		}
	}
	if len(result.Commands) > 0 {
		return "commands_emitted"
	}
	if result.Reason != "" {
		return result.Reason
	}
	return "launch_action_required"
}

func validateApplyDecisions(actions []PrepareRequiredAction, supplied []ApplyDecision) (map[RequiredActionKind]string, string) {
	if len(actions) != len(supplied) {
		return nil, "decisions_incomplete"
	}
	actionsByID := make(map[string]PrepareRequiredAction, len(actions))
	for _, action := range actions {
		actionsByID[action.ActionID] = action
	}
	result := make(map[RequiredActionKind]string, len(actions))
	seen := make(map[string]struct{}, len(supplied))
	for _, decision := range supplied {
		action, ok := actionsByID[decision.ActionID]
		if !ok {
			return nil, "decision_unknown_action"
		}
		if _, duplicate := seen[decision.ActionID]; duplicate {
			return nil, "decision_duplicate"
		}
		seen[decision.ActionID] = struct{}{}
		if !slices.Contains(action.AllowedDecisions, decision.Choice) {
			return nil, "decision_disallowed"
		}
		if decision.Choice == "deny" || decision.Choice == "abort" {
			return nil, "decision_declined"
		}
		result[action.Kind] = decision.Choice
	}
	return result, ""
}

func initializeApplySession(root *fsq.DeliveryRoot, handles []string) error {
	if err := root.EnsureRootDirs(); err != nil {
		return err
	}
	config := struct {
		Version    int      `json:"version"`
		CreatedUTC string   `json:"created_utc"`
		Agents     []string `json:"agents"`
	}{Version: 1, CreatedUTC: time.Now().UTC().Format(time.RFC3339), Agents: slices.Clone(handles)}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := root.WriteFileAtomic("meta", "config.json", data, 0o600); err != nil {
		return err
	}
	for _, handle := range handles {
		if err := root.EnsureAgentDirs(handle); err != nil {
			return fmt.Errorf("provision mailbox %s: %w", handle, err)
		}
	}
	return nil
}

func provisionExistingApplyRoster(root *fsq.DeliveryRoot, lease *Lease, handles []string) error {
	if err := lease.authorizeWrite(root); err != nil {
		return fmt.Errorf("authorize Apply roster write: %w", err)
	}
	authorization, inventory, err := fsq.OpenMailboxConfigAuthorization(root)
	if err != nil {
		if inventory.ActiveConfigStatus != string(fsq.MailboxPathMissing) {
			return err
		}
		roster, discoverErr := discoveredApplyRoster(root, handles)
		if discoverErr != nil {
			return discoverErr
		}
		if err := writeApplySessionConfig(root, lease, time.Now().UTC().Format(time.RFC3339), roster); err != nil {
			return err
		}
		authorization, _, err = fsq.OpenMailboxConfigAuthorization(root)
		if err != nil {
			return err
		}
	}
	defer func() { _ = authorization.Close() }()
	effective := append(authorization.ConfiguredAgents(), handles...)
	slices.Sort(effective)
	effective = slices.Compact(effective)
	if beforeApplyRosterMutationForTest != nil {
		beforeApplyRosterMutationForTest()
	}
	// Repair only the requested roster. Configured/discovered extras and all of
	// their history are observations, never mutation targets for Apply.
	repaired := fsq.RepairMailboxLayoutForAgentsWithAuthorizationAndWriteGuard(root, authorization, handles, func() error {
		return lease.authorizeWrite(root)
	})
	if repaired.Status == "failed" {
		return fmt.Errorf("provision Apply roster: %s", repaired.Failure.Message)
	}
	if !slices.Equal(effective, authorization.ConfiguredAgents()) {
		var current struct {
			CreatedUTC string `json:"created_utc"`
		}
		data, readErr := root.ReadFile(filepath.Join("meta", "config.json"))
		if readErr != nil {
			return readErr
		}
		if err := json.Unmarshal(data, &current); err != nil {
			return err
		}
		if err := authorization.Verify(); err != nil {
			return err
		}
		if err := writeApplySessionConfig(root, lease, current.CreatedUTC, effective); err != nil {
			return err
		}
	}
	return nil
}

func discoveredApplyRoster(root *fsq.DeliveryRoot, requested []string) ([]string, error) {
	roster := slices.Clone(requested)
	entries, err := root.ReadDir("agents")
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return nil, fmt.Errorf("unsafe existing mailbox entry %q", entry.Name())
		}
		if err := fsq.ValidateHandle(entry.Name()); err != nil {
			return nil, fmt.Errorf("invalid existing mailbox entry %q: %w", entry.Name(), err)
		}
		roster = append(roster, entry.Name())
	}
	slices.Sort(roster)
	return slices.Compact(roster), nil
}

func writeApplySessionConfig(root *fsq.DeliveryRoot, lease *Lease, createdUTC string, handles []string) error {
	if createdUTC == "" {
		createdUTC = time.Now().UTC().Format(time.RFC3339)
	}
	config := struct {
		Version    int      `json:"version"`
		CreatedUTC string   `json:"created_utc"`
		Agents     []string `json:"agents"`
	}{Version: 1, CreatedUTC: createdUTC, Agents: slices.Clone(handles)}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if err := lease.authorizeWrite(root); err != nil {
		return err
	}
	err = writeApplyConfigFile(root, append(data, '\n'))
	if err != nil {
		var committed *fsq.CommittedDurabilityError
		if errors.As(err, &committed) {
			return &applyCommittedMutationError{Reason: "roster_config_durability_uncertain", Err: err}
		}
	}
	return err
}

type applyCommittedMutationError struct {
	Reason string
	Err    error
}

func (err *applyCommittedMutationError) Error() string { return err.Err.Error() }
func (err *applyCommittedMutationError) Unwrap() error { return err.Err }

func buildApplyReconcileRequest(ctx context.Context, prepared PrepareRequest, runnable []PrepareParticipant, dependencies ApplyDependencies, root *fsq.DeliveryRoot, lease *Lease, decisions map[RequiredActionKind]string, snapshot PrepareResult, authorizedRootIdentity string) (ReconcileRequest, error) {
	config := ProjectConfig{Schema: ProjectConfigSchema, DefaultSession: prepared.Target.Session, Layout: LayoutIntent{Type: LayoutColumns}}
	adapters := make(map[string]HarnessAdapter, len(runnable))
	executionOptions := make(map[string]PrepareExecutionOptions, len(runnable))
	authorizedIdentities := make(map[string]AuthorizedParticipantIdentity, len(snapshot.Participants))
	for _, participant := range snapshot.Participants {
		if !participant.Runnable || (participant.Executable == nil && participant.Wrapper == nil && participant.CwdIdentity == "") {
			continue
		}
		authorizedIdentities[participant.Handle] = AuthorizedParticipantIdentity{
			Executable:  cloneConsultedExecutable(participant.Executable),
			Wrapper:     cloneConsultedExecutable(participant.Wrapper),
			CwdIdentity: participant.CwdIdentity,
		}
	}
	for _, participant := range runnable {
		adapter := dependencies.AdapterFor(participant.Provider, participant.Executable)
		if adapter == nil {
			return ReconcileRequest{}, fmt.Errorf("provider %q has no adapter", participant.Provider)
		}
		key := participant.Executable
		command := append([]string{participant.Executable}, participant.Args...)
		command = append(command, participant.BypassArgs...)
		var initial *InitialInputRequest
		if participant.InitialInput != nil {
			digest := initialInputDigest(participant.InitialInput.Text)
			initial = &InitialInputRequest{Kind: participant.InitialInput.Kind, Value: participant.InitialInput.Text, SHA256: digest}
		}
		named := participant.Execution.Named
		config.Agents = append(config.Agents, ProjectAgentConfig{
			Handle: participant.Handle, Adapter: key, Command: command, Env: cloneEnv(participant.EnvOverlay),
			Named: &named, Cwd: participant.Cwd, ResumePolicy: participant.ResumePolicy, InitialInput: initial, Wrapper: cloneWrapper(participant.Wrapper),
		})
		adapters[key] = adapter
		executionOptions[participant.Handle] = *clonePrepareExecutionOptions(&participant.Execution)
	}
	if err := config.Validate(); err != nil {
		return ReconcileRequest{}, err
	}
	request := ReconcileRequest{
		Context: ctx, ProjectRoot: prepared.Target.ProjectRoot, Session: prepared.Target.Session,
		AMQPath: dependencies.AMQPath, Root: root, Config: config, Launcher: prepared.Launcher,
		Preferences: slices.Clone(dependencies.Preferences), Backends: dependencies.Backends,
		Adapters: adapters, TrustStore: dependencies.TrustStore, HeldLease: lease,
		CrashHook:            dependencies.CrashHook,
		TrustAuthorityDigest: snapshot.BaseAuthorityDigest,
		HostIdentity:         dependencies.HostIdentity, AllowExternalCwd: true,
		ExecutionOptions:     executionOptions,
		AuthorizedIdentities: authorizedIdentities,
		AllowFreshFallback:   decisions[ActionStaleConversation] == "fresh_once",
		Placement:            prepared.Placement,
		OnLive:               onLivePolicies(runnable),
		CallerContext:        cloneCallerContext(prepared.CallerContext),
	}
	if choice := decisions[ActionTrustConfirmation]; choice == "trust_exact_subject" {
		request.ConfirmTrust = func(plan Plan, actualTrustDigest string) (bool, error) {
			planDigest, err := plan.SemanticDigest()
			if err != nil {
				return false, err
			}
			if planDigest != snapshot.PlanDigest {
				return false, fmt.Errorf("static plan changed after Apply authorization")
			}
			trustPlanDigest, err := plan.TrustSemanticDigest()
			if err != nil {
				return false, err
			}
			authorizedTrustDigest, err := PrepareTrustDigestWithAuthority(
				trustPlanDigest, prepared.Target.Session, prepared.Target.SessionRoot, authorizedRootIdentity, snapshot.BaseAuthorityDigest,
				onLiveKeepHandles(prepared.Participants),
			)
			if err != nil {
				return false, err
			}
			if authorizedTrustDigest != snapshot.TrustDigest {
				return false, fmt.Errorf("authorized trust subject changed after Apply authorization")
			}
			expectedActual, err := ExecutionTrustDigestWithAuthority(plan, prepared.Target.Session, root, snapshot.BaseAuthorityDigest)
			if err != nil {
				return false, err
			}
			if actualTrustDigest != expectedActual {
				return false, fmt.Errorf("execution trust subject changed after session publication")
			}
			return true, nil
		}
	}
	if choice := decisions[ActionRebindConfirmation]; choice != "" {
		request.Rebind = true
		request.ConfirmRebind = func(_ BindingRecord, _ bool) (RebindDisposition, bool, error) {
			switch choice {
			case "close_old":
				return RebindClose, true, nil
			case "leave_old":
				return RebindLeave, true, nil
			default:
				return "", false, fmt.Errorf("invalid rebind decision %q", choice)
			}
		}
	}
	return request, nil
}

func authorizedIdentitiesEqual(first, second PrepareResult) bool {
	if len(first.Participants) != len(second.Participants) {
		return false
	}
	for i := range first.Participants {
		left, right := first.Participants[i], second.Participants[i]
		if left.Handle != right.Handle || left.Runnable != right.Runnable || left.CwdIdentity != right.CwdIdentity ||
			!consultedExecutableEqual(left.Executable, right.Executable) || !consultedExecutableEqual(left.Wrapper, right.Wrapper) {
			return false
		}
	}
	return true
}

func consultedExecutableEqual(first, second *ConsultedExecutable) bool {
	if first == nil || second == nil {
		return first == second
	}
	return first.Requested == second.Requested && first.Consulted == second.Consulted && bytes.Equal(first.Identity, second.Identity)
}

// WithSessionCreationLock serializes in-repository creators for one direct
// session child. Filesystem-exclusive creation remains the correctness boundary
// against callers that do not cooperate with this advisory lock.
func WithSessionCreationLock(base *fsq.DeliveryRoot, session string, operation func() error) (returnErr error) {
	if operation == nil {
		return fmt.Errorf("session creation operation is missing")
	}
	file, err := openAdvisoryLock(base, applyCreationLockPath(session))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := lockExclusive(file); err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, unlockExclusive(file)) }()
	return operation()
}

func acquireExistingApplyLease(state *prepareTargetState, nonce string) (*Lease, error) {
	lockPath := applyCreationLockPath(state.target.Session)
	if _, err := state.baseRoot.Stat(lockPath); errors.Is(err, os.ErrNotExist) {
		return AcquireLease(state.sessionRoot, nonce)
	} else if err != nil {
		return nil, err
	}
	var lease *Lease
	err := WithSessionCreationLock(state.baseRoot, state.target.Session, func() error {
		var acquireErr error
		lease, acquireErr = AcquireLease(state.sessionRoot, nonce)
		return acquireErr
	})
	return lease, err
}

func applyRequestsRebind(request ApplyRequest) bool {
	for _, decision := range request.Decisions {
		if decision.Choice == "close_old" || decision.Choice == "leave_old" {
			return true
		}
	}
	return false
}

func applyCreationLockPath(session string) string {
	sum := sha256.Sum256([]byte(session))
	name := "create-" + hex.EncodeToString(sum[:12]) + ".lock"
	return filepath.Join("meta", "launch", name)
}

func applyActionResult(prepared PrepareResult, reason string) ApplyResult {
	result := applyResultFromPrepare(prepared)
	result.Outcome = ApplyOutcomeActionRequired
	result.ReasonCode = reason
	result.Disposition = MutationNotApplied
	return result
}

func applyResultFromPrepare(prepared PrepareResult) ApplyResult {
	return ApplyResult{
		SubjectSchema: prepared.SubjectSchema, SubjectDigest: prepared.SubjectDigest, PlanDigest: prepared.PlanDigest, TrustDigest: prepared.TrustDigest,
		Backend: prepared.Backend, Profile: prepared.Profile, Roster: prepared.Roster,
		Observations: slices.Clone(prepared.Observations), RequiredActions: slices.Clone(prepared.RequiredActions),
		CallerContext: cloneCallerContext(prepared.CallerContext),
		Disposition:   MutationNotApplied,
	}
}

func classifyApplyMutation(result ApplyResult, root *fsq.DeliveryRoot) ApplyResult {
	binding, bindingErr := LoadBinding(root)
	if bindingErr == nil {
		result.Disposition = MutationCommitted
		result.BindingGeneration = binding.LaunchNonce
		return result
	}
	if journal, present, journalErr := loadOptionalJournal(root); present {
		result.Disposition = MutationUncertain
		result.BindingGeneration = journal.LaunchNonce
		return result
	} else if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		// A journal that exists but cannot be decoded is still evidence that
		// reconciliation may have crossed the backend mutation boundary.
		result.Disposition = MutationUncertain
		return result
	}
	if !errors.Is(bindingErr, os.ErrNotExist) {
		result.Disposition = MutationUncertain
		return result
	}
	result.Disposition = MutationNotApplied
	return result
}

func applyPostCommitFailure(result ApplyResult, request ApplyRequest, root *fsq.DeliveryRoot, reconciled ReconcileResult, reason string, err error) ApplyResult {
	result.SubjectDigest = request.SubjectDigest
	result.Backend, result.TrustDigest = reconciled.Backend, reconciled.SemanticDigest
	result.Outcome, result.ReasonCode, result.FailureDetail = ApplyOutcomeActionRequired, reason, err.Error()
	return classifyApplyMutation(result, root)
}

func onLivePolicies(participants []PrepareParticipant) map[string]string {
	policies := make(map[string]string, len(participants))
	for _, participant := range participants {
		if participant.OnLive != "" {
			policies[participant.Handle] = participant.OnLive
		}
	}
	return policies
}

func stampApplySeatDispositions(result *ApplyResult, seats []SeatDisposition) {
	if len(seats) == 0 {
		return
	}
	byHandle := make(map[string]SeatDisposition, len(seats))
	for _, seat := range seats {
		byHandle[seat.Handle] = seat
	}
	for i := range result.Observations {
		seat, ok := byHandle[result.Observations[i].Handle]
		if !ok || !result.Observations[i].Runnable {
			continue
		}
		result.Observations[i].Disposition = seat.Decision
		if seat.Decision == SeatRefused {
			result.Observations[i].ReasonCode = seat.ReasonCode
			continue
		}
		if seat.Decision == SeatCreated {
			result.Observations[i].StartMode = seat.StartMode
		}
	}
}
