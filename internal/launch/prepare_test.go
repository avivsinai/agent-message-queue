package launch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestPrepareRejectsUnknownSubjectSchemaBeforeInspection(t *testing.T) {
	_, err := Prepare(context.Background(), PrepareRequest{
		IntentDigest:  "sha256:" + strings.Repeat("a", 64),
		Participants:  []PrepareParticipant{{Handle: "operator"}},
		SubjectSchema: 3,
	}, PrepareDependencies{})
	if err == nil || !strings.Contains(err.Error(), "unsupported subject schema") {
		t.Fatalf("Prepare error = %v", err)
	}
}

func TestPrepareRejectsPlacementOnSubjectSchemaV1BeforeInspection(t *testing.T) {
	_, err := Prepare(context.Background(), PrepareRequest{
		IntentDigest:  "sha256:" + strings.Repeat("a", 64),
		Participants:  []PrepareParticipant{{Handle: "operator"}},
		SubjectSchema: SubjectSchemaV1,
		Placement:     &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns},
	}, PrepareDependencies{})
	if err == nil || !strings.Contains(err.Error(), "placement requires subject schema") {
		t.Fatalf("Prepare error = %v", err)
	}
}

func TestPrepareRejectsInvalidPlacementBeforeInspection(t *testing.T) {
	_, err := Prepare(context.Background(), PrepareRequest{
		IntentDigest: "sha256:" + strings.Repeat("a", 64),
		Participants: []PrepareParticipant{{Handle: "operator"}},
		Placement:    &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns, LauncherPane: "%1"},
	}, PrepareDependencies{})
	if err == nil || !strings.Contains(err.Error(), "launcher_pane") {
		t.Fatalf("Prepare error = %v", err)
	}
}

func TestPrepareUnsupportedPlacementSkipsTrustAndCreate(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	fixture.request.Placement = &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns}
	backend := &prepareTestBackend{}
	before := prepareTreeSnapshot(t, fixture.root)
	result, err := Prepare(context.Background(), fixture.request, fixture.dependencies(backend))
	if err != nil {
		t.Fatal(err)
	}
	if after := prepareTreeSnapshot(t, fixture.root); after != before {
		t.Fatalf("unsupported placement Prepare changed filesystem: before %s after %s", before, after)
	}
	if backend.creates != 0 {
		t.Fatalf("Prepare created: %d", backend.creates)
	}
	if result.Outcome != PrepareOutcomeUnsupported || result.Reason != PlacementUnsupportedReason || len(result.RequiredActions) != 0 {
		t.Fatalf("unsupported placement = %#v", result)
	}
	if result.Placement.Supported || result.Placement.ReasonCode != PlacementUnsupportedReason {
		t.Fatalf("placement preview = %#v", result.Placement)
	}
}

func TestPrepareRejectsOnLiveKeepOnSubjectSchemaV1BeforeInspection(t *testing.T) {
	_, err := Prepare(context.Background(), PrepareRequest{
		IntentDigest:  "sha256:" + strings.Repeat("a", 64),
		Participants:  []PrepareParticipant{{Handle: "operator", OnLive: OnLiveKeep}},
		SubjectSchema: SubjectSchemaV1,
	}, PrepareDependencies{})
	if err == nil || !strings.Contains(err.Error(), "on_live keep requires subject schema") {
		t.Fatalf("Prepare error = %v", err)
	}
}

func TestPrepareV2SubjectDigestIncludesOnLiveKeepOnly(t *testing.T) {
	// Digest-stability guard: omit and explicit refuse share subject/trust
	// digests; keep is the only on_live value that enters them.
	fixture := newInternalPrepareFixture(t)
	omitted, err := Prepare(context.Background(), fixture.request, fixture.dependencies(&prepareTestBackend{}))
	if err != nil {
		t.Fatal(err)
	}
	refused := fixture.request
	refused.Participants[0].OnLive = OnLiveRefuse
	withRefuse, err := Prepare(context.Background(), refused, fixture.dependencies(&prepareTestBackend{}))
	if err != nil {
		t.Fatal(err)
	}
	if omitted.SubjectDigest != withRefuse.SubjectDigest || omitted.TrustDigest != withRefuse.TrustDigest {
		t.Fatalf("explicit refuse changed digests omit=%s/%s refuse=%s/%s",
			omitted.SubjectDigest, omitted.TrustDigest, withRefuse.SubjectDigest, withRefuse.TrustDigest)
	}
	kept := fixture.request
	kept.Participants[0].OnLive = OnLiveKeep
	withKeep, err := Prepare(context.Background(), kept, fixture.dependencies(&prepareTestBackend{}))
	if err != nil {
		t.Fatal(err)
	}
	if withKeep.SubjectDigest == omitted.SubjectDigest {
		t.Fatal("on_live keep did not enter the v2 subject digest")
	}
	if withKeep.TrustDigest == omitted.TrustDigest {
		t.Fatal("on_live keep did not enter the trust digest")
	}
}

