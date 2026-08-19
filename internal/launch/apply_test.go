package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestApplyRejectsBaseRootAuthorityDriftBeforeCreation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, project, configuredRoot string, configBytes []byte)
	}{
		{
			name: "project config identity replacement",
			mutate: func(t *testing.T, project, _ string, configBytes []byte) {
				replacement := filepath.Join(project, ".amqrc.next")
				if err := os.WriteFile(replacement, configBytes, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, filepath.Join(project, ".amqrc")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "base parent identity replacement",
			mutate: func(t *testing.T, _ string, configuredRoot string, _ []byte) {
				old := configuredRoot + ".old"
				if err := os.Rename(configuredRoot, old); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(configuredRoot, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			project := filepath.Join(root, "project")
			configuredRoot := filepath.Join(root, "configured")
			for _, path := range []string{project, configuredRoot} {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			configBytes, err := json.Marshal(map[string]string{"root": configuredRoot})
			if err != nil {
				t.Fatal(err)
			}
			configBytes = append(configBytes, '\n')
			if err := os.WriteFile(filepath.Join(project, ".amqrc"), configBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			profileRoot := filepath.Join(configuredRoot, "profile-a")
			sessionRoot := filepath.Join(profileRoot, "collab")
			request := ApplyRequest{Prepare: PrepareRequest{
				Target:   PrepareTarget{ProjectRoot: project, BaseRoot: profileRoot, SessionRoot: sessionRoot, Session: "collab"},
				Launcher: LauncherCommands, IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'d'}, 64)),
				Participants: []PrepareParticipant{{Handle: "operator", Runnable: false}},
			}}
			dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
				Backends: map[string]Backend{LauncherCommands: &prepareTestBackend{}},
				AdapterFor: func(provider, executable string) HarnessAdapter {
					return prepareTestAdapter{provider: provider}
				},
				HostIdentity: "host:test",
			}}
			prepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
			if err != nil {
				t.Fatal(err)
			}
			request.SubjectDigest = prepared.SubjectDigest
			request.Decisions = []ApplyDecision{}
			beforeApplyBaseRootCreateForTest = func() { test.mutate(t, project, configuredRoot, configBytes) }
			t.Cleanup(func() { beforeApplyBaseRootCreateForTest = nil })

			result, err := Apply(context.Background(), request, dependencies)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "subject_changed" {
				t.Fatalf("Apply drift result = %#v", result)
			}
			if _, err := os.Lstat(profileRoot); !os.IsNotExist(err) {
				t.Fatalf("authority drift created profile root: %v", err)
			}
			if _, err := os.Lstat(sessionRoot); !os.IsNotExist(err) {
				t.Fatalf("authority drift created session root: %v", err)
			}
		})
	}
}

func TestApplyRestartsAfterBaseCreationBeforeSessionPublication(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "project")
	configuredRoot := filepath.Join(root, "configured")
	for _, path := range []string{project, configuredRoot} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configBytes, err := json.Marshal(map[string]string{"root": configuredRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".amqrc"), append(configBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	profileRoot := filepath.Join(configuredRoot, "profile-a")
	sessionRoot := filepath.Join(profileRoot, "collab")
	request := ApplyRequest{Prepare: PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: project, BaseRoot: profileRoot, SessionRoot: sessionRoot, Session: "collab"},
		Launcher: LauncherCommands, IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'e'}, 64)),
		Participants: []PrepareParticipant{{Handle: "operator", Runnable: false}},
	}}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends: map[string]Backend{LauncherCommands: &prepareTestBackend{}},
		AdapterFor: func(provider, executable string) HarnessAdapter {
			return prepareTestAdapter{provider: provider}
		},
		HostIdentity: "host:test",
	}}
	prepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request.SubjectDigest = prepared.SubjectDigest
	request.Decisions = []ApplyDecision{}
	crash := errors.New("crash after base creation")
	afterApplyBaseRootCreatedForTest = func() error { return crash }
	_, err = Apply(context.Background(), request, dependencies)
	if !errors.Is(err, crash) {
		t.Fatalf("Apply crash error = %v", err)
	}
	afterApplyBaseRootCreatedForTest = nil
	t.Cleanup(func() { afterApplyBaseRootCreatedForTest = nil })
	if info, err := os.Stat(profileRoot); err != nil || !info.IsDir() {
		t.Fatalf("base root not committed before crash: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(sessionRoot); !os.IsNotExist(err) {
		t.Fatalf("session published before injected crash: %v", err)
	}

	reprepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	if len(reprepared.PlannedWrites) != 0 {
		t.Fatalf("restart still planned existing base root: %#v", reprepared.PlannedWrites)
	}
	request.SubjectDigest = reprepared.SubjectDigest
	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeProvisionedNoRunnable {
		t.Fatalf("restart Apply result = %#v", result)
	}
	if info, err := os.Stat(sessionRoot); err != nil || !info.IsDir() {
		t.Fatalf("restart did not publish session: info=%v err=%v", info, err)
	}
}

