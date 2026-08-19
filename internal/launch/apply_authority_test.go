//go:build unix

package launch

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestApplyRefusesTargetSwapBeforeAnyReplacementTreeWrite(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	fixture.request.Participants = append(fixture.request.Participants, PrepareParticipant{Handle: "reviewer", Runnable: false})
	backend := &prepareTestBackend{}
	dependencies := ApplyDependencies{PrepareDependencies: fixture.dependencies(backend)}
	prepared, err := Prepare(context.Background(), fixture.request, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{Prepare: fixture.request, SubjectDigest: prepared.SubjectDigest}
	var openCount atomic.Int32
	previousOpenPrepareTarget := openPrepareTargetForApply
	openPrepareTargetForApply = func(target PrepareTarget) (*prepareTargetState, error) {
		openCount.Add(1)
		return openPrepareTarget(target)
	}

	parked := fixture.sessionRoot + ".authorized"
	replacement := fixture.sessionRoot + ".replacement"
	marker := filepath.Join(fixture.sessionRoot, "meta", "marker")
	replacementMarker := filepath.Join(replacement, "meta", "marker")
	var callbackCalls atomic.Int32
	beforeApplyAuthorizeApplyForTest = func() {
		callbackCalls.Add(1)
		if err := os.Rename(fixture.sessionRoot, parked); err != nil {
			t.Fatalf("park authorized session: %v", err)
		}
		if err := fsq.EnsureRootDirs(replacement); err != nil {
			t.Fatalf("create replacement session: %v", err)
		}
		if err := os.WriteFile(replacementMarker, []byte("replacement\n"), 0o600); err != nil {
			t.Fatalf("mark replacement session: %v", err)
		}
		if err := os.Rename(replacement, fixture.sessionRoot); err != nil {
			t.Fatalf("publish replacement session: %v", err)
		}
	}
	t.Cleanup(func() {
		beforeApplyAuthorizeApplyForTest = nil
		openPrepareTargetForApply = previousOpenPrepareTarget
		_ = os.RemoveAll(fixture.sessionRoot)
		_ = os.Rename(parked, fixture.sessionRoot)
		_ = os.RemoveAll(replacement)
	})

	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if callbackCalls.Load() != 1 {
		t.Fatalf("authorization hook calls = %d", callbackCalls.Load())
	}
	if openCount.Load() != 1 {
		t.Fatalf("Apply opened prepare target %d times, want exactly once", openCount.Load())
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "subject_changed" {
		t.Fatalf("target swap result = %#v", result)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("replacement\n")) {
		t.Fatalf("replacement tree changed: %q", got)
	}
	if backend.creates != 0 {
		t.Fatalf("target swap created backend resources: %d", backend.creates)
	}
}

type countingApplyAdapter struct {
	reconcileAdapter
	capabilityCalls *atomic.Int32
}

func (adapter countingApplyAdapter) Capabilities(ctx context.Context) AdapterCapabilities {
	adapter.capabilityCalls.Add(1)
	return adapter.reconcileAdapter.Capabilities(ctx)
}

func TestApplyRejectsAuthorizedIdentityReplacementBeforeCapabilitiesOrCreate(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, fixture *internalPrepareFixture) func()
	}{
		{
			name: "provider executable",
			setup: func(t *testing.T, fixture *internalPrepareFixture) func() {
				path := filepath.Join(fixture.root, "provider")
				writeExec(t, path, "#!/bin/sh\necho one\n")
				fixture.request.Participants[0].Executable = path
				return replaceExecutableAfterAuthorization(t, path, "#!/bin/sh\necho replacement\n")
			},
		},
		{
			name: "wrapper executable",
			setup: func(t *testing.T, fixture *internalPrepareFixture) func() {
				path := filepath.Join(fixture.root, "wrapper")
				writeExec(t, path, "#!/bin/sh\necho one\n")
				fixture.request.Participants[0].Wrapper = &Wrapper{Executable: path, Args: []string{"--wrapped"}}
				return replaceExecutableAfterAuthorization(t, path, "#!/bin/sh\necho replacement\n")
			},
		},
		{
			name: "working directory",
			setup: func(t *testing.T, fixture *internalPrepareFixture) func() {
				path := fixture.cwd
				parked := path + ".authorized"
				return func() {
					if err := os.Rename(path, parked); err != nil {
						t.Fatalf("park authorized cwd: %v", err)
					}
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatalf("create replacement cwd: %v", err)
					}
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInternalPrepareFixture(t)
			mutate := test.setup(t, &fixture)
			backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
			var capabilityCalls atomic.Int32
			adapter := countingApplyAdapter{
				reconcileAdapter: reconcileAdapter{name: ClaudeProvider, mode: AdapterModeMint, available: true},
				capabilityCalls:  &capabilityCalls,
			}
			store, err := OpenTrustStore(t.TempDir(), fixture.projectRoot)
			if err != nil {
				t.Fatal(err)
			}
			dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
				Backends: map[string]Backend{"test": backend}, Preferences: []string{"test"}, TrustStore: store, HostIdentity: "host:test",
				AdapterFor: func(provider, executable string) HarnessAdapter { return adapter },
			}}
			prepared, err := Prepare(context.Background(), fixture.request, dependencies.PrepareDependencies)
			if err != nil {
				t.Fatal(err)
			}
			request := ApplyRequest{Prepare: fixture.request, SubjectDigest: prepared.SubjectDigest}
			for _, action := range prepared.RequiredActions {
				request.Decisions = append(request.Decisions, ApplyDecision{ActionID: action.ActionID, Choice: "trust_exact_subject"})
			}
			beforeApplyReconcileForTest = mutate
			t.Cleanup(func() { beforeApplyReconcileForTest = nil })

			result, err := Apply(context.Background(), request, dependencies)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "authorized_identity_changed" {
				t.Fatalf("replacement result = %#v", result)
			}
			if capabilityCalls.Load() != 0 {
				t.Fatalf("replacement reached adapter capabilities: %d", capabilityCalls.Load())
			}
			if backend.creates != 0 {
				t.Fatalf("replacement created backend resources: %d", backend.creates)
			}
		})
	}
}