func TestPrepareV2SubjectDigestIncludesPlacement(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	omitted, err := Prepare(context.Background(), fixture.request, fixture.dependencies(&prepareTestBackend{}))
	if err != nil {
		t.Fatal(err)
	}
	v1Request := fixture.request
	v1Request.SubjectSchema = SubjectSchemaV1
	v1, err := Prepare(context.Background(), v1Request, fixture.dependencies(&prepareTestBackend{}))
	if err != nil {
		t.Fatal(err)
	}
	if omitted.SubjectSchema != SubjectSchemaV2 || omitted.SubjectDigest == v1.SubjectDigest {
		t.Fatalf("v2 omitted digest did not diverge from v1: v1=%s v2=%s", v1.SubjectDigest, omitted.SubjectDigest)
	}
	staggered := fixture.request
	staggered.Launcher = LauncherTMux
	staggered.Placement = &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns, StaggerMS: 250}
	tmux := &prepareTestBackend{}
	withStagger, err := Prepare(context.Background(), staggered, PrepareDependencies{
		Backends: map[string]Backend{LauncherTMux: tmux}, Preferences: []string{LauncherTMux},
		AdapterFor:   func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		HostIdentity: "host:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline := fixture.request
	baseline.Launcher = LauncherTMux
	baseline.Placement = &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns}
	withoutStagger, err := Prepare(context.Background(), baseline, PrepareDependencies{
		Backends: map[string]Backend{LauncherTMux: tmux}, Preferences: []string{LauncherTMux},
		AdapterFor:   func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		HostIdentity: "host:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if withStagger.SubjectDigest == withoutStagger.SubjectDigest {
		t.Fatal("stagger_ms did not enter the v2 subject digest")
	}
	if withStagger.Outcome == PrepareOutcomeUnsupported || withoutStagger.Outcome == PrepareOutcomeUnsupported {
		t.Fatalf("tmux session placement unsupported: stagger=%#v baseline=%#v", withStagger, withoutStagger)
	}
}

func TestPrepareSubjectSchemasProduceDistinctDigestsForSameState(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	v1Request := fixture.request
	v1Request.SubjectSchema = SubjectSchemaV1
	v1, err := Prepare(context.Background(), v1Request, fixture.dependencies(&prepareTestBackend{}))
	if err != nil {
		t.Fatal(err)
	}
	v2Request := fixture.request
	v2Request.SubjectSchema = SubjectSchemaV2
	v2, err := Prepare(context.Background(), v2Request, fixture.dependencies(&prepareTestBackend{}))
	if err != nil {
		t.Fatal(err)
	}
	if v1.SubjectSchema != SubjectSchemaV1 || v2.SubjectSchema != SubjectSchemaV2 {
		t.Fatalf("subject schemas = %d/%d", v1.SubjectSchema, v2.SubjectSchema)
	}
	if v1.PlanDigest != v2.PlanDigest || v1.TrustDigest != v2.TrustDigest || v1.SubjectDigest == v2.SubjectDigest {
		t.Fatalf("same-state digests: plan equal=%t trust equal=%t subject different=%t\nv1=%s\nv2=%s",
			v1.PlanDigest == v2.PlanDigest, v1.TrustDigest == v2.TrustDigest, v1.SubjectDigest != v2.SubjectDigest,
			v1.SubjectDigest, v2.SubjectDigest)
	}
}

func TestPrepareOwningLayerIsZeroWriteAndBuildsTypedActions(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	writePrepareConversation(t, fixture.sessionRoot, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureStale,
		LaunchNonce: "11111111-1111-4111-8111-111111111111", Reason: CaptureReasonEvidenceMissing,
	})
	writePrepareBinding(t, fixture.sessionRoot, prepareBinding("host:test", "instance:test", "11111111-1111-4111-8111-111111111111"))
	backend := &prepareTestBackend{}
	before := prepareTreeSnapshot(t, fixture.root)

	first, err := Prepare(context.Background(), fixture.request, fixture.dependencies(backend))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(context.Background(), fixture.request, fixture.dependencies(backend))
	if err != nil {
		t.Fatal(err)
	}
	after := prepareTreeSnapshot(t, fixture.root)
	if before != after {
		t.Fatalf("Prepare changed filesystem: before %s, after %s", before, after)
	}
	if backend.detects != 2 || backend.creates != 0 || backend.closes != 0 || backend.focuses != 0 || backend.inspects != 2 {
		t.Fatalf("backend calls = detect:%d inspect:%d create:%d close:%d focus:%d", backend.detects, backend.inspects, backend.creates, backend.closes, backend.focuses)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated Prepare changed result:\n%#v\n%#v", first, second)
	}
	if first.Outcome != PrepareOutcomeActionRequired || len(first.RequiredActions) != 2 {
		t.Fatalf("Prepare result = %#v", first)
	}
	kinds := []RequiredActionKind{first.RequiredActions[0].Kind, first.RequiredActions[1].Kind}
	if !slices.Equal(kinds, []RequiredActionKind{ActionStaleConversation, ActionTrustConfirmation}) {
		t.Fatalf("action kinds = %v", kinds)
	}
	for _, action := range first.RequiredActions {
		if !validDigest(action.ActionID) {
			t.Fatalf("action ID = %q", action.ActionID)
		}
	}
	if got := first.Participants[0].Command.Argv[len(first.Participants[0].Command.Argv)-1]; got != "${launch_nonce}" {
		t.Fatalf("static command placeholder = %q", got)
	}
}

