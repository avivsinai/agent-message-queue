package launch

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLaunchJournalRequiresLeaseAndClearsOnlyExactRecord(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	request := reconcileFixture(t, backend)
	nonce := "019c8a2f-2b13-7000-8000-000000000010"
	plan, agents, conversations := journalFixturePlan(nonce)
	plan.Agents[0].Execution = &PrepareExecutionOptions{
		RequireWake: true, NoGitignore: true, WakeMode: "enabled",
		InjectorMode: "raw", InjectorVia: "/opt/amq/inject", InjectorArgs: []string{"send"},
		SymphonyEvents: []string{"after_create", "before_run", "after_run", "before_remove"}, SymphonyWorkspaceKey: "team-17",
	}
	digest, err := plan.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewLaunchJournal(request, backend.name, backend.Detect(), plan, digest, nonce, agents, conversations, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJournal(request.Root, nil, record); err == nil {
		t.Fatal("WriteJournal without lease succeeded")
	}
	lease, err := AcquireLease(request.Root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJournal(request.Root, lease, record); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadJournal(request.Root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Plan.Agents[0].Execution, plan.Agents[0].Execution) {
		t.Fatalf("journal execution options = %#v, want %#v", loaded.Plan.Agents[0].Execution, plan.Agents[0].Execution)
	}
	if loaded.Placement.Effective != LegacyPlacement(backend.name) || loaded.Placement.Requested != nil {
		t.Fatalf("omitted journal placement = %#v", loaded.Placement)
	}
	request.ExecutionOptions = map[string]PrepareExecutionOptions{"claude": *plan.Agents[0].Execution}
	if err := loaded.ValidateRequest(request); err != nil {
		t.Fatalf("journal rejected matching execution options: %v", err)
	}
	changedOptions := request.ExecutionOptions["claude"]
	changedOptions.NoGitignore = !changedOptions.NoGitignore
	request.ExecutionOptions["claude"] = changedOptions
	if err := loaded.ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "execution options changed") {
		t.Fatalf("journal accepted changed execution options: %v", err)
	}
	changed := record
	changed.CreatedAt = changed.CreatedAt.Add(time.Second)
	if err := ClearJournal(request.Root, lease, changed); err == nil {
		t.Fatal("ClearJournal removed a different record")
	}
	if err := ClearJournal(request.Root, lease, loaded); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJournal(request.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal after clear: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchJournalRejectsPlanRosterAndBindingDrift(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	request := reconcileFixture(t, backend)
	nonce := "019c8a2f-2b13-7000-8000-000000000011"
	plan, agents, conversations := journalFixturePlan(nonce)
	digest, err := plan.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewLaunchJournal(request, backend.name, backend.Detect(), plan, digest, nonce, agents, conversations, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	drifted := record
	drifted.PlanDigest = "sha256:" + strings.Repeat("0", 64)
	if err := drifted.Validate(); err == nil {
		t.Fatal("journal accepted a different plan digest")
	}
	drifted = record
	drifted.Conversations = nil
	if err := drifted.Validate(); err == nil {
		t.Fatal("journal accepted a different roster")
	}
	drifted = record
	drifted.Phase = JournalCreated
	drifted.Binding = &BindingRecord{
		Version: BindingVersion, Backend: backend.name, HostIdentity: "host:test", InstanceIdentity: "instance:test",
		Profile: backend.Detect().Profile.Identity(), LaunchNonce: testLaunchNonce,
		Resources: ResourceIdentitySet{Version: ResourceSetVersion, Resources: []ResourceIdentity{{OpaqueID: "resource:test"}}},
	}
	if err := drifted.Validate(); err == nil {
		t.Fatal("journal accepted a binding from a different launch generation")
	}
	originalProject := request.ProjectRoot + "-original"
	if err := os.Rename(request.ProjectRoot, originalProject); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(originalProject) })
	if err := os.Mkdir(request.ProjectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := record.ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "physical") {
		t.Fatalf("journal accepted replacement project directory: %v", err)
	}
	if err := os.Remove(request.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalProject, request.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	request.Config.Agents[0].Env = map[string]string{"LANG": "C"}
	if err := record.ValidateRequest(request); err == nil {
		t.Fatal("journal accepted roster drift")
	}
}

func TestLaunchJournalRejectsExtraAgentRecord(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	request := reconcileFixture(t, backend)
	nonce := "019c8a2f-2b13-7000-8000-000000000013"
	plan, agents, conversations := journalFixturePlan(nonce)
	digest, err := plan.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewLaunchJournal(request, backend.name, backend.Detect(), plan, digest, nonce, agents, conversations, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	record.Agents = append(record.Agents, AgentReconcileResult{Handle: "operator", ConversationDisposition: DispositionFresh})
	if err := record.Validate(); err == nil || !strings.Contains(err.Error(), "roster lengths") {
		t.Fatalf("journal accepted extra agent record: %v", err)
	}
}

func TestNewLaunchJournalFailsClosedOnPhysicalIdentityProbeError(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	request := reconcileFixture(t, backend)
	nonce := "019c8a2f-2b13-7000-8000-000000000014"
	plan, agents, conversations := journalFixturePlan(nonce)
	digest, err := plan.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	probeErr := errors.New("injected physical identity failure")
	oldTree := stableTreeIdentity
	oldTreeInfo := stableTreeIdentityInfo
	t.Cleanup(func() {
		stableTreeIdentity = oldTree
		stableTreeIdentityInfo = oldTreeInfo
	})
	stableTreeIdentity = func(string) (string, error) { return "", probeErr }
	if _, err := NewLaunchJournal(request, backend.name, backend.Detect(), plan, digest, nonce, agents, conversations, time.Now()); !errors.Is(err, probeErr) {
		t.Fatalf("NewLaunchJournal = %v, want physical identity failure", err)
	}

	stableTreeIdentity = oldTree
	stableTreeIdentityInfo = func(os.FileInfo) (string, error) { return "", probeErr }
	if _, err := NewLaunchJournal(request, backend.name, backend.Detect(), plan, digest, nonce, agents, conversations, time.Now()); !errors.Is(err, probeErr) {
		t.Fatalf("NewLaunchJournal root probe = %v, want physical identity failure", err)
	}
}

func TestLaunchJournalValidateRequestFailsClosedOnProjectPhysicalIdentityProbeError(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	request := reconcileFixture(t, backend)
	nonce := "019c8a2f-2b13-7000-8000-000000000015"
	plan, agents, conversations := journalFixturePlan(nonce)
	digest, err := plan.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewLaunchJournal(request, backend.name, backend.Detect(), plan, digest, nonce, agents, conversations, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	probeErr := errors.New("injected project physical identity failure")
	oldTree := stableTreeIdentity
	stableTreeIdentity = func(string) (string, error) { return "", probeErr }
	t.Cleanup(func() { stableTreeIdentity = oldTree })
	if err := record.ValidateRequest(request); !errors.Is(err, probeErr) || !strings.Contains(err.Error(), "resolve project physical identity") {
		t.Fatalf("ValidateRequest = %v, want wrapped project physical identity error", err)
	}
}

func journalFixturePlan(nonce string) (Plan, []AgentReconcileResult, []ConversationRecord) {
	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{{
		Handle: "claude", Argv: []string{"/usr/bin/true", nonce}, Cwd: "/tmp",
		AdapterMode: AdapterModeMint, ResumePolicy: ResumeEnabled, LaunchNonce: nonce,
		ConversationID: nonce, DynamicArgv: []DynamicArg{{Index: 1, Kind: DynamicArgLaunchNonce}},
	}}}
	agents := []AgentReconcileResult{{Handle: "claude", ConversationDisposition: DispositionFresh}}
	conversations := []ConversationRecord{{
		Version: ConversationVersion, Handle: "claude", State: CapturePending,
		ProviderVersion: "test", LaunchNonce: nonce,
	}}
	return plan, agents, conversations
}

func TestLaunchJournalPersistsPlacementAndRejectsDrift(t *testing.T) {
	backend := &reconcileBackend{name: LauncherTMux, inspect: InspectAbsent}
	request := reconcileFixture(t, backend)
	request.Placement = &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutRows, StaggerMS: 250}
	nonce := "019c8a2f-2b13-7000-8000-000000000012"
	plan, agents, conversations := journalFixturePlan(nonce)
	digest, err := plan.SemanticDigest()
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewLaunchJournal(request, backend.name, backend.Detect(), plan, digest, nonce, agents, conversations, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if record.Placement.Requested == nil || record.Placement.Effective.Target != PlacementTargetNewWindow ||
		record.Placement.Effective.Layout != PlacementLayoutRows || record.Placement.Effective.StaggerMS != 250 {
		t.Fatalf("journal placement = %#v", record.Placement)
	}
	if err := record.ValidateRequest(request); err != nil {
		t.Fatalf("matching placement rejected: %v", err)
	}
	request.Placement = &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns}
	if err := record.ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "placement changed") {
		t.Fatalf("journal accepted placement drift: %v", err)
	}
}
