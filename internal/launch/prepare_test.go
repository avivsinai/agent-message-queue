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
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

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

// TestPrepareSurfacesCodexCaptureVersionRefusal protects issue #1107: Prepare
// probes a capture adapter before planning a fresh seat and reports the
// capture_version_unsupported refusal on that participant alone. The refused
// seat contributes no plan agent and does not make the whole launch
// unsupported, so the other seats still proceed and Reconcile refuses only it.
func TestPrepareSurfacesCodexCaptureVersionRefusal(t *testing.T) {
	for _, tc := range []struct {
		version, planned, reason string
	}{
		{"0.146.0", "unsupported", "capture_version_unsupported"},
		{"0.154.0", "fresh", ""},
	} {
		t.Run(tc.version, func(t *testing.T) {
			fixture := newInternalPrepareFixture(t)
			if err := fsq.EnsureAgentDirs(fixture.sessionRoot, "codex"); err != nil {
				t.Fatal(err)
			}
			_, executable := testExecutable(t, CodexProvider)
			fixture.request.Participants = append(fixture.request.Participants, PrepareParticipant{
				Handle: "codex", Runnable: true, Provider: CodexProvider, Executable: executable,
				Cwd: fixture.cwd, ResumePolicy: ResumeEnabled,
				Execution: PrepareExecutionOptions{WakeMode: "enabled"},
			})
			dependencies := fixture.dependencies(&prepareTestBackend{})
			dependencies.AdapterFor = func(provider, executable string) HarnessAdapter {
				if provider != CodexProvider {
					return prepareTestAdapter{provider: provider}
				}
				adapter := NewCodexAdapter(executable)
				adapter.probe = fakeCommandProbe{path: executable, outputs: map[string]string{
					"--version": "codex-cli " + tc.version + "\n", "--help": "commands: resume",
					"resume --help": "Usage: codex resume [OPTIONS] [SESSION_ID]",
				}}
				return adapter
			}
			result, err := Prepare(context.Background(), fixture.request, dependencies)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome == PrepareOutcomeUnsupported {
				t.Fatalf("Prepare at %s made the whole launch unsupported: %q", tc.version, result.Reason)
			}
			claude, codex := result.Participants[0], result.Participants[1]
			if claude.PlannedOutcome != "fresh" || claude.Command == nil {
				t.Fatalf("claude seat at %s = %#v", tc.version, claude)
			}
			if codex.PlannedOutcome != tc.planned || result.Observations[1].ReasonCode != tc.reason || (codex.Command == nil) != (tc.reason != "") {
				t.Fatalf("codex seat at %s = planned %q reason %q command %#v", tc.version, codex.PlannedOutcome, result.Observations[1].ReasonCode, codex.Command)
			}
			wantPlanned := []string{"claude", "codex"}
			if tc.reason != "" {
				wantPlanned = []string{"claude"}
			}
			var trust *PrepareRequiredAction
			for i := range result.RequiredActions {
				if result.RequiredActions[i].Kind == ActionTrustConfirmation {
					trust = &result.RequiredActions[i]
				}
			}
			if trust == nil || !slices.Equal(trust.Handles, wantPlanned) {
				t.Fatalf("planned agents at %s = %#v, want %v", tc.version, trust, wantPlanned)
			}
		})
	}
}

type countingCommandProbe struct {
	fakeCommandProbe
	runs *int
}

func (probe countingCommandProbe) Output(ctx context.Context, executable string, args ...string) ([]byte, error) {
	*probe.runs++
	return probe.fakeCommandProbe.Output(ctx, executable, args...)
}