func TestPrepareSubjectDigestIncludesBindingObservation(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	backend := &prepareTestBackend{}
	writePrepareBinding(t, fixture.sessionRoot, prepareBinding("host:test", "instance:test", "11111111-1111-4111-8111-111111111111"))
	first, err := Prepare(context.Background(), fixture.request, fixture.dependencies(backend))
	if err != nil {
		t.Fatal(err)
	}
	writePrepareBinding(t, fixture.sessionRoot, prepareBinding("host:test", "instance:test", "22222222-2222-4222-8222-222222222222"))
	second, err := Prepare(context.Background(), fixture.request, fixture.dependencies(backend))
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest || first.SubjectDigest == second.SubjectDigest {
		t.Fatalf("binding-only digest change = plan:%t trust:%t subject:%t",
			first.PlanDigest != second.PlanDigest,
			first.TrustDigest != second.TrustDigest,
			first.SubjectDigest != second.SubjectDigest)
	}
}

func TestPrepareSubjectDigestIncludesPrivateConversationIdentity(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	backend := &prepareTestBackend{}
	firstID := "11111111-1111-4111-8111-111111111111"
	secondID := "22222222-2222-4222-8222-222222222222"
	writePrepareConversation(t, fixture.sessionRoot, readyPrepareConversation(firstID))
	first, err := Prepare(context.Background(), fixture.request, fixture.dependencies(backend))
	if err != nil {
		t.Fatal(err)
	}
	writePrepareConversation(t, fixture.sessionRoot, readyPrepareConversation(secondID))
	second, err := Prepare(context.Background(), fixture.request, fixture.dependencies(backend))
	if err != nil {
		t.Fatal(err)
	}
	if first.Observations[0].Conversation != second.Observations[0].Conversation ||
		first.PlanDigest != second.PlanDigest || first.TrustDigest != second.TrustDigest || first.SubjectDigest == second.SubjectDigest {
		t.Fatalf("conversation-identity-only change = public-state:%t plan:%t trust:%t subject:%t",
			first.Observations[0].Conversation != second.Observations[0].Conversation,
			first.PlanDigest != second.PlanDigest,
			first.TrustDigest != second.TrustDigest,
			first.SubjectDigest != second.SubjectDigest)
	}
}

