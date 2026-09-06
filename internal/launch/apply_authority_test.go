//go:build unix

package launch

import (
	"bytes"
	"context"
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