// TestPrepareChecksCaptureContainmentBeforeProbe protects the PR #1108 review
// findings: Prepare refuses a project-contained capture provider before its
// capability probe executes it, and refuses only that seat, as Reconcile does.
func TestPrepareChecksCaptureContainmentBeforeProbe(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	if err := fsq.EnsureAgentDirs(fixture.sessionRoot, "codex"); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(fixture.projectRoot, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, CodexProvider)
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.request.Participants = append(fixture.request.Participants, PrepareParticipant{
		Handle: "codex", Runnable: true, Provider: CodexProvider, Executable: executable,
		Cwd: fixture.cwd, ResumePolicy: ResumeEnabled,
		Execution: PrepareExecutionOptions{WakeMode: "enabled"},
	})
	runs := 0
	dependencies := fixture.dependencies(&prepareTestBackend{})
	dependencies.AdapterFor = func(provider, executable string) HarnessAdapter {
		if provider != CodexProvider {
			return prepareTestAdapter{provider: provider}
		}
		adapter := NewCodexAdapter(executable)
		adapter.probe = countingCommandProbe{runs: &runs, fakeCommandProbe: fakeCommandProbe{path: executable, outputs: map[string]string{
			"--version": "codex-cli 0.163.0\n", "--help": "commands: resume",
			"resume --help": "Usage: codex resume [OPTIONS] [SESSION_ID]",
		}}}
		return adapter
	}
	result, err := Prepare(context.Background(), fixture.request, dependencies)
	if err != nil {
		t.Fatalf("Prepare failed the whole launch: %v", err)
	}
	if runs != 0 {
		t.Fatalf("Prepare executed the contained provider %d times", runs)
	}
	claude, codex := result.Participants[0], result.Participants[1]
	if claude.PlannedOutcome != "fresh" || codex.PlannedOutcome != "unsupported" || codex.Command != nil ||
		result.Observations[1].ReasonCode != ProviderProjectContainedCode {
		t.Fatalf("seats = claude %q, codex %q reason %q", claude.PlannedOutcome, codex.PlannedOutcome, result.Observations[1].ReasonCode)
	}
}

// TestPrepareAdmitsCodexWorkspaceWriteSandboxPins protects issue #1212: a
// Codex seat under --sandbox workspace-write takes its writable roots and
// network access from config.toml unless -c pins them, and those pins must be
// provider arguments inside the trust digest, not argv an embedder appends
// after admission.
func TestPrepareAdmitsCodexWorkspaceWriteSandboxPins(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	if err := fsq.EnsureAgentDirs(fixture.sessionRoot, "codex"); err != nil {
		t.Fatal(err)
	}
	_, executable := testExecutable(t, CodexProvider)
	fixture.request.Participants = append(fixture.request.Participants, PrepareParticipant{
		Handle: "codex", Runnable: true, Provider: CodexProvider, Executable: executable,
		Args: []string{"--sandbox", "workspace-write"}, Cwd: fixture.cwd, ResumePolicy: ResumeEnabled,
		Execution: PrepareExecutionOptions{WakeMode: "enabled"},
	})
	dependencies := fixture.dependencies(&prepareTestBackend{})
	dependencies.AdapterFor = func(provider, executable string) HarnessAdapter {
		if provider != CodexProvider {
			return prepareTestAdapter{provider: provider}
		}
		adapter := NewCodexAdapter(executable)
		adapter.probe = fakeCommandProbe{path: executable, outputs: map[string]string{
			"--version": "codex-cli 0.154.0\n", "--help": "commands: resume",
			"resume --help": "Usage: codex resume [OPTIONS] [SESSION_ID]",
		}}
		return adapter
	}
	unpinned, err := Prepare(context.Background(), fixture.request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	pins := []string{"sandbox_workspace_write.writable_roots=[]", "sandbox_workspace_write.network_access=false"}
	fixture.request.Participants[1].Args = append(fixture.request.Participants[1].Args, "-c", pins[0], "-c", pins[1])
	pinned, err := Prepare(context.Background(), fixture.request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	codex := pinned.Participants[1]
	if codex.PlannedOutcome != "fresh" || codex.Command == nil || !slices.Contains(codex.Command.Argv, pins[0]) || !slices.Contains(codex.Command.Argv, pins[1]) {
		t.Fatalf("pinned codex seat = planned %q command %#v", codex.PlannedOutcome, codex.Command)
	}
	if pinned.TrustDigest == unpinned.TrustDigest {
		t.Fatalf("sandbox pins did not change the trust digest %s", pinned.TrustDigest)
	}
	advertised := map[string][]string{}
	for _, override := range ProviderStaticInputCapabilities(CodexProvider).ConfigOverrides {
		advertised[override.Key] = override.AllowedValues
	}
	if !slices.Equal(advertised["sandbox_workspace_write.writable_roots"], []string{"[]"}) ||
		!slices.Equal(advertised["sandbox_workspace_write.network_access"], []string{"true", "false"}) {
		t.Fatalf("codex config overrides = %v", advertised)
	}
}