func TestPrepareForeignBindingRestrictsRebindDecision(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	backend := &prepareTestBackend{}
	writePrepareBinding(t, fixture.sessionRoot, prepareBinding("host:foreign", "instance:test", "11111111-1111-4111-8111-111111111111"))
	result, err := Prepare(context.Background(), fixture.request, fixture.dependencies(backend))
	if err != nil {
		t.Fatal(err)
	}
	var rebind *PrepareRequiredAction
	for i := range result.RequiredActions {
		if result.RequiredActions[i].Kind == ActionRebindConfirmation {
			rebind = &result.RequiredActions[i]
		}
	}
	if rebind == nil || rebind.ReasonCode != "foreign_binding" || !slices.Equal(rebind.AllowedDecisions, []string{"leave_old", "abort"}) {
		t.Fatalf("foreign rebind action = %#v", rebind)
	}
	if backend.closes != 0 || backend.focuses != 0 || backend.creates != 0 {
		t.Fatalf("Prepare mutated foreign backend: %#v", backend)
	}
}

func TestPrepareCapturePendingRequiresDecisionWithoutFreshPlan(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	writePrepareConversation(t, fixture.sessionRoot, ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CapturePending,
		LaunchNonce: "11111111-1111-4111-8111-111111111111",
	})
	result, err := Prepare(context.Background(), fixture.request, fixture.dependencies(&prepareTestBackend{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RequiredActions) != 1 || result.RequiredActions[0].Kind != ActionStaleConversation {
		t.Fatalf("pending actions = %#v", result.RequiredActions)
	}
	if result.Participants[0].PlannedOutcome != "capture_pending" || result.Participants[0].Command != nil {
		t.Fatalf("pending participant = %#v", result.Participants[0])
	}
}

func TestPreparePinsMailboxConfigThroughFinalVerification(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	configPath := filepath.Join(fixture.sessionRoot, "meta", "config.json")
	if err := os.WriteFile(configPath, []byte(`{"version":1,"agents":["claude"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := fixture.dependencies(&prepareTestBackend{})
	dependencies.AdapterFor = func(provider, executable string) HarnessAdapter {
		return hookPrepareAdapter{
			prepareTestAdapter: prepareTestAdapter{provider: provider},
			beforePlan: func() {
				if err := os.WriteFile(configPath, []byte(`{"version":1,"agents":["claude","operator"]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		}
	}
	if _, err := Prepare(context.Background(), fixture.request, dependencies); err == nil {
		t.Fatal("Prepare accepted a mailbox config changed after roster inspection")
	}
}

func TestPrepareTreatsMissingRuntimeIdentityAsForeign(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	writePrepareBinding(t, fixture.sessionRoot, prepareBinding("host:test", "instance:test", "11111111-1111-4111-8111-111111111111"))
	backend := &missingIdentityPrepareBackend{}
	dependencies := PrepareDependencies{
		Backends: map[string]Backend{"test": backend}, Preferences: []string{"test"},
		AdapterFor:   func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		HostIdentity: "host:test",
	}
	result, err := Prepare(context.Background(), fixture.request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != PrepareOutcomeActionRequired {
		t.Fatalf("missing-identity outcome = %q", result.Outcome)
	}
	var rebind *PrepareRequiredAction
	for i := range result.RequiredActions {
		if result.RequiredActions[i].Kind == ActionRebindConfirmation {
			rebind = &result.RequiredActions[i]
		}
	}
	if rebind == nil || rebind.ReasonCode != "foreign_binding" || !slices.Equal(rebind.AllowedDecisions, []string{"leave_old", "abort"}) {
		t.Fatalf("missing-identity rebind = %#v", rebind)
	}
	if backend.inspects != 0 {
		t.Fatalf("Prepare inspected a binding with unknown runtime identity: %d", backend.inspects)
	}
}

func TestTmuxDetectDoesNotMutateBackend(t *testing.T) {
	backend := NewTmuxBackend("sh")
	backend.run = func(context.Context, ...string) (string, error) { return "tmux 3.4", nil }
	backend.hostname = func() (string, error) { return "host:test", nil }
	before := backend.binary
	if result := backend.Detect(); !result.Available {
		t.Fatalf("tmux detection = %#v", result)
	}
	if backend.binary != before {
		t.Fatalf("Detect mutated binary from %q to %q", before, backend.binary)
	}
}

func TestPrepareAutoFollowsPreferenceOutsideCmux(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	fixture.request.Launcher = LauncherAuto
	tmux := &prepareTestBackend{}
	cmux := &prepareTestBackend{}
	result, err := Prepare(context.Background(), fixture.request, PrepareDependencies{
		Backends:    map[string]Backend{LauncherCMux: cmux, LauncherTMux: tmux},
		Preferences: []string{LauncherTMux, LauncherCommands},
		AdapterFor: func(provider, executable string) HarnessAdapter {
			return prepareTestAdapter{provider: provider}
		},
		HostIdentity: "host:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != LauncherTMux {
		t.Fatalf("outside backend = %q", result.Backend)
	}
}

func TestPrepareAutoInsideCmuxPrependsCmux(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	fixture.request.Launcher = LauncherAuto
	tmux := &prepareTestBackend{}
	cmux := &prepareTestBackend{}
	t.Setenv("CMUX_SURFACE_ID", "F901D722-6789-4BBB-9818-C4E97F20BEB3")
	result, err := Prepare(context.Background(), fixture.request, PrepareDependencies{
		Backends:    map[string]Backend{LauncherCMux: cmux, LauncherTMux: tmux},
		Preferences: []string{LauncherTMux, LauncherCommands},
		AdapterFor: func(provider, executable string) HarnessAdapter {
			return prepareTestAdapter{provider: provider}
		},
		HostIdentity: "host:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != LauncherCMux {
		t.Fatalf("inside-cmux backend = %q", result.Backend)
	}
}

type internalPrepareFixture struct {
	root        string
	projectRoot string
	sessionRoot string
	cwd         string
	request     PrepareRequest
}

func newInternalPrepareFixture(t *testing.T) internalPrepareFixture {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	cwd := filepath.Join(root, "sibling")
	base := filepath.Join(root, "mail")
	session := filepath.Join(base, "collab")
	for _, path := range []string{project, cwd, base} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsq.EnsureRootDirs(session); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(session, "claude"); err != nil {
		t.Fatal(err)
	}
	return internalPrepareFixture{
		root: root, projectRoot: project, sessionRoot: session, cwd: cwd,
		request: PrepareRequest{
			Target:   PrepareTarget{ProjectRoot: project, SessionRoot: session, Session: "collab"},
			Launcher: "test", IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
			Participants: []PrepareParticipant{{
				Handle: "claude", Runnable: true, Provider: ClaudeProvider, Executable: "claude",
				Cwd: cwd, ResumePolicy: ResumeEnabled,
				Execution: PrepareExecutionOptions{WakeMode: "enabled"},
			}},
		},
	}
}

func (fixture internalPrepareFixture) dependencies(backend *prepareTestBackend) PrepareDependencies {
	return PrepareDependencies{
		Backends: map[string]Backend{"test": backend}, Preferences: []string{"test"},
		AdapterFor: func(provider, executable string) HarnessAdapter {
			return prepareTestAdapter{provider: provider}
		},
		HostIdentity: "host:test",
	}
}

type prepareTestAdapter struct{ provider string }

type hookPrepareAdapter struct {
	prepareTestAdapter
	beforePlan func()
}

func (adapter hookPrepareAdapter) PlanFresh(request PlanRequest) (AgentPlan, error) {
	adapter.beforePlan()
	return adapter.prepareTestAdapter.PlanFresh(request)
}

func (adapter prepareTestAdapter) Name() string       { return adapter.provider }
func (prepareTestAdapter) Mode() AdapterMode          { return AdapterModeMint }
func (prepareTestAdapter) CommittedEnvKeys() []string { return nil }
func (adapter prepareTestAdapter) Capabilities(context.Context) AdapterCapabilities {
	return AdapterCapabilities{Provider: adapter.provider, Mode: adapter.Mode(), Available: true, ProviderVersion: "test", Fresh: true, Resume: true}
}
func (adapter prepareTestAdapter) PlanFresh(request PlanRequest) (AgentPlan, error) {
	plan := AgentPlan{
		Handle: request.Handle, Argv: []string{"claude", "--session-id", request.LaunchNonce}, Cwd: request.Cwd,
		AdapterMode: adapter.Mode(), ResumePolicy: request.ResumePolicy, LaunchNonce: request.LaunchNonce,
		ConversationID: request.LaunchNonce, DynamicArgv: []DynamicArg{{Index: 2, Kind: DynamicArgLaunchNonce}},
	}
	return plan, plan.Validate()
}
func (adapter prepareTestAdapter) PlanResume(request ResumeRequest) (AgentPlan, error) {
	plan := AgentPlan{
		Handle: request.Handle, Argv: []string{"claude", "--resume", request.Conversation.ID}, Cwd: request.Cwd,
		AdapterMode: adapter.Mode(), ResumePolicy: request.ResumePolicy, LaunchNonce: request.LaunchNonce,
		ConversationID: request.Conversation.ID, DynamicArgv: []DynamicArg{{Index: 2, Kind: DynamicArgConversationID}},
	}
	return plan, plan.Validate()
}
func (prepareTestAdapter) CaptureIdentity(CaptureRequest) CaptureResult {
	return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonAdapterMintsIdentity}
}

type prepareTestBackend struct {
	detects  int
	inspects int
	creates  int
	closes   int
	focuses  int
}

type missingIdentityPrepareBackend struct{ prepareTestBackend }

func (*missingIdentityPrepareBackend) Detect() DetectResult {
	profile := Profile{Backend: "test", Platform: "test", VersionRange: "*", Version: 1, Capabilities: []Capability{CapCreate, CapInspect, CapClose, CapFocus}}
	return DetectResult{Available: true, Profile: profile, HostIdentity: "host:test", Effective: slices.Clone(profile.Capabilities)}
}

func (backend *prepareTestBackend) Detect() DetectResult {
	backend.detects++
	profile := Profile{Backend: "test", Platform: "test", VersionRange: "*", Version: 1, Capabilities: []Capability{CapCreate, CapInspect, CapClose, CapFocus}}
	return DetectResult{Available: true, Profile: profile, HostIdentity: "host:test", InstanceIdentity: "instance:test", Effective: slices.Clone(profile.Capabilities)}
}
func (backend *prepareTestBackend) Create(CreateRequest) (CreateResult, error) {
	backend.creates++
	return CreateResult{}, fmt.Errorf("Prepare called Create")
}
func (backend *prepareTestBackend) Inspect(InspectRequest) (InspectResult, error) {
	backend.inspects++
	return InspectResult{Status: InspectPresent, Evidence: "test-present"}, nil
}
func (backend *prepareTestBackend) Close(CloseRequest) (CloseResult, error) {
	backend.closes++
	return CloseResult{}, fmt.Errorf("Prepare called Close")
}
func (backend *prepareTestBackend) Focus(FocusRequest) (FocusResult, error) {
	backend.focuses++
	return FocusResult{}, fmt.Errorf("Prepare called Focus")
}

func prepareBinding(host, instance, nonce string) BindingRecord {
	return BindingRecord{
		Version: BindingVersion, Backend: "test", HostIdentity: host, InstanceIdentity: instance,
		Profile: "test/test/v1", LaunchNonce: nonce,
		Resources: ResourceIdentitySet{Version: ResourceSetVersion, Resources: []ResourceIdentity{{OpaqueID: "resource:claude", Agent: "claude"}}},
	}
}

func readyPrepareConversation(identity string) ConversationRecord {
	launchNonce := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	return ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureReady,
		Identity: ConversationIdentity{Provider: ClaudeProvider, ID: identity}, ProviderVersion: "test",
		LaunchNonce: launchNonce,
		ExecutionEvidence: &ConversationExecutionEvidence{
			Backend: "test", Profile: "test/test/v1", Outcome: OutcomeCreated,
			LaunchNonce: launchNonce, ConversationID: identity,
		},
	}
}

func writePrepareBinding(t *testing.T, rootPath string, binding BindingRecord) {
	t.Helper()
	withPrepareLease(t, rootPath, []string{"claude"}, func(root *fsq.DeliveryRoot, lease *Lease) error {
		return WriteBinding(root, lease, binding)
	})
}

func writePrepareConversation(t *testing.T, rootPath string, record ConversationRecord) {
	t.Helper()
	withPrepareLease(t, rootPath, []string{record.Handle}, func(root *fsq.DeliveryRoot, lease *Lease) error {
		return WriteConversation(root, lease, record)
	})
}

func withPrepareLease(t *testing.T, rootPath string, handles []string, operation func(*fsq.DeliveryRoot, *Lease) error) {
	t.Helper()
	identity, err := fsq.SnapshotDeliveryRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(rootPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lease, err := AcquireLease(root, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Release(); err != nil {
			t.Error(err)
		}
	}()
	if err := lease.LockHandles(handles...); err != nil {
		t.Fatal(err)
	}
	if err := operation(root, lease); err != nil {
		t.Fatal(err)
	}
}

func prepareTreeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var snapshot bytes.Buffer
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&snapshot, "%s\x00%s\x00%04o\x00", filepath.ToSlash(relative), entry.Type(), info.Mode().Perm())
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot.Write(data)
		}
		snapshot.WriteByte(0)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(snapshot.Bytes())
	return hex.EncodeToString(sum[:])
}