type applyUnavailableAdapter struct{ prepareTestAdapter }

func (adapter applyUnavailableAdapter) Capabilities(context.Context) AdapterCapabilities {
	return AdapterCapabilities{
		Provider: adapter.provider, Mode: adapter.Mode(), Available: false,
		ProviderVersion: "test", Reason: "executable_not_found",
	}
}

func TestApplyDoesNotPromoteNoActionToApplied(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	base := filepath.Join(root, "mail")
	for _, path := range []string{project, base} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	request := ApplyRequest{Prepare: PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: project, SessionRoot: filepath.Join(base, "collab"), Session: "collab"},
		Launcher: "test", IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'f'}, 64)),
		Participants: []PrepareParticipant{{
			Handle: "claude", Provider: ClaudeProvider, Executable: "claude", Runnable: true,
			Cwd: project, ResumePolicy: ResumeDisabled,
		}},
	}}
	backend := &prepareTestBackend{}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends: map[string]Backend{"test": backend}, Preferences: []string{"test"},
		AdapterFor: func(provider, executable string) HarnessAdapter {
			return applyUnavailableAdapter{prepareTestAdapter{provider: provider}}
		},
		HostIdentity: "host:test",
	}}
	prepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request.SubjectDigest = prepared.SubjectDigest
	for _, action := range prepared.RequiredActions {
		request.Decisions = append(request.Decisions, ApplyDecision{ActionID: action.ActionID, Choice: "trust_exact_subject"})
	}

	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "launch_action_required" {
		t.Fatalf("Apply result = %#v, want action_required without public no_action promotion", result)
	}
	if backend.creates != 0 {
		t.Fatalf("unavailable adapter created backend resources: %d", backend.creates)
	}
}

type applyInspectUnknownBackend struct{ prepareTestBackend }

func (backend *applyInspectUnknownBackend) Inspect(InspectRequest) (InspectResult, error) {
	backend.inspects++
	return InspectResult{Status: InspectUnknown, Evidence: "test inspection unavailable", ActionRequired: true}, nil
}

