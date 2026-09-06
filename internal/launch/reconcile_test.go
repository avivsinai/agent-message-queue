package launch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestReconcileRejectsProjectProviderBeforeCapabilities(t *testing.T) {
	req := reconcileFixture(t, Commands{})
	provider, sentinel := writeProjectSideEffectingClaude(t, req.ProjectRoot)
	t.Setenv("PATH", filepath.Dir(provider)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AMQ_419_SENTINEL", sentinel)
	req.Adapters = map[string]HarnessAdapter{ClaudeProvider: NewClaudeAdapter(ClaudeProvider)}
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || len(result.Agents) != 1 || !strings.Contains(result.Agents[0].Reason, "inside the project") {
		t.Fatalf("project provider result = %#v, want typed containment refusal", result)
	}
	if !strings.Contains(result.Agents[0].Reason, ProviderProjectContainedCode) {
		t.Fatalf("project provider reason = %q, want %s", result.Agents[0].Reason, ProviderProjectContainedCode)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("project provider capability probe ran side effect: %v", err)
	}
}

func writeProjectSideEffectingClaude(t *testing.T, project string) (string, string) {
	t.Helper()
	bin := filepath.Join(project, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(bin, ClaudeProvider)
	sentinel := filepath.Join(project, "capability-probe-ran")
	script := "#!/bin/sh\nprintf touched > \"$AMQ_419_SENTINEL\"\n"
	if err := os.WriteFile(provider, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return provider, sentinel
}

type reconcileAdapter struct {
	name               string
	capabilityProvider string
	mode               AdapterMode
	available          bool
	reason             string
	freshUnsupported   bool
	resumeUnsupported  bool
	captureUnsupported bool
	preSpawnAcquire    bool
}

func (a reconcileAdapter) Name() string               { return a.name }
func (a reconcileAdapter) Mode() AdapterMode          { return a.mode }
func (a reconcileAdapter) CommittedEnvKeys() []string { return nil }
func (a reconcileAdapter) Capabilities(context.Context) AdapterCapabilities {
	provider := a.name
	if a.capabilityProvider != "" {
		provider = a.capabilityProvider
	}
	providerVersion := "test"
	if a.name == CodexProvider {
		providerVersion = codexCaptureVersion
	} else if a.preSpawnAcquire {
		providerVersion = cursorCaptureVersion
	}
	return AdapterCapabilities{
		Provider: provider, Mode: a.mode, Available: a.available,
		ProviderVersion: providerVersion, Fresh: a.available && !a.freshUnsupported,
		Resume:          a.available && !a.resumeUnsupported,
		Capture:         a.available && a.mode == AdapterModeCapture && !a.captureUnsupported,
		PreSpawnAcquire: a.available && a.preSpawnAcquire, Reason: a.reason,
	}
}

func TestReconcileCursorPreSpawnLeavesPendingConversationForWrapper(t *testing.T) {
	backend := &reconcileBackend{name: "cursor-test"}
	req := reconcileFixture(t, backend)
	req.Config.Agents[0].Adapter = CursorProvider
	req.Config.Agents[0].Command = []string{CursorProvider}
	req.Adapters = map[string]HarnessAdapter{CursorProvider: reconcileAdapter{
		name: CursorProvider, mode: AdapterModeCapture, available: true, preSpawnAcquire: true,
	}}
	result, err := Reconcile(req)
	if err != nil || result.AggregateCode != 0 || result.Outcome != OutcomeCreated {
		t.Fatalf("Cursor reconcile = %#v, %v", result, err)
	}
	record, err := LoadConversation(req.Root, "claude")
	if err != nil || record.State != CapturePending || record.ExecutionEvidence != nil || record.ProviderVersion != cursorCaptureVersion {
		t.Fatalf("pending Cursor conversation = %#v, %v", record, err)
	}
	ticket, err := LoadExecutionTicket(req.Root, "claude")
	if err != nil || !ticket.PreSpawnAcquire || ticket.State != ExecutionPending || ticket.Backend != backend.name ||
		ticket.Profile != backend.Detect().Profile.Identity() || len(ticket.DynamicArgv) != 1 {
		t.Fatalf("pending Cursor ticket = %#v, %v", ticket, err)
	}
}

func (a reconcileAdapter) PlanFresh(req PlanRequest) (AgentPlan, error) {
	if a.name == CodexProvider && a.mode == AdapterModeCapture {
		notify, err := codexNotifyOverride(req)
		if err != nil {
			return AgentPlan{}, err
		}
		plan := AgentPlan{
			Handle: req.Handle, Argv: []string{"/usr/bin/true", "-c", notify}, Cwd: req.Cwd,
			AdapterMode: a.mode, ResumePolicy: req.ResumePolicy, LaunchNonce: req.LaunchNonce,
		}
		return plan, plan.Validate()
	}
	if a.preSpawnAcquire {
		plan := AgentPlan{
			Handle: req.Handle, Argv: []string{"/usr/bin/true", "--resume", preSpawnConversationPlaceholder}, Cwd: req.Cwd,
			AdapterMode: a.mode, ResumePolicy: req.ResumePolicy, LaunchNonce: req.LaunchNonce, PreSpawnAcquire: true,
			DynamicArgv: []DynamicArg{{Index: 2, Kind: DynamicArgConversationID}},
		}
		return plan, plan.Validate()
	}
	plan := AgentPlan{
		Handle: req.Handle, Argv: []string{"/usr/bin/true", req.LaunchNonce}, Cwd: req.Cwd,
		AdapterMode: a.mode, ResumePolicy: req.ResumePolicy, LaunchNonce: req.LaunchNonce,
		DynamicArgv: []DynamicArg{{Index: 1, Kind: DynamicArgLaunchNonce}},
	}
	if a.mode == AdapterModeMint {
		plan.ConversationID = req.LaunchNonce
	}
	return plan, plan.Validate()
}
func (a reconcileAdapter) PlanResume(req ResumeRequest) (AgentPlan, error) {
	if a.name == CodexProvider && a.mode == AdapterModeCapture {
		notify, err := codexNotifyOverride(req.PlanRequest)
		if err != nil {
			return AgentPlan{}, err
		}
		plan := AgentPlan{
			Handle: req.Handle, Argv: []string{"/usr/bin/true", "resume", "-c", notify, req.Conversation.ID}, Cwd: req.Cwd,
			AdapterMode: a.mode, ResumePolicy: ResumeEnabled, LaunchNonce: req.LaunchNonce, ConversationID: req.Conversation.ID,
			DynamicArgv: []DynamicArg{{Index: 4, Kind: DynamicArgConversationID}},
		}
		return plan, plan.Validate()
	}
	plan := AgentPlan{
		Handle: req.Handle, Argv: []string{"/usr/bin/true", req.Conversation.ID}, Cwd: req.Cwd,
		AdapterMode: a.mode, ResumePolicy: ResumeEnabled, LaunchNonce: req.LaunchNonce,
		ConversationID: req.Conversation.ID,
		DynamicArgv:    []DynamicArg{{Index: 1, Kind: DynamicArgConversationID}},
	}
	return plan, plan.Validate()
}
func (a reconcileAdapter) CaptureIdentity(request CaptureRequest) CaptureResult {
	if a.mode == AdapterModeCapture {
		return captureCodexIdentity(request)
	}
	return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonAdapterMintsIdentity}
}

type reconcileBackend struct {
	mu                        sync.Mutex
	name                      string
	inspect                   InspectStatus
	creates                   int
	closes                    int
	focuses                   int
	createGate                chan struct{}
	createStart               chan struct{}
	capture                   bool
	captured                  *CaptureEvidence
	invalidBind               bool
	reclaims                  int
	resourceUp                bool
	reclaimAs                 ReclaimStatus
	reclaimList               []ResourceIdentity
	reclaimFlip               bool
	createErr                 error
	definiteErr               bool
	leaveJournalOnCreateError bool
	joined                    bool
	joinBindingSeen           bool
	planNonce                 string
	planHandles               []string
	persistCandidate          bool
}

func (b *reconcileBackend) Detect() DetectResult {
	profile := Profile{Backend: b.name, Platform: "test", VersionRange: "*", Version: 1, Capabilities: []Capability{CapCreate, CapInspect, CapClose, CapFocus, CapReclaim}}
	return DetectResult{
		Available: true, Profile: profile, Effective: profile.Capabilities,
		HostIdentity: "host:test", InstanceIdentity: "instance:test",
	}
}
func (b *reconcileBackend) Create(req CreateRequest) (CreateResult, error) {
	b.mu.Lock()
	b.creates++
	createErr, definiteErr, leaveJournal := b.createErr, b.definiteErr, b.leaveJournalOnCreateError
	b.joinBindingSeen = req.JoinBinding != nil
	if createErr != nil {
		b.mu.Unlock()
		if leaveJournal {
			if err := os.WriteFile(JournalPath(req.Root.Base()), []byte("join-create-error-sentinel"), 0o600); err != nil {
				return CreateResult{}, errors.Join(createErr, err)
			}
		}
		if definiteErr {
			return CreateResult{}, &DefinitePreCreateError{Err: createErr}
		}
		return CreateResult{}, createErr
	}
	b.resourceUp = true
	b.joined = req.JoinBinding != nil
	b.planHandles = make([]string, 0, len(req.Plan.Agents))
	for _, agent := range req.Plan.Agents {
		b.planHandles = append(b.planHandles, agent.Handle)
	}
	if len(req.Plan.Agents) > 0 {
		b.planNonce = req.Plan.Agents[0].LaunchNonce
	}
	b.mu.Unlock()
	if b.createStart != nil {
		select {
		case b.createStart <- struct{}{}:
		default:
		}
	}
	if b.createGate != nil {
		<-b.createGate
	}
	bindingNonce := req.Plan.Agents[0].LaunchNonce
	if req.JoinBinding != nil {
		bindingNonce = req.JoinBinding.LaunchNonce
	}
	result := CreateResult{Outcome: OutcomeCreated, Profile: b.Detect().Profile.Identity(), Binding: BindingRecord{
		Version: BindingVersion, Backend: b.name, HostIdentity: "host:test", InstanceIdentity: "instance:test",
		Profile: b.Detect().Profile.Identity(), LaunchNonce: bindingNonce,
		Resources: ResourceIdentitySet{Version: ResourceSetVersion, Resources: []ResourceIdentity{{OpaqueID: "resource:test"}}},
	}}
	if b.invalidBind {
		result.Binding.Resources.Version = 0
	}
	if b.capture {
		if b.captured == nil {
			evidence, err := ParseCodexNotifyEvidence(codexNotifyTestPayload("019c8a2f-2b13-7000-8000-000000000001", req.Plan.Agents[0].Cwd), req.Plan.Agents[0].LaunchNonce, req.Plan.Agents[0].Handle, codexCaptureVersion, req.Plan.Agents[0].Cwd)
			if err != nil {
				return CreateResult{}, err
			}
			b.captured = &evidence
		}
		result.CaptureEvidence = map[string][]CaptureEvidence{req.Plan.Agents[0].Handle: {*b.captured}}
	}
	if b.persistCandidate && req.PersistCandidate != nil {
		if err := req.PersistCandidate(result.Binding); err != nil {
			return CreateResult{}, err
		}
	}
	return result, nil
}
func (b *reconcileBackend) Inspect(InspectRequest) (InspectResult, error) {
	return InspectResult{Status: b.inspect, Evidence: "test evidence", ActionRequired: b.inspect == InspectUnknown}, nil
}
func (b *reconcileBackend) Close(CloseRequest) (CloseResult, error) {
	b.mu.Lock()
	b.closes++
	b.mu.Unlock()
	return CloseResult{Outcome: Outcome("closed")}, nil
}
func (b *reconcileBackend) Focus(FocusRequest) (FocusResult, error) {
	b.mu.Lock()
	b.focuses++
	b.mu.Unlock()
	return FocusResult{Outcome: OutcomeAttached}, nil
}
func (b *reconcileBackend) Reclaim(req ReclaimRequest) (ReclaimResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reclaims++
	if b.reclaimAs != "" && b.reclaimAs != ReclaimAdoptable {
		return ReclaimResult{
			Status: b.reclaimAs, Evidence: "test classified recovery",
			Resources: slices.Clone(b.reclaimList),
		}, nil
	}
	if !b.resourceUp {
		return ReclaimResult{Status: ReclaimAbsent, Evidence: "test resource absent"}, nil
	}
	binding := BindingRecord{
		Version: BindingVersion, Backend: b.name, HostIdentity: "host:test", InstanceIdentity: "instance:test",
		Profile: b.Detect().Profile.Identity(), LaunchNonce: req.Journal.LaunchNonce,
		Resources: ResourceIdentitySet{Version: ResourceSetVersion, Resources: []ResourceIdentity{{OpaqueID: "resource:test"}}},
	}
	result := ReclaimResult{
		Status: ReclaimAdoptable, Evidence: "test name and nonce match", Binding: binding,
		Resources: slices.Clone(binding.Resources.Resources),
	}
	if b.reclaimFlip && b.reclaims > 1 {
		result.Binding.Resources.Resources[0].OpaqueID = "resource:replacement"
		result.Resources = slices.Clone(result.Binding.Resources.Resources)
	}
	if b.capture {
		if b.captured == nil {
			evidence, err := ParseCodexNotifyEvidence(codexNotifyTestPayload("019c8a2f-2b13-7000-8000-000000000001", req.Journal.Plan.Agents[0].Cwd), req.Journal.LaunchNonce, req.Journal.Plan.Agents[0].Handle, codexCaptureVersion, req.Journal.Plan.Agents[0].Cwd)
			if err != nil {
				return ReclaimResult{}, err
			}
			b.captured = &evidence
		}
		result.CaptureEvidence = map[string][]CaptureEvidence{req.Journal.Plan.Agents[0].Handle: {*b.captured}}
	}
	return result, nil
}

func reconcileFixture(t *testing.T, backend Backend) ReconcileRequest {
	t.Helper()
	project := t.TempDir()
	_, root := harnessRoot(t)
	store, err := OpenTrustStore(t.TempDir(), project)
	if err != nil {
		t.Fatal(err)
	}
	backendName := backend.Detect().Profile.Backend
	return ReconcileRequest{
		ProjectRoot: project, Session: "collab", Root: root,
		Config: ProjectConfig{Schema: ProjectConfigSchema, DefaultSession: "collab", Layout: LayoutIntent{Type: LayoutColumns}, Agents: []ProjectAgentConfig{
			{Handle: "claude", Adapter: "claude", Command: []string{"claude"}, ResumePolicy: ResumeEnabled},
		}},
		Launcher: backendName, Preferences: []string{backendName}, Backends: map[string]Backend{backendName: backend},
		Adapters:   map[string]HarnessAdapter{"claude": reconcileAdapter{name: "claude", mode: AdapterModeMint, available: true}},
		TrustStore: store, ConfirmTrust: func(Plan, string) (bool, error) { return true, nil }, HostIdentity: "host:test",
	}
}

func writeReconcileBinding(t *testing.T, req ReconcileRequest, backend *reconcileBackend, profile string) {
	t.Helper()
	lease, err := AcquireLease(req.Root, testLaunchNonce)
	if err != nil {
		t.Fatal(err)
	}
	record := BindingRecord{
		Version: BindingVersion, Backend: backend.name, HostIdentity: "host:test", InstanceIdentity: "instance:test",
		Profile: profile, LaunchNonce: testLaunchNonce,
		Resources: ResourceIdentitySet{Version: ResourceSetVersion, Resources: []ResourceIdentity{{OpaqueID: "resource:test"}}},
	}
	if err := WriteBinding(req.Root, lease, record); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func writeReconcileConversation(t *testing.T, req ReconcileRequest, record ConversationRecord) {
	t.Helper()
	lease, err := AcquireLease(req.Root, testLaunchNonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles(record.Handle); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(req.Root, lease, record); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func reconcileExecutionEvidence(backend *reconcileBackend, nonce string) *ConversationExecutionEvidence {
	return &ConversationExecutionEvidence{
		Backend: backend.name, Profile: backend.Detect().Profile.Identity(), Outcome: OutcomeCreated,
		LaunchNonce: nonce, ConversationID: testConversationID,
	}
}

func TestReconcileInspectUnknownMakesZeroBackendMutations(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectUnknown}
	req := reconcileFixture(t, backend)
	writeReconcileBinding(t, req, backend, backend.Detect().Profile.Identity())
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Reason != "inspect_unknown" || backend.creates != 0 || backend.closes != 0 {
		t.Fatalf("result=%#v creates=%d closes=%d", result, backend.creates, backend.closes)
	}
}

func TestReconcilePresentCompatibleAttachesWithoutCreate(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectPresent}
	req := reconcileFixture(t, backend)
	writeReconcileConversation(t, req, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureReady,
		Identity: ConversationIdentity{Provider: "claude", ID: testConversationID}, LaunchNonce: testLaunchNonce,
		ExecutionEvidence: reconcileExecutionEvidence(backend, testLaunchNonce),
	})
	writeReconcileBinding(t, req, backend, backend.Detect().Profile.Identity())
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 0 || result.Outcome != OutcomeAttached || backend.creates != 0 || backend.closes != 0 || backend.focuses != 1 {
		t.Fatalf("result=%#v creates=%d closes=%d focuses=%d", result, backend.creates, backend.closes, backend.focuses)
	}
}

func TestReconcileOnLiveKeepJoinWritesTicketsUnderLeaseNonce(t *testing.T) {
	backend := &reconcileBackend{name: LauncherTMux, inspect: InspectPresent}
	req := reconcileFixture(t, backend)
	req.Config.Agents = append(req.Config.Agents, ProjectAgentConfig{
		Handle: "codex", Adapter: "claude", Command: []string{"claude"}, ResumePolicy: ResumeFresh,
	})
	req.OnLive = map[string]string{"claude": OnLiveKeep, "codex": OnLiveKeep}
	lease, err := AcquireLease(req.Root, testLaunchNonce)
	if err != nil {
		t.Fatal(err)
	}
	binding := BindingRecord{
		Version: BindingVersion, Backend: LauncherTMux, HostIdentity: "host:test", InstanceIdentity: "instance:test",
		Profile: backend.Detect().Profile.Identity(), LaunchNonce: testLaunchNonce,
		Resources: ResourceIdentitySet{Version: ResourceSetVersion, Resources: []ResourceIdentity{
			{OpaqueID: "resource:claude", Agent: "claude"},
		}},
	}
	if err := WriteBinding(req.Root, lease, binding); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	held, err := AcquireLease(req.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	req.HeldLease = held
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 0 || result.Outcome != OutcomeCreated {
		t.Fatalf("join result=%#v", result)
	}
	if backend.creates != 1 || !backend.joined {
		t.Fatalf("join create=%d joined=%v", backend.creates, backend.joined)
	}
	if backend.planNonce != held.LaunchNonce() {
		t.Fatalf("join plan nonce=%s lease=%s", backend.planNonce, held.LaunchNonce())
	}
	ticket, err := LoadExecutionTicket(req.Root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.LaunchNonce != held.LaunchNonce() {
		t.Fatalf("created ticket nonce=%s lease=%s", ticket.LaunchNonce, held.LaunchNonce())
	}
	if _, err := LoadExecutionTicket(req.Root, "claude"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("kept seat wrote an execution ticket: %v", err)
	}
	published, err := LoadBinding(req.Root)
	if err != nil {
		t.Fatal(err)
	}
	if published.LaunchNonce != testLaunchNonce {
		t.Fatalf("join rebound generation to %s", published.LaunchNonce)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileForeignBindingRequiresLeaveRebind(t *testing.T) {
	old := &reconcileBackend{name: "old", inspect: InspectPresent}
	newBackend := &reconcileBackend{name: "new", inspect: InspectAbsent}
	req := reconcileFixture(t, newBackend)
	req.Backends["old"] = old
	req.Launcher = "new"
	writeReconcileBinding(t, req, old, old.Detect().Profile.Identity())
	record, err := LoadBinding(req.Root)
	if err != nil {
		t.Fatal(err)
	}
	record.HostIdentity = "host:foreign"
	lease, err := AcquireLease(req.Root, testLaunchNonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBinding(req.Root, lease, record); err != nil {
		t.Fatal(err)
	}
	_ = lease.Release()
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || newBackend.creates != 0 || old.closes != 0 {
		t.Fatalf("blocked foreign result=%#v", result)
	}
	req.Rebind = true
	req.ConfirmRebind = func(BindingRecord, bool) (RebindDisposition, bool, error) { return RebindLeave, true, nil }
	result, err = Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 0 || newBackend.creates != 1 || old.closes != 0 {
		t.Fatalf("leave rebind result=%#v creates=%d closes=%d", result, newBackend.creates, old.closes)
	}
}

func TestReconcileAutoInsideCmuxPrependsCmux(t *testing.T) {
	tmux := &reconcileBackend{name: LauncherTMux, inspect: InspectAbsent}
	cmux := &reconcileBackend{name: LauncherCMux, inspect: InspectAbsent}
	req := reconcileFixture(t, tmux)
	req.Backends[LauncherCMux] = cmux
	req.Launcher = LauncherAuto
	req.Preferences = []string{LauncherTMux, LauncherCommands}
	t.Setenv("CMUX_SURFACE_ID", "F901D722-6789-4BBB-9818-C4E97F20BEB3")
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != LauncherCMux || cmux.creates != 1 || tmux.creates != 0 {
		t.Fatalf("inside-cmux selected %#v tmux=%d cmux=%d", result, tmux.creates, cmux.creates)
	}
}

func TestReconcileStaleHandleFailsClosedWhilePeerContinues(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	req := reconcileFixture(t, backend)
	req.ResumeOnly = true
	req.Config.Agents = append(req.Config.Agents, ProjectAgentConfig{
		Handle: "peer", Adapter: "claude", Command: []string{"claude"}, ResumePolicy: ResumeEnabled,
	})
	writeReconcileConversation(t, req, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureStale,
		LaunchNonce: testLaunchNonce, Reason: CaptureReasonEvidenceMissing,
	})
	writeReconcileConversation(t, req, ConversationRecord{
		Version: ConversationVersion, Handle: "peer", State: CaptureReady,
		Identity:          ConversationIdentity{Provider: "claude", ID: testConversationID},
		LaunchNonce:       testLaunchNonce,
		ExecutionEvidence: reconcileExecutionEvidence(backend, testLaunchNonce),
	})
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Reason != ReasonStaleConversation || backend.creates != 1 || result.Plan == nil || len(result.Plan.Agents) != 1 || result.Plan.Agents[0].Handle != "peer" {
		t.Fatalf("partial stale result=%#v creates=%d", result, backend.creates)
	}
	if result.Agents[0].Reason != "stale_conversation" || result.Agents[1].ConversationDisposition != DispositionResumed {
		t.Fatalf("agent results=%#v", result.Agents)
	}
}

func TestReconcileStaleAllowsExplicitFreshFallback(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	req := reconcileFixture(t, backend)
	req.ResumeOnly = true
	req.AllowFreshFallback = true
	writeReconcileConversation(t, req, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureStale,
		LaunchNonce: testLaunchNonce, Reason: CaptureReasonEvidenceMissing,
	})
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 0 || result.Agents[0].ConversationDisposition != DispositionFreshAfterStale || backend.creates != 1 {
		t.Fatalf("fallback result=%#v", result)
	}
}

func TestReconcileMintStaysPendingUntilAcknowledgedThenResumesExactIdentity(t *testing.T) {
	commands := Commands{}
	req := reconcileFixture(t, commands)
	first, err := Reconcile(req)
	if err != nil || first.Plan == nil {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	firstID := first.Plan.Agents[0].ConversationID

	managed := &reconcileBackend{name: "managed", inspect: InspectAbsent}
	req.Launcher = managed.name
	req.Preferences = []string{managed.name}
	req.Backends = map[string]Backend{managed.name: managed}
	second, err := Reconcile(req)
	if err != nil || second.AggregateCode != 0 || second.Plan == nil {
		t.Fatalf("second result=%#v err=%v", second, err)
	}
	readyID := second.Plan.Agents[0].ConversationID
	if readyID == firstID || second.Agents[0].Reason != ReasonPriorLaunchNotExecuted || second.Agents[0].ConversationDisposition != DispositionFresh {
		t.Fatalf("second result=%#v, first ID=%q ready ID=%q", second, firstID, readyID)
	}
	record, err := LoadConversation(req.Root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != CapturePending || record.Identity.ID != "" || record.ExecutionEvidence != nil {
		t.Fatalf("backend creation made conversation resumable: %#v", record)
	}
	ticket, err := LoadExecutionTicket(req.Root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.State != ExecutionPending || ticket.LaunchNonce != readyID {
		t.Fatalf("execution ticket=%#v, want pending generation %q", ticket, readyID)
	}
	if _, err := PrepareExecution(req.Root, "claude", ticket.LaunchNonce, ExecutionEnvelope{
		Cwd: ticket.Cwd, AMQExecutable: ticket.AMQExecutable,
		ProviderExecutable: ticket.ProviderExecutable, TargetArgv: ticket.TargetArgv, Execution: ticket.Execution,
	}); err != nil {
		t.Fatal(err)
	}
	record, err = LoadConversation(req.Root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != CaptureReady || record.Identity.ID != readyID || record.ExecutionEvidence == nil || record.ExecutionEvidence.Backend != CommandsBackendName {
		t.Fatalf("acknowledged record=%#v", record)
	}

	third, err := Reconcile(req)
	if err != nil || third.AggregateCode != 0 || third.Plan == nil {
		t.Fatalf("third result=%#v err=%v", third, err)
	}
	if third.Agents[0].ConversationDisposition != DispositionResumed || third.Plan.Agents[0].ConversationID != readyID {
		t.Fatalf("third result=%#v, want resumed ID %q", third, readyID)
	}
}

func TestReconcileConcurrentLaunchCreatesOnce(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent, createGate: make(chan struct{}), createStart: make(chan struct{}, 1)}
	req := reconcileFixture(t, backend)
	firstDone := make(chan ReconcileResult, 1)
	go func() {
		result, _ := Reconcile(req)
		firstDone <- result
	}()
	<-backend.createStart
	second, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if second.AggregateCode != 6 || backend.creates != 1 {
		t.Fatalf("concurrent result=%#v creates=%d", second, backend.creates)
	}
	close(backend.createGate)
	first := <-firstDone
	if first.AggregateCode != 0 || backend.creates != 1 {
		t.Fatalf("first result=%#v creates=%d", first, backend.creates)
	}
}

func TestReconcileUntrustedAndRosterDriftAreStructured(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	req := reconcileFixture(t, backend)
	req.ConfirmTrust = nil
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || backend.creates != 0 {
		t.Fatalf("untrusted result=%#v", result)
	}
	req.Adapters["claude"] = reconcileAdapter{name: "claude", mode: AdapterModeMint, reason: "executable_not_found"}
	result, err = Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 0 || result.Outcome != OutcomeNoAction ||
		result.Agents[0].ConversationDisposition != DispositionUnsupported || backend.creates != 0 {
		t.Fatalf("roster drift result=%#v", result)
	}
	req.Adapters["claude"] = reconcileAdapter{
		name: "claude", capabilityProvider: "wrong-provider", mode: AdapterModeMint, available: true,
	}
	result, err = Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 1 || result.Outcome == OutcomeNoAction ||
		result.Agents[0].ConversationDisposition != DispositionDegraded || backend.creates != 0 {
		t.Fatalf("degraded result=%#v", result)
	}
}

func TestReconcileManagedCreateCrashMatrixConvergesWithoutDuplicateSpawn(t *testing.T) {
	for _, stage := range []string{
		"journal_written", "backend_created", "journal_created",
		"conversations_written", "binding_written", "journal_cleared",
	} {
		t.Run(stage, func(t *testing.T) {
			backend := &reconcileBackend{name: "test", inspect: InspectPresent}
			req := reconcileFixture(t, backend)
			crash := errors.New("injected crash")
			fired := false
			req.CrashHook = func(got string) error {
				if got == stage && !fired {
					fired = true
					return crash
				}
				return nil
			}
			if _, err := Reconcile(req); !errors.Is(err, crash) {
				t.Fatalf("crash error = %v", err)
			}
			inspection, err := InspectLease(req.Root)
			if err != nil {
				t.Fatal(err)
			}
			if inspection.State != LeaseMissing {
				t.Fatalf("lease after crash = %#v", inspection)
			}

			req.CrashHook = nil
			result, err := Reconcile(req)
			wantCode := 0
			if stage == "journal_cleared" {
				// The binding and pending conversation are durable, but the
				// provider was never acknowledged. Do not expose a resumable
				// conversation or silently start a duplicate.
				wantCode = 6
			}
			if err != nil || result.AggregateCode != wantCode {
				t.Fatalf("recovery result=%#v err=%v", result, err)
			}
			if backend.creates != 1 {
				t.Fatalf("backend creates=%d, want exactly one", backend.creates)
			}
			if _, err := LoadJournal(req.Root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal after recovery: %v", err)
			}
			if _, err := LoadBinding(req.Root); err != nil {
				t.Fatalf("binding after recovery: %v", err)
			}
			record, err := LoadConversation(req.Root, "claude")
			if err != nil || record.State != CapturePending || record.Identity.ID != "" || record.ExecutionEvidence != nil {
				t.Fatalf("conversation after recovery=%#v err=%v, want pending until execution acknowledgement", record, err)
			}
		})
	}
}

func TestReconcileJournalRequiresClassifiedReclaimAndPreservesInventory(t *testing.T) {
	for _, test := range []struct {
		name       string
		wrap       bool
		status     ReclaimStatus
		wantReason string
	}{
		{name: "no_reclaim", wrap: true, wantReason: "launch_recovery_not_supported"},
		{name: "incomplete", status: ReclaimIncomplete, wantReason: "launch_recovery_incomplete"},
		{name: "unknown", status: ReclaimUnknown, wantReason: "launch_recovery_unknown"},
		{name: "foreign", status: ReclaimForeign, wantReason: "launch_recovery_foreign"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &reconcileBackend{
				name: "test", inspect: InspectAbsent, reclaimAs: test.status,
				reclaimList: []ResourceIdentity{{OpaqueID: "resource:partial", Agent: "claude"}},
			}
			var selected Backend = backend
			if test.wrap {
				selected = noReclaimBackend{Backend: backend}
			}
			req := reconcileFixture(t, selected)
			crash := errors.New("injected crash")
			req.CrashHook = func(stage string) error {
				if stage == "backend_created" {
					return crash
				}
				return nil
			}
			if _, err := Reconcile(req); !errors.Is(err, crash) {
				t.Fatalf("crash error = %v", err)
			}
			req.CrashHook = nil
			result, err := Reconcile(req)
			if err != nil || result.AggregateCode != 6 || result.Reason != test.wantReason || backend.creates != 1 {
				t.Fatalf("result=%#v creates=%d err=%v", result, backend.creates, err)
			}
			if test.status != "" && (result.Recovery == nil || !slices.Equal(result.Recovery.Resources, backend.reclaimList)) {
				t.Fatalf("recovery report=%#v, want exact inventory %#v", result.Recovery, backend.reclaimList)
			}
			if _, err := LoadJournal(req.Root); err != nil {
				t.Fatalf("journal was not preserved: %v", err)
			}
			if _, err := LoadBinding(req.Root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovery published binding: %v", err)
			}
		})
	}
}

type noReclaimBackend struct{ Backend }

func TestReconcileCaptureEvidencePersistsExactIdentity(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent, capture: true}
	req := reconcileFixture(t, backend)
	req.Config.Agents[0].Adapter = "codex"
	req.Config.Agents[0].Command = []string{"codex"}
	req.Adapters = map[string]HarnessAdapter{"codex": reconcileAdapter{name: "codex", mode: AdapterModeCapture, available: true}}
	result, err := Reconcile(req)
	if err != nil || result.AggregateCode != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	record, err := LoadConversation(req.Root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != CaptureReady || record.Identity.Provider != CodexProvider || record.Identity.ID != "019c8a2f-2b13-7000-8000-000000000001" || record.ExecutionEvidence == nil || record.ExecutionEvidence.Backend != backend.name || len(record.EvidenceRefs) != 1 {
		t.Fatalf("record=%#v", record)
	}
	evidence, ref, err := ReadEvidence(req.Root, record.EvidenceRefs[0])
	if err != nil || evidence.Kind != EvidenceProviderCapture || ref.ID != record.EvidenceRefs[0] {
		t.Fatalf("capture evidence = %#v %#v, %v", evidence, ref, err)
	}
}

func TestReconcileCreateFailureClassificationControlsJournalClear(t *testing.T) {
	for _, definite := range []bool{true, false} {
		t.Run(map[bool]string{true: "definite", false: "uncertain"}[definite], func(t *testing.T) {
			backend := &reconcileBackend{name: "test", inspect: InspectAbsent, createErr: errors.New("create failed"), definiteErr: definite}
			req := reconcileFixture(t, backend)
			result, err := Reconcile(req)
			if err != nil || result.AggregateCode != 1 {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			_, journalErr := LoadJournal(req.Root)
			if definite && !errors.Is(journalErr, os.ErrNotExist) {
				t.Fatalf("definite pre-create failure retained journal: %v", journalErr)
			}
			if !definite && journalErr != nil {
				t.Fatalf("uncertain create failure lost journal: %v", journalErr)
			}
			_, ticketErr := LoadExecutionTicket(req.Root, "claude")
			if definite && !errors.Is(ticketErr, os.ErrNotExist) {
				t.Fatalf("definite pre-create failure retained execution ticket: %v", ticketErr)
			}
			if !definite && ticketErr != nil {
				t.Fatalf("uncertain create failure lost execution ticket: %v", ticketErr)
			}
		})
	}
}