func replaceExecutableAfterAuthorization(t *testing.T, path, body string) func() {
	t.Helper()
	return func() {
		replacement := path + ".next"
		writeExec(t, replacement, body)
		if err := os.Rename(replacement, path); err != nil {
			t.Fatalf("replace authorized executable: %v", err)
		}
	}
}

func TestApplyKeepsUnchangedAuthorizedIdentities(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	provider := filepath.Join(fixture.root, "provider")
	writeExec(t, provider, "#!/bin/sh\necho one\n")
	fixture.request.Participants[0].Executable = provider
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	var capabilityCalls atomic.Int32
	adapter := countingApplyAdapter{
		reconcileAdapter: reconcileAdapter{name: ClaudeProvider, mode: AdapterModeMint, available: true},
		capabilityCalls:  &capabilityCalls,
	}
	store, err := OpenTrustStore(t.TempDir(), fixture.projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends: map[string]Backend{"test": backend}, Preferences: []string{"test"}, TrustStore: store, HostIdentity: "host:test",
		AdapterFor: func(provider, executable string) HarnessAdapter { return adapter },
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
	if result.Outcome != ApplyOutcomeApplied {
		t.Fatalf("unchanged identities result = %#v", result)
	}
	if capabilityCalls.Load() == 0 || backend.creates != 1 {
		t.Fatalf("unchanged identities did not execute reconciliation: capabilities=%d creates=%d", capabilityCalls.Load(), backend.creates)
	}
}

func TestReconcileJournalRecoveryRefusesProviderReplacementBeforeCapabilities(t *testing.T) {
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	request := reconcileFixture(t, backend)
	provider := filepath.Join(t.TempDir(), "claude")
	writeExec(t, provider, "#!/bin/sh\necho one\n")
	t.Setenv("PATH", filepath.Dir(provider)+string(os.PathListSeparator)+os.Getenv("PATH"))
	request.Config.Agents[0].Command = []string{"claude"}
	resolved, err := ResolveConsultedExecutable("claude")
	if err != nil {
		t.Fatal(err)
	}
	cwdIdentity, err := fsq.StableTreeIdentity(request.ProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	request.AuthorizedIdentities = map[string]AuthorizedParticipantIdentity{
		"claude": {Executable: &resolved, CwdIdentity: cwdIdentity},
	}
	crash := errors.New("crash after journal write")
	request.CrashHook = func(stage string) error {
		if stage == "journal_written" {
			return crash
		}
		return nil
	}
	initial, err := Reconcile(request)
	if !errors.Is(err, crash) {
		t.Fatalf("initial crash error = %v result=%#v", err, initial)
	}
	if _, err := LoadJournal(request.Root); err != nil {
		t.Fatalf("journal after injected crash: %v", err)
	}
	replacement := provider + ".next"
	writeExec(t, replacement, "#!/bin/sh\necho replacement\n")
	if err := os.Rename(replacement, provider); err != nil {
		t.Fatal(err)
	}
	var capabilityCalls atomic.Int32
	request.CrashHook = nil
	request.Adapters["claude"] = countingApplyAdapter{
		reconcileAdapter: reconcileAdapter{name: ClaudeProvider, mode: AdapterModeMint, available: true},
		capabilityCalls:  &capabilityCalls,
	}
	result, err := Reconcile(request)
	if err != nil {
		t.Fatal(err)
	}
	if result.AggregateCode != 6 || result.Outcome != OutcomeActionRequired || result.Reason != "authorized_identity_changed" {
		t.Fatalf("recovery replacement result = %#v", result)
	}
	if capabilityCalls.Load() != 0 || backend.creates != 0 {
		t.Fatalf("recovery replacement reached execution: capabilities=%d creates=%d", capabilityCalls.Load(), backend.creates)
	}
}

func TestApplyPostReconcileTargetSwapCannotReportApplied(t *testing.T) {
	fixture := newInternalPrepareFixture(t)
	provider := filepath.Join(fixture.root, "provider")
	writeExec(t, provider, "#!/bin/sh\necho one\n")
	fixture.request.Participants[0].Executable = provider
	backend := &reconcileBackend{name: "test", inspect: InspectAbsent}
	var capabilityCalls atomic.Int32
	adapter := countingApplyAdapter{
		reconcileAdapter: reconcileAdapter{name: ClaudeProvider, mode: AdapterModeMint, available: true},
		capabilityCalls:  &capabilityCalls,
	}
	store, err := OpenTrustStore(t.TempDir(), fixture.projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := ApplyDependencies{PrepareDependencies: PrepareDependencies{
		Backends: map[string]Backend{"test": backend}, Preferences: []string{"test"}, TrustStore: store, HostIdentity: "host:test",
		AdapterFor: func(provider, executable string) HarnessAdapter { return adapter },
	}}
	prepared, err := Prepare(context.Background(), fixture.request, dependencies.PrepareDependencies)
	if err != nil {
		t.Fatal(err)
	}
	request := ApplyRequest{Prepare: fixture.request, SubjectDigest: prepared.SubjectDigest}
	for _, action := range prepared.RequiredActions {
		request.Decisions = append(request.Decisions, ApplyDecision{ActionID: action.ActionID, Choice: "trust_exact_subject"})
	}
	parked := fixture.sessionRoot + ".authorized"
	replacement := fixture.sessionRoot + ".replacement"
	afterApplyReconcileForTest = func() {
		if err := os.Rename(fixture.sessionRoot, parked); err != nil {
			t.Fatalf("park post-reconcile session: %v", err)
		}
		if err := fsq.EnsureRootDirs(replacement); err != nil {
			t.Fatalf("create post-reconcile replacement: %v", err)
		}
		if err := os.Rename(replacement, fixture.sessionRoot); err != nil {
			t.Fatalf("publish post-reconcile replacement: %v", err)
		}
	}
	t.Cleanup(func() {
		afterApplyReconcileForTest = nil
		_ = os.RemoveAll(fixture.sessionRoot)
		_ = os.Rename(parked, fixture.sessionRoot)
		_ = os.RemoveAll(replacement)
	})
	result, err := Apply(context.Background(), request, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ApplyOutcomeActionRequired || result.ReasonCode != "subject_changed" {
		t.Fatalf("post-reconcile swap result = %#v", result)
	}
	if backend.creates != 1 || capabilityCalls.Load() == 0 {
		t.Fatalf("post-reconcile swap did not execute the original plan: creates=%d capabilities=%d", backend.creates, capabilityCalls.Load())
	}
}
