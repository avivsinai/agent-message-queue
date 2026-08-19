package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReconcileEmitsCanonicalResolvedWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires host policy support")
	}
	req := reconcileFixture(t, Commands{})
	alias := filepath.Join(t.TempDir(), "project-alias")
	if err := os.Symlink(req.ProjectRoot, alias); err != nil {
		t.Fatal(err)
	}
	req.ProjectRoot = alias
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(alias)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Commands) != 1 || result.Commands[0].Cwd != resolved || result.Plan == nil || result.Plan.Agents[0].Cwd != resolved {
		t.Fatalf("canonical cwd result=%#v, want %q", result, resolved)
	}
}

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
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("project provider capability probe ran side effect: %v", err)
	}
}

func TestReconcileJournalRejectsProjectProviderBeforeCapabilities(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	req := reconcileFixture(t, backend)
	provider, sentinel := writeProjectSideEffectingClaude(t, req.ProjectRoot)
	t.Setenv("PATH", filepath.Dir(provider)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AMQ_419_SENTINEL", sentinel)
	nonce := "019c8a2f-2b13-7000-8000-000000000099"
	plan, agents, conversations := journalFixturePlan(nonce)
	plan.Agents[0].Argv[0] = provider
	digest, err := plan.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewLaunchJournal(req, backend.name, backend.Detect(), plan, digest, nonce, agents, conversations, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(req.Root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJournal(req.Root, lease, record); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	req.Adapters = map[string]HarnessAdapter{ClaudeProvider: NewClaudeAdapter(ClaudeProvider)}
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Reason != "launch_journal_plan_unavailable" {
		t.Fatalf("project provider journal result = %#v, want plan refusal", result)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("journal capability probe ran side effect: %v", err)
	}
}

func TestResolveLaunchAMQExecutableRejectsProjectPath(t *testing.T) {
	project := t.TempDir()
	inside := filepath.Join(project, "amq")
	if err := os.WriteFile(inside, []byte("amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveLaunchAMQExecutable(inside, project); err == nil || !strings.Contains(err.Error(), AMQProjectContainedCode) {
		t.Fatalf("project-contained AMQ path error = %v, want %s", err, AMQProjectContainedCode)
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

func TestReconcileUsesAndRetainsCallerHeldLease(t *testing.T) {
	req := reconcileFixture(t, Commands{})
	lease, err := AcquireLease(req.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	req.HeldLease = lease
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan == nil || len(result.Commands) != 1 {
		t.Fatalf("held-lease reconciliation = %#v", result)
	}
	inspection, err := InspectLease(req.Root)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.State != LeaseValid || inspection.Nonce != lease.LaunchNonce() {
		t.Fatalf("caller-held lease after Reconcile = %#v", inspection)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
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

func TestReconcileCaptureFreshRequiresFreshAndCaptureCapabilities(t *testing.T) {
	for _, test := range []struct {
		name       string
		adapter    reconcileAdapter
		wantReason string
	}{
		{name: "fresh missing", adapter: reconcileAdapter{freshUnsupported: true}, wantReason: "fresh_capability_unsupported"},
		{name: "capture missing", adapter: reconcileAdapter{captureUnsupported: true, reason: "capture_version_unsupported"}, wantReason: "capture_version_unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := reconcileFixture(t, Commands{})
			req.Config.Agents[0].Adapter = CodexProvider
			req.Config.Agents[0].Command[0] = CodexProvider
			test.adapter.name, test.adapter.mode, test.adapter.available = CodexProvider, AdapterModeCapture, true
			req.Adapters = map[string]HarnessAdapter{CodexProvider: test.adapter}
			result, err := Reconcile(req)
			if err != nil {
				t.Fatal(err)
			}
			if result.AggregateCode != 6 || result.Outcome != OutcomeActionRequired || len(result.Commands) != 0 || len(result.Agents) != 1 ||
				result.Agents[0].ConversationDisposition != DispositionActionRequired || result.Agents[0].Reason != test.wantReason {
				t.Fatalf("unsupported capture result=%#v", result)
			}
			if _, err := LoadConversation(req.Root, "claude"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsupported capture wrote conversation state: %v", err)
			}
			if _, err := LoadExecutionTicket(req.Root, "claude"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsupported capture wrote execution ticket: %v", err)
			}
		})
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

func TestReconcileCaptureReadyResumeRequiresResumeAlone(t *testing.T) {
	req := reconcileFixture(t, Commands{})
	req.Config.Agents[0].Adapter = CodexProvider
	req.Config.Agents[0].Command[0] = CodexProvider
	req.Adapters = map[string]HarnessAdapter{CodexProvider: reconcileAdapter{
		name: CodexProvider, mode: AdapterModeCapture, available: true,
		captureUnsupported: true, reason: "capture_version_unsupported",
	}}
	writeReconcileConversation(t, req, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureReady,
		Identity: ConversationIdentity{Provider: CodexProvider, ID: testConversationID}, ProviderVersion: codexCaptureVersion, LaunchNonce: testLaunchNonce,
		ExecutionEvidence: reconcileExecutionEvidence(&reconcileBackend{name: CommandsBackendName}, testLaunchNonce),
	})
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Outcome != OutcomeCommandsEmitted || len(result.Commands) != 1 ||
		result.Agents[0].ConversationDisposition != DispositionResumed || result.Plan == nil || result.Plan.Agents[0].ConversationID != testConversationID {
		t.Fatalf("resume-only capability result=%#v", result)
	}
}

func TestReconcileCaptureReadyRefusesWithoutResumeCapability(t *testing.T) {
	req := reconcileFixture(t, Commands{})
	req.Config.Agents[0].Adapter = CodexProvider
	req.Config.Agents[0].Command[0] = CodexProvider
	req.Adapters = map[string]HarnessAdapter{CodexProvider: reconcileAdapter{
		name: CodexProvider, mode: AdapterModeCapture, available: true, resumeUnsupported: true,
	}}
	writeReconcileConversation(t, req, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureReady,
		Identity: ConversationIdentity{Provider: CodexProvider, ID: testConversationID}, ProviderVersion: codexCaptureVersion, LaunchNonce: testLaunchNonce,
		ExecutionEvidence: reconcileExecutionEvidence(&reconcileBackend{name: CommandsBackendName}, testLaunchNonce),
	})
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Outcome != OutcomeActionRequired || len(result.Commands) != 0 ||
		result.Agents[0].ConversationDisposition != DispositionActionRequired || result.Agents[0].Reason != "resume_capability_unsupported" {
		t.Fatalf("unsupported resume result=%#v", result)
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
	mu          sync.Mutex
	name        string
	inspect     InspectStatus
	creates     int
	closes      int
	focuses     int
	createGate  chan struct{}
	createStart chan struct{}
	capture     bool
	captured    *CaptureEvidence
	invalidBind bool
	reclaims    int
	resourceUp  bool
	reclaimAs   ReclaimStatus
	reclaimList []ResourceIdentity
	reclaimFlip bool
	createErr   error
	definiteErr bool
	joined      bool
	planNonce   string
	planHandles []string
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
	createErr, definiteErr := b.createErr, b.definiteErr
	if createErr != nil {
		b.mu.Unlock()
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

func TestReconcilePresentIncompatibleMakesNoBackendMutation(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectPresent}
	req := reconcileFixture(t, backend)
	writeReconcileBinding(t, req, backend, "test/test/v999")
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Reason != "present_binding_incompatible" || backend.creates != 0 || backend.closes != 0 || backend.focuses != 0 {
		t.Fatalf("result=%#v creates=%d closes=%d focuses=%d", result, backend.creates, backend.closes, backend.focuses)
	}
}

func TestReconcilePresentWithoutConversationMakesNoBackendMutation(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectPresent}
	req := reconcileFixture(t, backend)
	writeReconcileBinding(t, req, backend, backend.Detect().Profile.Identity())
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Reason != "binding_present_without_resumable_conversation" || backend.creates != 0 || backend.closes != 0 || backend.focuses != 0 {
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

func TestReconcileOnLiveKeepDoesNotRedeliverKeptSeat(t *testing.T) {
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
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	ref, err := WriteEvidence(req.Root, lease, EvidenceWriteRequest{
		Kind: EvidenceManual, Handle: "claude", ObservedAt: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
		Payload: []byte(`{"kept":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(req.Root, lease, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureReady,
		Identity: ConversationIdentity{Provider: "claude", ID: testConversationID}, LaunchNonce: testLaunchNonce,
		ExecutionEvidence: reconcileExecutionEvidence(backend, testLaunchNonce),
		EvidenceRefs:      []string{ref.ID},
	}); err != nil {
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
	conversationBefore, err := os.ReadFile(ConversationPath(req.Root.Base(), "claude"))
	if err != nil {
		t.Fatal(err)
	}
	evidenceBefore, err := os.ReadFile(EvidencePath(req.Root.Base(), ref.ID))
	if err != nil {
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
	if result.AggregateCode != 0 || backend.creates != 1 || !slices.Equal(backend.planHandles, []string{"codex"}) {
		t.Fatalf("kept seat redelivered: result=%#v creates=%d handles=%v", result, backend.creates, backend.planHandles)
	}
	conversationAfter, err := os.ReadFile(ConversationPath(req.Root.Base(), "claude"))
	if err != nil {
		t.Fatal(err)
	}
	evidenceAfter, err := os.ReadFile(EvidencePath(req.Root.Base(), ref.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(conversationBefore, conversationAfter) {
		t.Fatalf("kept conversation mutated\nbefore=%s\nafter=%s", conversationBefore, conversationAfter)
	}
	if !bytes.Equal(evidenceBefore, evidenceAfter) {
		t.Fatalf("kept evidence mutated")
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileOnLiveProfileMismatchRefusesCohortWithoutMutation(t *testing.T) {
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
		Profile: "tmux/test/v2", LaunchNonce: testLaunchNonce,
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
	bindingBefore, err := os.ReadFile(BindingPath(req.Root.Base()))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Reason != ReasonLiveParticipantRefused {
		t.Fatalf("profile mismatch result=%#v", result)
	}
	if backend.creates != 0 || backend.closes != 0 || backend.focuses != 0 {
		t.Fatalf("cohort refusal mutated backend creates=%d closes=%d focuses=%d", backend.creates, backend.closes, backend.focuses)
	}
	bindingAfter, err := os.ReadFile(BindingPath(req.Root.Base()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bindingBefore, bindingAfter) {
		t.Fatalf("cohort refusal mutated binding")
	}
	if _, err := LoadJournal(req.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cohort refusal wrote a journal: %v", err)
	}
	if _, err := LoadExecutionTicket(req.Root, "claude"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cohort refusal wrote a claude ticket: %v", err)
	}
	if _, err := LoadExecutionTicket(req.Root, "codex"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cohort refusal wrote a codex ticket: %v", err)
	}
	if _, err := os.Stat(ConversationPath(req.Root.Base(), "claude")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cohort refusal wrote a claude conversation")
	}
	if _, err := os.Stat(ConversationPath(req.Root.Base(), "codex")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cohort refusal wrote a codex conversation")
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

func TestReconcileInstanceReuseNeverClosesUnrelatedResource(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectPresent}
	req := reconcileFixture(t, backend)
	writeReconcileBinding(t, req, backend, backend.Detect().Profile.Identity())
	record, err := LoadBinding(req.Root)
	if err != nil {
		t.Fatal(err)
	}
	record.InstanceIdentity = "instance:reused"
	lease, err := AcquireLease(req.Root, testLaunchNonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBinding(req.Root, lease, record); err != nil {
		t.Fatal(err)
	}
	_ = lease.Release()
	req.Rebind = true
	req.ConfirmRebind = func(BindingRecord, bool) (RebindDisposition, bool, error) { return RebindClose, true, nil }
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Reason != "foreign_rebind_requires_leave" || backend.closes != 0 || backend.creates != 0 {
		t.Fatalf("result=%#v closes=%d creates=%d", result, backend.closes, backend.creates)
	}
}

func TestReconcileAutoSelectsPreferenceWhenNotInsideCmux(t *testing.T) {
	tmux := &reconcileBackend{name: LauncherTMux, inspect: InspectAbsent}
	cmux := &reconcileBackend{name: LauncherCMux, inspect: InspectAbsent}
	req := reconcileFixture(t, tmux)
	req.Backends[LauncherCMux] = cmux
	req.Launcher = LauncherAuto
	req.Preferences = []string{LauncherTMux, LauncherCommands}
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != LauncherTMux || tmux.creates != 1 || cmux.creates != 0 {
		t.Fatalf("ping-live without inside-cmux selected %#v tmux=%d cmux=%d", result, tmux.creates, cmux.creates)
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

func TestReconcileExplicitLauncherWinsInsideCmux(t *testing.T) {
	tmux := &reconcileBackend{name: LauncherTMux, inspect: InspectAbsent}
	cmux := &reconcileBackend{name: LauncherCMux, inspect: InspectAbsent}
	req := reconcileFixture(t, tmux)
	req.Backends[LauncherCMux] = cmux
	req.Launcher = LauncherTMux
	req.Preferences = []string{LauncherCMux, LauncherTMux}
	t.Setenv("CMUX_SURFACE_ID", "F901D722-6789-4BBB-9818-C4E97F20BEB3")
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != LauncherTMux || tmux.creates != 1 || cmux.creates != 0 {
		t.Fatalf("explicit launcher = %#v tmux=%d cmux=%d", result, tmux.creates, cmux.creates)
	}
}

func TestReconcileAbsentBindingAutoSelectsPreferredBackend(t *testing.T) {
	old := &reconcileBackend{name: "old", inspect: InspectAbsent}
	preferred := &reconcileBackend{name: "preferred", inspect: InspectAbsent}
	req := reconcileFixture(t, old)
	req.Backends["preferred"] = preferred
	req.Launcher = LauncherAuto
	req.Preferences = []string{"preferred", "old"}
	writeReconcileBinding(t, req, old, old.Detect().Profile.Identity())
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 0 || result.Backend != "preferred" || preferred.creates != 1 || old.creates != 0 || old.closes != 0 {
		t.Fatalf("result=%#v preferred creates=%d old creates=%d closes=%d", result, preferred.creates, old.creates, old.closes)
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

func TestReconcileResumeDistinguishesNoSavedConversationFromStale(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	req := reconcileFixture(t, backend)
	req.ResumeOnly = true
	missing, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if missing.AggregateCode != 6 || missing.Outcome != OutcomeActionRequired || missing.Reason != ReasonNoSavedConversation ||
		missing.Agents[0].ConversationDisposition != DispositionActionRequired || missing.Agents[0].Reason != ReasonNoSavedConversation || backend.creates != 0 {
		t.Fatalf("missing result=%#v creates=%d", missing, backend.creates)
	}

	writeReconcileConversation(t, req, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureStale,
		LaunchNonce: testLaunchNonce, Reason: CaptureReasonEvidenceMissing,
	})
	stale, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if stale.AggregateCode != 6 || stale.Outcome != OutcomeActionRequired || stale.Reason != ReasonStaleConversation ||
		stale.Agents[0].ConversationDisposition != DispositionDegraded || stale.Agents[0].Reason != ReasonStaleConversation || backend.creates != 0 {
		t.Fatalf("stale result=%#v creates=%d", stale, backend.creates)
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

func TestReconcilePlanOnlyMintWithoutExecutionRemintsPending(t *testing.T) {
	backend := Commands{}
	req := reconcileFixture(t, backend)
	first, err := Reconcile(req)
	if err != nil || first.AggregateCode != 6 || first.Outcome != OutcomeCommandsEmitted || first.Plan == nil {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	firstID := first.Plan.Agents[0].ConversationID
	firstRecord, err := LoadConversation(req.Root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if firstRecord.State != CapturePending || firstRecord.Identity.ID != "" || firstRecord.ExecutionEvidence != nil || firstRecord.LaunchNonce != firstID {
		t.Fatalf("first record=%#v, want pending nonce %q without evidence", firstRecord, firstID)
	}
	ticket, err := LoadExecutionTicket(req.Root, "claude")
	if err != nil || ticket.State != ExecutionPending || ticket.LaunchNonce != firstID || !slices.Equal(ticket.TargetArgv, first.Plan.Agents[0].Argv) {
		t.Fatalf("first execution ticket=%#v err=%v", ticket, err)
	}
	resumeReq := req
	resumeReq.ResumeOnly = true
	resume, err := Reconcile(resumeReq)
	if err != nil || resume.AggregateCode != 6 || resume.Reason != ReasonStaleConversation || resume.Plan != nil {
		t.Fatalf("resume pending result=%#v err=%v", resume, err)
	}
	unchanged, err := LoadConversation(req.Root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.LaunchNonce != firstID {
		t.Fatalf("resume reminted pending record: got %q want %q", unchanged.LaunchNonce, firstID)
	}

	second, err := Reconcile(req)
	if err != nil || second.AggregateCode != 6 || second.Outcome != OutcomeCommandsEmitted || second.Plan == nil {
		t.Fatalf("second result=%#v err=%v", second, err)
	}
	secondID := second.Plan.Agents[0].ConversationID
	if secondID == firstID || second.Agents[0].ConversationDisposition != DispositionFresh || second.Agents[0].Reason != ReasonPriorLaunchNotExecuted {
		t.Fatalf("second result=%#v, first ID=%q second ID=%q", second, firstID, secondID)
	}
	secondRecord, err := LoadConversation(req.Root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if secondRecord.State != CapturePending || secondRecord.LaunchNonce != secondID || secondRecord.ExecutionEvidence != nil {
		t.Fatalf("second record=%#v, want new pending nonce %q", secondRecord, secondID)
	}
}

func TestReconcileMintExecutionEvidencePromotesThenResumesExactIdentity(t *testing.T) {
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
	if record.State != CaptureReady || record.Identity.ID != readyID || record.ExecutionEvidence == nil || record.ExecutionEvidence.Backend != managed.name {
		t.Fatalf("promoted record=%#v", record)
	}

	third, err := Reconcile(req)
	if err != nil || third.AggregateCode != 0 || third.Plan == nil {
		t.Fatalf("third result=%#v err=%v", third, err)
	}
	if third.Agents[0].ConversationDisposition != DispositionResumed || third.Plan.Agents[0].ConversationID != readyID {
		t.Fatalf("third result=%#v, want resumed ID %q", third, readyID)
	}
}

func TestReconcileInvalidCreatedBindingCannotPromoteMint(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent, invalidBind: true}
	req := reconcileFixture(t, backend)
	if _, err := Reconcile(req); err == nil || !strings.Contains(err.Error(), "invalid binding") {
		t.Fatalf("Reconcile error = %v", err)
	}
	if _, err := LoadConversation(req.Root, "claude"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid binding published conversation state: %v", err)
	}
}

func TestReconcileReadyRecordWithoutExecutionEvidenceFailsClosed(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	req := reconcileFixture(t, backend)
	req.ResumeOnly = true
	data, err := json.Marshal(ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureReady,
		Identity:    ConversationIdentity{Provider: "claude", ID: testConversationID},
		LaunchNonce: testLaunchNonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := req.Root.WriteFileAtomic(conversationDir, "claude.json", append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Outcome != OutcomeActionRequired || result.Agents[0].Reason != "conversation_state_unreadable" || backend.creates != 0 || result.Plan != nil {
		t.Fatalf("result=%#v creates=%d", result, backend.creates)
	}
}

func TestAggregateReconcileCodePrecedence(t *testing.T) {
	agents := []AgentReconcileResult{
		{Handle: "failure", Code: 1, ConversationDisposition: DispositionDegraded},
		{Handle: "timeout", Code: 4, ConversationDisposition: DispositionDegraded},
		{Handle: "action", Code: 6, ConversationDisposition: DispositionDegraded},
		{Handle: "expected", Code: 6, ConversationDisposition: DispositionUnsupported},
	}
	if got := aggregateReconcileCode(agents); got != 6 {
		t.Fatalf("aggregate = %d, want 6", got)
	}
	if got := aggregateReconcileCode(agents[:2]); got != 4 {
		t.Fatalf("aggregate without action = %d, want 4", got)
	}
	if got := aggregateReconcileCode(agents[3:]); got != 0 {
		t.Fatalf("expected unsupported aggregate = %d, want 0", got)
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

func TestReconcileThreeAgentRosterCreatesOneLayoutAndThreeRefs(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	req := reconcileFixture(t, backend)
	for _, handle := range []string{"peer", "third"} {
		req.Config.Agents = append(req.Config.Agents, ProjectAgentConfig{
			Handle: handle, Adapter: "claude", Command: []string{"claude"}, ResumePolicy: ResumeEnabled,
		})
	}
	result, err := Reconcile(req)
	if err != nil || result.AggregateCode != 0 || backend.creates != 1 || result.Plan == nil || len(result.Plan.Agents) != 3 {
		t.Fatalf("result=%#v creates=%d err=%v", result, backend.creates, err)
	}
	for _, handle := range []string{"claude", "peer", "third"} {
		if _, err := LoadConversation(req.Root, handle); err != nil {
			t.Fatalf("conversation %s: %v", handle, err)
		}
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
			if err != nil || result.AggregateCode != 0 {
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
			if err != nil || record.State != CaptureReady || record.ExecutionEvidence == nil {
				t.Fatalf("conversation after recovery=%#v err=%v", record, err)
			}
		})
	}
}

func TestReconcileCrashRecoveryPreservesExistingConversationIdentity(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	req := reconcileFixture(t, backend)
	writeReconcileConversation(t, req, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureReady,
		Identity: ConversationIdentity{Provider: "claude", ID: testConversationID}, LaunchNonce: testLaunchNonce,
		ExecutionEvidence: reconcileExecutionEvidence(backend, testLaunchNonce),
	})
	crash := errors.New("injected crash")
	req.CrashHook = func(stage string) error {
		if stage == "backend_created" {
			return crash
		}
		return nil
	}
	first, err := Reconcile(req)
	if !errors.Is(err, crash) || first.Plan == nil || first.Plan.Agents[0].ConversationID != testConversationID {
		t.Fatalf("first result=%#v err=%v", first, err)
	}
	journal, err := LoadJournal(req.Root)
	if err != nil || journal.Conversations[0].Identity.ID != testConversationID || journal.Conversations[0].LaunchNonce != testLaunchNonce {
		t.Fatalf("journal=%#v err=%v", journal, err)
	}
	req.CrashHook = nil
	result, err := Reconcile(req)
	if err != nil || result.AggregateCode != 0 || backend.creates != 1 {
		t.Fatalf("recovery result=%#v creates=%d err=%v", result, backend.creates, err)
	}
	record, err := LoadConversation(req.Root, "claude")
	if err != nil || record.Identity.ID != testConversationID || record.LaunchNonce != testLaunchNonce {
		t.Fatalf("conversation=%#v err=%v", record, err)
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

func TestReconcileRevalidatesAdoptionImmediatelyBeforeBinding(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent, reclaimFlip: true}
	req := reconcileFixture(t, backend)
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
	if err != nil || result.AggregateCode != 6 || result.Reason != "launch_recovery_changed" || backend.reclaims != 2 {
		t.Fatalf("result=%#v reclaims=%d err=%v", result, backend.reclaims, err)
	}
	if _, err := LoadBinding(req.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed adoption published binding: %v", err)
	}
	if _, err := LoadJournal(req.Root); err != nil {
		t.Fatalf("changed adoption lost journal: %v", err)
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

func TestReconcileCaptureEvidencePublicationCrashRecovery(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := map[bool]string{false: "verified reuse", true: "corrupt collision"}[corrupt]
		t.Run(name, func(t *testing.T) {
			backend := &reconcileBackend{name: "test", inspect: InspectAbsent, capture: true}
			req := reconcileFixture(t, backend)
			req.Config.Agents[0].Adapter = CodexProvider
			req.Config.Agents[0].Command = []string{CodexProvider}
			req.Adapters = map[string]HarnessAdapter{CodexProvider: reconcileAdapter{name: CodexProvider, mode: AdapterModeCapture, available: true}}
			crash := errors.New("injected crash after evidence publication")
			req.CrashHook = func(stage string) error {
				if stage == "evidence_written" {
					return crash
				}
				return nil
			}
			if _, err := Reconcile(req); !errors.Is(err, crash) {
				t.Fatalf("crash error = %v", err)
			}
			journalBefore, err := LoadJournal(req.Root)
			if err != nil {
				t.Fatal(err)
			}
			if backend.captured == nil {
				t.Fatal("backend did not retain capture evidence")
			}
			_, expectedRef, _, err := prepareEvidence(EvidenceWriteRequest{
				Kind: EvidenceProviderCapture, Handle: "claude", ObservedAt: backend.captured.observedAt, Payload: backend.captured.payload,
			})
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				if err := os.WriteFile(req.Root.DisplayPath(filepath.Join(evidenceDirectory, evidenceFilename(expectedRef.ID))), []byte(`{"different":true}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			req.CrashHook = nil
			result, recoveryErr := Reconcile(req)
			if corrupt {
				var evidenceErr *EvidenceCorruptError
				if !errors.As(recoveryErr, &evidenceErr) {
					t.Fatalf("corrupt collision error = %v", recoveryErr)
				}
				journalAfter, err := LoadJournal(req.Root)
				if err != nil || !reflect.DeepEqual(journalAfter, journalBefore) {
					t.Fatalf("journal changed after corrupt collision: before=%#v after=%#v err=%v", journalBefore, journalAfter, err)
				}
				return
			}
			if recoveryErr != nil || result.AggregateCode != 0 || backend.creates != 1 {
				t.Fatalf("recovery result=%#v creates=%d err=%v", result, backend.creates, recoveryErr)
			}
			record, err := LoadConversation(req.Root, "claude")
			if err != nil || !reflect.DeepEqual(record.EvidenceRefs, []string{expectedRef.ID}) {
				t.Fatalf("recovered conversation=%#v err=%v", record, err)
			}
		})
	}
}
