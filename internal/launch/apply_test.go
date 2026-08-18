package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