func TestApplyRefusesActionRequiredWithoutDecisionBeforeRosterMutation(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	fixture.request.Participants[0] = PrepareParticipant{Handle: "claude", Runnable: false}
	backend := &applyInspectUnknownBackend{}
	writePrepareBinding(t, fixture.sessionRoot, prepareBinding("host:test", "instance:test", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	dependencies := PrepareDependencies{
		Backends:     map[string]Backend{"test": backend},
		Preferences:  []string{"test"},
		AdapterFor:   func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		HostIdentity: "host:test",
	}
	prepared, err := Prepare(context.Background(), fixture.request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Outcome != PrepareOutcomeActionRequired || len(prepared.RequiredActions) != 0 {
		t.Fatalf("Prepare = %#v, want action_required without decisions", prepared)
	}
	before := prepareTreeSnapshot(t, fixture.sessionRoot)
	result, err := Apply(context.Background(), ApplyRequest{
		Prepare:       fixture.request,
		SubjectDigest: prepared.SubjectDigest,
	}, ApplyDependencies{PrepareDependencies: dependencies})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != ApplyReasonPrepareActionRequiredWithoutDecision {
		t.Fatalf("Apply = %#v, want typed action-required refusal", result)
	}
	if after := prepareTreeSnapshot(t, fixture.sessionRoot); after != before {
		t.Fatalf("Apply mutated roster/config on action_required without decision: before=%s after=%s", before, after)
	}
}

type applyUnsupportedNoCreateBackend struct{ reconcileBackend }

func (backend *applyUnsupportedNoCreateBackend) Detect() DetectResult {
	profile := Profile{Backend: backend.name, Platform: "test", VersionRange: "*", Version: 1, Capabilities: []Capability{CapInspect}}
	return DetectResult{Available: true, Profile: profile, Effective: slices.Clone(profile.Capabilities), HostIdentity: "host:test", InstanceIdentity: "instance:test"}
}

func (backend *applyUnsupportedNoCreateBackend) Create(CreateRequest) (CreateResult, error) {
	backend.creates++
	return CreateResult{Outcome: OutcomeUnsupported, Reason: "test backend unsupported"}, nil
}

func applyF31Result(t *testing.T, backend Backend) ApplyResult {
	t.Helper()
	providerBin := t.TempDir()
	providerPath := filepath.Join(providerBin, "claude")
	if err := os.WriteFile(providerPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", providerBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fixture := newInternalPrepareFixture(t)
	store, err := OpenTrustStore(t.TempDir(), fixture.projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends: map[string]Backend{"test": backend}, Preferences: []string{"test"},
		AdapterFor:   func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		HostIdentity: "host:test", TrustStore: store,
	}}
	prepared, err := Prepare(context.Background(), fixture.request, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{Prepare: fixture.request, SubjectDigest: prepared.SubjectDigest}
	for _, action := range prepared.RequiredActions {
		request.Decisions = append(request.Decisions, ApplyDecision{ActionID: action.ActionID, Choice: "trust_exact_subject"})
	}
	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestApplyMapsUnsupportedCreateToActionRequiredAndSupportedCreateToApplied(t *testing.T) {
	unsupported := &applyUnsupportedNoCreateBackend{reconcileBackend: reconcileBackend{name: "test", inspect: InspectAbsent}}
	unsupportedResult := applyF31Result(t, unsupported)
	if unsupportedResult.Outcome != ApplyOutcomeActionRequired || unsupportedResult.ReasonCode != "test backend unsupported" {
		t.Fatalf("unsupported Apply result = %#v, want action_required", unsupportedResult)
	}
	if unsupportedResult.Outcome == ApplyOutcomeApplied {
		t.Fatal("unsupported Create was promoted to Applied")
	}

	supported := &reconcileBackend{name: "test", inspect: InspectAbsent}
	supportedResult := applyF31Result(t, supported)
	if supportedResult.Outcome != ApplyOutcomeApplied {
		t.Fatalf("supported Apply result = %#v, want applied", supportedResult)
	}
}

func TestApplyV1RunnableParticipantsRequireReprepare(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	backend := &prepareTestBackend{}
	request := fixture.request
	request.SubjectSchema = SubjectSchemaV1
	// Keep the requested runnable handle absent from the pre-existing roster.
	// If the reprepare guard moves after roster provisioning, Apply must expose
	// that mutant by creating reviewer and adding it to config.json.
	request.Participants[0].Handle = "reviewer"
	configPath := filepath.Join(fixture.sessionRoot, "meta", "config.json")
	if err := os.WriteFile(configPath, []byte(`{"version":1,"agents":["claude"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := ApplyDependencies{PrepareDependencies: fixture.dependencies(backend)}
	prepared, err := Prepare(context.Background(), request, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeRoster := prepareTreeSnapshot(t, filepath.Join(fixture.sessionRoot, "agents"))

	applyRequest := ApplyRequest{Prepare: request, SubjectDigest: prepared.SubjectDigest}
	for _, action := range prepared.RequiredActions {
		applyRequest.Decisions = append(applyRequest.Decisions, ApplyDecision{ActionID: action.ActionID, Choice: "trust_exact_subject"})
	}
	result, err := Apply(context.Background(), applyRequest, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "reprepare_required" {
		t.Fatalf("v1 runnable Apply result = %#v, want typed reprepare refusal", result)
	}
	if result.SubjectDigest != prepared.SubjectDigest || result.PlanDigest != prepared.PlanDigest || result.TrustDigest != prepared.TrustDigest {
		t.Fatalf("reprepare result lost prepared digests: result=%#v prepared=%#v", result, prepared)
	}
	if backend.creates != 0 {
		t.Fatalf("v1 reprepare refusal created backend resources: %d", backend.creates)
	}
	afterConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterConfig, beforeConfig) {
		t.Fatalf("v1 reprepare refusal changed session config: before=%q after=%q", beforeConfig, afterConfig)
	}
	if afterRoster := prepareTreeSnapshot(t, filepath.Join(fixture.sessionRoot, "agents")); afterRoster != beforeRoster {
		t.Fatalf("v1 reprepare refusal changed durable roster: before=%s after=%s", beforeRoster, afterRoster)
	}
}

func TestApplyPublishedSessionCannotLoseLeaseTransitionRace(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	base := filepath.Join(root, "mail")
	for _, path := range []string{project, base} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	request := ApplyRequest{Prepare: PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: project, SessionRoot: filepath.Join(base, "collab"), Session: "collab"},
		Launcher: LauncherCommands, IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
		Participants: []PrepareParticipant{{Handle: "operator", Runnable: false}},
	}}
	backend := &prepareTestBackend{}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends:     map[string]Backend{LauncherCommands: backend},
		AdapterFor:   func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		HostIdentity: "host:test",
	}}
	prepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request.SubjectDigest = prepared.SubjectDigest
	request.Decisions = []ApplyDecision{}

	published := make(chan struct{})
	releasePublisher := make(chan struct{})
	afterApplySessionPublishedForTest = func() {
		close(published)
		<-releasePublisher
	}
	t.Cleanup(func() { afterApplySessionPublishedForTest = nil })

	type applyResponse struct {
		result ApplyResult
		err    error
	}
	first := make(chan applyResponse, 1)
	go func() {
		result, applyErr := Apply(context.Background(), request, dependencies)
		first <- applyResponse{result: result, err: applyErr}
	}()
	<-published

	second := make(chan applyResponse, 1)
	go func() {
		result, applyErr := Apply(context.Background(), request, dependencies)
		second <- applyResponse{result: result, err: applyErr}
	}()
	select {
	case response := <-second:
		t.Fatalf("existing-path Apply crossed publication before publisher acquired its lease: %#v", response)
	case <-time.After(100 * time.Millisecond):
	}
	close(releasePublisher)

	firstResult := <-first
	secondResult := <-second
	if firstResult.err != nil || firstResult.result.Outcome != ApplyOutcomeProvisionedNoRunnable {
		t.Fatalf("publisher result=%#v err=%v", firstResult.result, firstResult.err)
	}
	if secondResult.err != nil || secondResult.result.ReasonCode != "subject_changed" {
		t.Fatalf("racing result=%#v err=%v", secondResult.result, secondResult.err)
	}
	if _, err := os.Stat(filepath.Join(base, "collab", "meta", "launch", leaseFilename)); !os.IsNotExist(err) {
		t.Fatalf("Apply transition retained lease record: %v", err)
	}
}

func TestApplyNeverReplacesUncooperativeRacingSession(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	base := filepath.Join(root, "mail")
	for _, path := range []string{project, base} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	request := ApplyRequest{Prepare: PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: project, SessionRoot: filepath.Join(base, "collab"), Session: "collab"},
		Launcher: LauncherCommands, IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'b'}, 64)),
		Participants: []PrepareParticipant{{Handle: "operator", Runnable: false}},
	}}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends:     map[string]Backend{LauncherCommands: &prepareTestBackend{}},
		AdapterFor:   func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		HostIdentity: "host:test",
	}}
	prepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request.SubjectDigest = prepared.SubjectDigest
	beforeApplySessionPublishForTest = func() {
		if err := os.Mkdir(request.Prepare.Target.SessionRoot, 0o711); err != nil {
			t.Fatalf("create racing session: %v", err)
		}
	}
	t.Cleanup(func() { beforeApplySessionPublishForTest = nil })

	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "subject_changed" {
		t.Fatalf("racing Apply result = %#v", result)
	}
	info, err := os.Stat(request.Prepare.Target.SessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o711 {
		t.Fatalf("racing session mode = %04o, want untouched 0711", info.Mode().Perm())
	}
	entries, err := os.ReadDir(request.Prepare.Target.SessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("racing session was merged or replaced: %v", entries)
	}
}

func TestApplyRosterMutationStopsWhenLeaseAuthorityIsRevoked(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	base := filepath.Join(root, "mail")
	session := filepath.Join(base, "collab")
	for _, path := range []string{project, base} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsq.EnsureRootDirs(session); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(session, "operator"); err != nil {
		t.Fatal(err)
	}
	config := []byte("{\"version\":1,\"agents\":[\"operator\"]}\n")
	if err := os.WriteFile(filepath.Join(session, "meta", "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{Prepare: PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: project, SessionRoot: session, Session: "collab"},
		Launcher: LauncherCommands, IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'c'}, 64)),
		Participants: []PrepareParticipant{
			{Handle: "operator", Runnable: false},
			{Handle: "reviewer", Runnable: false},
		},
	}}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends:     map[string]Backend{LauncherCommands: &prepareTestBackend{}},
		AdapterFor:   func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		HostIdentity: "host:test",
	}}
	prepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request.SubjectDigest = prepared.SubjectDigest
	beforeApplyRosterMutationForTest = func() {
		if err := os.Remove(filepath.Join(session, bindingDirectory, leaseFilename)); err != nil {
			t.Fatalf("revoke Apply lease: %v", err)
		}
	}
	t.Cleanup(func() { beforeApplyRosterMutationForTest = nil })

	if _, err := Apply(context.Background(), request, dependencies); err == nil {
		t.Fatal("Apply succeeded after lease authority was revoked")
	}
	if _, err := os.Stat(filepath.Join(session, "agents", "reviewer")); !os.IsNotExist(err) {
		t.Fatalf("Apply mutated roster after lease revocation: %v", err)
	}
	gotConfig, err := os.ReadFile(filepath.Join(session, "meta", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotConfig, config) {
		t.Fatalf("Apply changed config after lease revocation: %q", gotConfig)
	}
}

func TestApplyReturnsTypedOutcomeForCommittedPublicationDurabilityFailure(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	base := filepath.Join(root, "mail")
	for _, path := range []string{project, base} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	request := ApplyRequest{Prepare: PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: project, SessionRoot: filepath.Join(base, "collab"), Session: "collab"},
		Launcher: LauncherCommands, IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'d'}, 64)),
		Participants: []PrepareParticipant{{Handle: "operator", Runnable: false}},
	}}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends:     map[string]Backend{LauncherCommands: &prepareTestBackend{}},
		AdapterFor:   func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		HostIdentity: "host:test",
	}}
	prepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request.SubjectDigest = prepared.SubjectDigest
	originalPublish := publishApplySession
	publishApplySession = func(base *fsq.DeliveryRoot, session string, initialize func(*fsq.DeliveryRoot) error) (*fsq.DeliveryRoot, error) {
		published, publishErr := originalPublish(base, session, initialize)
		if publishErr != nil {
			return published, publishErr
		}
		return published, &fsq.CommittedDurabilityError{FinalPath: published.Base(), Err: errors.New("parent sync failed")}
	}
	t.Cleanup(func() { publishApplySession = originalPublish })

	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "session_publication_durability_uncertain" || result.SubjectDigest != request.SubjectDigest {
		t.Fatalf("committed durability Apply result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(request.Prepare.Target.SessionRoot, "agents", "operator")); err != nil {
		t.Fatalf("committed session was not published fully: %v", err)
	}
	if _, err := os.Stat(filepath.Join(request.Prepare.Target.SessionRoot, bindingDirectory, leaseFilename)); !os.IsNotExist(err) {
		t.Fatalf("committed durability outcome retained lease: %v", err)
	}
}

func TestApplyReturnsTypedOutcomeForCommittedRosterConfigDurabilityFailure(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	base := filepath.Join(root, "mail")
	session := filepath.Join(base, "collab")
	for _, path := range []string{project, base} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsq.EnsureRootDirs(session); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(session, "operator"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "meta", "config.json"), []byte("{\"version\":1,\"agents\":[\"operator\"]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{Prepare: PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: project, SessionRoot: session, Session: "collab"},
		Launcher: LauncherCommands, IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'e'}, 64)),
		Participants: []PrepareParticipant{
			{Handle: "operator", Runnable: false},
			{Handle: "reviewer", Runnable: false},
		},
	}}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends:     map[string]Backend{LauncherCommands: &prepareTestBackend{}},
		AdapterFor:   func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		HostIdentity: "host:test",
	}}
	prepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request.SubjectDigest = prepared.SubjectDigest
	originalWrite := writeApplyConfigFile
	writeApplyConfigFile = func(root *fsq.DeliveryRoot, data []byte) error {
		if err := originalWrite(root, data); err != nil {
			return err
		}
		return &fsq.CommittedDurabilityError{FinalPath: filepath.Join(root.Base(), "meta", "config.json"), Err: errors.New("config parent sync failed")}
	}
	t.Cleanup(func() { writeApplyConfigFile = originalWrite })

	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "roster_config_durability_uncertain" || result.SubjectDigest != request.SubjectDigest {
		t.Fatalf("committed roster config Apply result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(session, "agents", "reviewer")); err != nil {
		t.Fatalf("committed roster was not applied: %v", err)
	}
}

func TestApplyFinalPrepareFailureReturnsCommittedBinding(t *testing.T) {
	request, dependencies, _ := committedRunnableApplyFixture(t)
	beforeApplyFinalPrepareForTest = func() {
		request.Prepare.Participants[0].Executable = "\x00"
	}
	t.Cleanup(func() { beforeApplyFinalPrepareForTest = nil })

	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "post_commit_prepare_failed" ||
		result.Disposition != MutationCommitted || result.BindingGeneration == "" || result.Backend != "test" {
		t.Fatalf("post-commit Prepare result = %#v", result)
	}
}

func TestApplyReconcileCrashAfterBindingReturnsCommittedGeneration(t *testing.T) {
	request, dependencies, _ := committedRunnableApplyFixture(t)
	crash := errors.New("crash after binding write")
	dependencies.CrashHook = func(stage string) error {
		if stage == "binding_written" {
			return crash
		}
		return nil
	}

	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.Disposition != MutationCommitted || result.BindingGeneration == "" || result.Backend != "test" {
		t.Fatalf("binding-written crash result = %#v", result)
	}
}

func TestApplyReconcileCrashAfterJournalWrittenWithoutBindingIsUncertain(t *testing.T) {
	request, dependencies, _ := committedRunnableApplyFixture(t)
	crash := errors.New("crash after journal write")
	dependencies.CrashHook = func(stage string) error {
		if stage == "journal_written" {
			return crash
		}
		return nil
	}

	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.Disposition != MutationUncertain || result.BindingGeneration == "" {
		t.Fatalf("journal-written crash result = %#v", result)
	}
}

func TestClassifyApplyMutationCorruptJournalWithoutBindingIsUncertain(t *testing.T) {
	request, dependencies, _ := committedRunnableApplyFixture(t)
	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeApplied || result.Disposition != MutationCommitted {
		t.Fatalf("successful Apply = %#v", result)
	}
	if err := os.Remove(BindingPath(request.Prepare.Target.SessionRoot)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(JournalPath(request.Prepare.Target.SessionRoot), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(request.Prepare.Target.SessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(request.Prepare.Target.SessionRoot, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	classified := classifyApplyMutation(ApplyResult{Outcome: ApplyOutcomeActionRequired}, root)
	if classified.Disposition != MutationUncertain {
		t.Fatalf("corrupt journal classification = %#v, want uncertain", classified)
	}
}

func TestApplyEvidenceFailureReturnsCommittedBinding(t *testing.T) {
	request, dependencies, _ := committedRunnableApplyFixture(t)
	collectApplyEvidenceRefs = func(*fsq.DeliveryRoot, []string) ([]EvidenceRef, error) {
		return nil, errors.New("evidence read timeout")
	}
	t.Cleanup(func() { collectApplyEvidenceRefs = CollectEvidenceRefs })

	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "post_commit_evidence_failed" ||
		result.Disposition != MutationCommitted || result.BindingGeneration == "" || result.Backend != "test" {
		t.Fatalf("post-commit evidence result = %#v", result)
	}
}

func TestApplyDefinitePreMutationFailureIsNotApplied(t *testing.T) {
	request, dependencies, backend := committedRunnableApplyFixture(t)
	backend.createErr = errors.New("create refused before mutation")
	backend.definiteErr = true

	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.Disposition != MutationNotApplied || result.BindingGeneration != "" {
		t.Fatalf("pre-mutation result = %#v", result)
	}
}

func committedRunnableApplyFixture(t *testing.T) (ApplyRequest, ApplyDependencies, *reconcileBackend) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	base := filepath.Join(root, "mail")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	_, executable := testExecutable(t, ClaudeProvider)
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	store, err := OpenTrustStore(filepath.Join(root, "state"), project)
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{Prepare: PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: project, SessionRoot: filepath.Join(base, "collab"), Session: "collab"},
		Launcher: "test", IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
		Participants: []PrepareParticipant{{Handle: "claude", Provider: ClaudeProvider, Executable: executable, Runnable: true, Cwd: project, ResumePolicy: ResumeDisabled}},
	}}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends: map[string]Backend{"test": backend}, Preferences: []string{"test"}, TrustStore: store,
		AdapterFor: func(provider, executable string) HarnessAdapter {
			return reconcileAdapter{name: provider, mode: AdapterModeMint, available: true}
		},
		HostIdentity: "host:test",
	}}
	prepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request.SubjectDigest = prepared.SubjectDigest
	for _, action := range prepared.RequiredActions {
		request.Decisions = append(request.Decisions, ApplyDecision{ActionID: action.ActionID, Choice: "trust_exact_subject"})
	}
	return request, dependencies, backend
}

func TestClassifyApplyMutationUnreadableBindingWithoutJournalIsUncertain(t *testing.T) {
	request, _, _ := committedRunnableApplyFixture(t)
	session := request.Prepare.Target.SessionRoot
	if err := fsq.EnsureRootDirs(session); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(BindingPath(session)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(BindingPath(session), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(JournalPath(session)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(session)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(session, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	classified := classifyApplyMutation(ApplyResult{Outcome: ApplyOutcomeActionRequired}, root)
	if classified.Disposition != MutationUncertain {
		t.Fatalf("unreadable binding classification = %#v, want uncertain", classified)
	}
}

func TestOnLiveOmittedMatchesV061Apply(t *testing.T) {
	// Apply-path guard: omit matches explicit refuse (v0.61 whole-binding
	// refusal). Digest stability is TestPrepareV2SubjectDigestIncludesOnLiveKeepOnly.
	omitted := applyMixedLiveMissing(t, "")
	refused := applyMixedLiveMissing(t, OnLiveRefuse)
	if omitted.Outcome != ApplyOutcomeActionRequired || omitted.ReasonCode != "binding_present_without_resumable_conversation" {
		t.Fatalf("omitted on_live Apply = %#v, want v0.61 whole-binding refusal", omitted)
	}
	if omitted.Outcome != refused.Outcome || omitted.ReasonCode != refused.ReasonCode {
		t.Fatalf("omitted Apply diverged from explicit refuse: omit=%#v refuse=%#v", omitted, refused)
	}
	for _, result := range []ApplyResult{omitted, refused} {
		for _, observation := range result.Observations {
			if observation.Disposition == SeatKept || observation.Disposition == SeatCreated {
				t.Fatalf("omitted/refuse stamped keep-or-create: %#v", result.Observations)
			}
		}
	}
	kept := applyMixedLiveMissing(t, OnLiveKeep)
	if kept.ReasonCode == omitted.ReasonCode {
		t.Fatalf("on_live keep used the v0.61 refusal path: %#v", kept)
	}
}

func applyMixedLiveMissing(t *testing.T, onLive string) ApplyResult {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	base := filepath.Join(root, "mail")
	session := filepath.Join(base, "collab")
	for _, path := range []string{project, base} {
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
	writePrepareBinding(t, session, prepareBinding("host:test", "instance:test", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	store, err := OpenTrustStore(t.TempDir(), project)
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{Prepare: PrepareRequest{
		Target:   PrepareTarget{ProjectRoot: project, SessionRoot: session, Session: "collab"},
		Launcher: "test", IntentDigest: "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
		Participants: []PrepareParticipant{
			{
				Handle: "claude", Provider: ClaudeProvider, Executable: "claude", Runnable: true,
				Cwd: project, ResumePolicy: ResumeEnabled, OnLive: onLive,
			},
			{
				Handle: "codex", Provider: CodexProvider, Executable: "codex", Runnable: true,
				Cwd: project, ResumePolicy: ResumeFresh, OnLive: onLive,
			},
		},
	}}
	backend := &prepareTestBackend{}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends: map[string]Backend{"test": backend}, Preferences: []string{"test"},
		AdapterFor: func(provider, executable string) HarnessAdapter { return prepareTestAdapter{provider: provider} },
		TrustStore: store, HostIdentity: "host:test",
	}}
	prepared, err := Prepare(context.Background(), request.Prepare, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request.SubjectDigest = prepared.SubjectDigest
	for _, action := range prepared.RequiredActions {
		request.Decisions = append(request.Decisions, ApplyDecision{ActionID: action.ActionID, Choice: "trust_exact_subject"})
	}
	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if backend.creates != 0 {
		t.Fatalf("on_live %q created backend resources: %d result=%#v", onLive, backend.creates, result)
	}
	return result
}
