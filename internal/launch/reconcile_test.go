package launch

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type reconcileAdapter struct {
	name      string
	mode      AdapterMode
	available bool
	reason    string
}

func (a reconcileAdapter) Name() string               { return a.name }
func (a reconcileAdapter) Mode() AdapterMode          { return a.mode }
func (a reconcileAdapter) CommittedEnvKeys() []string { return nil }
func (a reconcileAdapter) Capabilities(context.Context) AdapterCapabilities {
	return AdapterCapabilities{
		Provider: a.name, Mode: a.mode, Available: a.available,
		ProviderVersion: "test", Fresh: a.available, Resume: a.available,
		Capture: a.available && a.mode == AdapterModeCapture, Reason: a.reason,
	}
}
func (a reconcileAdapter) PlanFresh(req PlanRequest) (AgentPlan, error) {
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
}

func (b *reconcileBackend) Detect() DetectResult {
	profile := Profile{Backend: b.name, Platform: "test", VersionRange: "*", Version: 1, Capabilities: []Capability{CapCreate, CapInspect, CapClose, CapFocus}}
	return DetectResult{
		Available: true, Profile: profile, Effective: profile.Capabilities,
		HostIdentity: "host:test", InstanceIdentity: "instance:test",
	}
}
func (b *reconcileBackend) Create(req CreateRequest) (CreateResult, error) {
	b.mu.Lock()
	b.creates++
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
	result := CreateResult{Outcome: OutcomeCreated, Profile: b.Detect().Profile.Identity(), Binding: BindingRecord{
		Version: BindingVersion, Backend: b.name, HostIdentity: "host:test", InstanceIdentity: "instance:test",
		Profile: b.Detect().Profile.Identity(), LaunchNonce: req.Plan.Agents[0].LaunchNonce,
		Resources: ResourceIdentitySet{Version: ResourceSetVersion, Resources: []ResourceIdentity{{OpaqueID: "resource:test"}}},
	}}
	if b.capture {
		evidence, err := ParseCodexThreadStartedEvidence([]byte(`{"method":"thread/started","params":{"thread":{"id":"019c8a2f-2b13-7000-8000-000000000001","cliVersion":"test"}}}`), req.Plan.Agents[0].LaunchNonce, false)
		if err != nil {
			return CreateResult{}, err
		}
		result.CaptureEvidence = map[string][]CaptureEvidence{req.Plan.Agents[0].Handle: {evidence}}
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
		Identity:    ConversationIdentity{Provider: "claude", ID: testConversationID},
		LaunchNonce: testLaunchNonce,
	})
	result, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || backend.creates != 1 || result.Plan == nil || len(result.Plan.Agents) != 1 || result.Plan.Agents[0].Handle != "peer" {
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
	if result.AggregateCode != 0 || result.Agents[0].ConversationDisposition != DispositionUnsupported || backend.creates != 0 {
		t.Fatalf("roster drift result=%#v", result)
	}
}

func TestReconcileCrashAfterCreateReleasesLeaseForRecovery(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
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
}

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
	if record.State != CaptureReady || record.Identity.Provider != CodexProvider || record.Identity.ID != "019c8a2f-2b13-7000-8000-000000000001" {
		t.Fatalf("record=%#v", record)
	}
}
