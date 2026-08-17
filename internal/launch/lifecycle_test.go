package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestPublicLifecycleManagedObservations(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	project := t.TempDir()
	sessionPath := filepath.Join(project, ".agent-mail", "collab")
	if err := fsq.EnsureRootDirs(sessionPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionPath, "meta", "config.json"), []byte(`{"version":1,"agents":["claude"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(sessionPath, "claude"); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(sessionPath, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	tmux := NewTmuxBackend("tmux")
	tmux.socketName = fmt.Sprintf("amq-lifecycle-%d-%d", os.Getpid(), time.Now().UnixNano())
	tmux.focus = func(context.Context, string) error { return nil }
	t.Cleanup(func() { stopTmuxTestServer(t, tmux) })
	backend := &lifecycleCountingBackend{TmuxBackend: tmux}
	fakeAMQ := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(fakeAMQ, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	nonce := "72727272-7272-4272-8272-727272727272"
	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{{
		Handle: "claude", Argv: []string{"/bin/sleep", "60"}, Cwd: project,
		AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce,
		ConversationID: "73737373-7373-4373-8373-737373737373",
	}}}
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	writeLifecycleBinding(t, root, created.Binding)
	target := PrepareTarget{ProjectRoot: project, SessionRoot: sessionPath, Session: "collab"}
	deps := LifecycleDependencies{Backends: map[string]Backend{LauncherTMux: backend}}

	before := prepareTreeSnapshot(t, sessionPath)
	inspected, err := InspectLifecycle(context.Background(), LifecycleRequest{Target: target}, deps)
	if err != nil || inspected.Outcome != LifecycleOutcomeInspected || inspected.State != string(InspectPresent) || len(inspected.Observations) != 1 || inspected.Observations[0].Resource == "" {
		t.Fatalf("Inspect = %#v, %v", inspected, err)
	}
	if after := prepareTreeSnapshot(t, sessionPath); after != before {
		t.Fatal("Inspect changed the session tree")
	}

	backend.unknown = true
	mutations := backend.mutations
	before = prepareTreeSnapshot(t, sessionPath)
	unknown, err := FocusLifecycle(context.Background(), LifecycleRequest{Target: target}, deps)
	if err != nil || unknown.ReasonCode != "inspect_unknown" || backend.mutations != mutations {
		t.Fatalf("unknown Focus = %#v, %v mutations=%d", unknown, err, backend.mutations)
	}
	if after := prepareTreeSnapshot(t, sessionPath); after != before {
		t.Fatal("unknown Focus changed the session tree")
	}
	backend.unknown = false

	foreign := created.Binding
	foreign.HostIdentity = "foreign-host"
	writeLifecycleBinding(t, root, foreign)
	before = prepareTreeSnapshot(t, sessionPath)
	refused, err := CloseLifecycle(context.Background(), LifecycleRequest{Target: target}, deps)
	if err != nil || refused.ReasonCode != "foreign_binding" || backend.mutations != mutations {
		t.Fatalf("foreign Close = %#v, %v mutations=%d", refused, err, backend.mutations)
	}
	if after := prepareTreeSnapshot(t, sessionPath); after != before {
		t.Fatal("foreign Close changed the session tree")
	}
	writeLifecycleBinding(t, root, created.Binding)

	profileMismatch := created.Binding
	profileMismatch.Profile = "tmux/test/v999"
	writeLifecycleBinding(t, root, profileMismatch)
	before = prepareTreeSnapshot(t, sessionPath)
	refused, err = FocusLifecycle(context.Background(), LifecycleRequest{Target: target}, deps)
	if err != nil || refused.ReasonCode != "profile_mismatch" || backend.mutations != mutations {
		t.Fatalf("profile Focus = %#v, %v mutations=%d", refused, err, backend.mutations)
	}
	if after := prepareTreeSnapshot(t, sessionPath); after != before {
		t.Fatal("profile-mismatch Focus changed the session tree")
	}
	writeLifecycleBinding(t, root, created.Binding)

	changed := created.Binding
	changed.LaunchNonce = "74747474-7474-4474-8474-747474747474"
	t.Cleanup(func() { beforeLifecycleBackendMutationForTest = nil })
	beforeLifecycleBackendMutationForTest = func() { writeLifecycleBindingRaw(t, sessionPath, changed) }
	refused, err = FocusLifecycle(context.Background(), LifecycleRequest{Target: target}, deps)
	beforeLifecycleBackendMutationForTest = nil
	if err != nil || refused.ReasonCode != "binding_changed" || backend.mutations != mutations {
		t.Fatalf("changed binding Focus = %#v, %v mutations=%d", refused, err, backend.mutations)
	}
	writeLifecycleBinding(t, root, created.Binding)

	focused, err := FocusLifecycle(context.Background(), LifecycleRequest{Target: target}, deps)
	if err != nil || focused.Outcome != string(OutcomeAttached) || backend.mutations != mutations+1 {
		t.Fatalf("Focus = %#v, %v mutations=%d", focused, err, backend.mutations)
	}
	backend.closeLeavesResource = true
	unexpected, err := CloseLifecycle(context.Background(), LifecycleRequest{Target: target}, deps)
	if err != nil || unexpected.Outcome != string(OutcomeActionRequired) || unexpected.ReasonCode != "post_mutation_state_unexpected" || unexpected.State != string(InspectPresent) || backend.mutations != mutations+2 {
		t.Fatalf("Close with resource retained = %#v, %v mutations=%d", unexpected, err, backend.mutations)
	}
	backend.closeLeavesResource = false
	closed, err := CloseLifecycle(context.Background(), LifecycleRequest{Target: target}, deps)
	if err != nil || closed.Outcome != string(OutcomeClosed) || closed.State != string(InspectAbsent) || backend.mutations != mutations+3 {
		t.Fatalf("Close = %#v, %v mutations=%d", closed, err, backend.mutations)
	}
	if _, err := LoadBinding(root); err != nil {
		t.Fatalf("Close removed durable binding: %v", err)
	}
}

func TestInspectMissingBindingCreatesNothing(t *testing.T) {
	project := t.TempDir()
	sessionPath := filepath.Join(project, ".agent-mail", "collab")
	if err := fsq.EnsureRootDirs(sessionPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionPath, "meta", "config.json"), []byte(`{"version":1,"agents":["operator"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := prepareTreeSnapshot(t, sessionPath)
	result, err := InspectLifecycle(context.Background(), LifecycleRequest{Target: PrepareTarget{
		ProjectRoot: project, SessionRoot: sessionPath, Session: "collab",
	}}, LifecycleDependencies{})
	if err != nil || result.Outcome != LifecycleOutcomeInspected || result.State != string(InspectAbsent) || result.ReasonCode != "binding_missing" {
		t.Fatalf("missing binding Inspect = %#v, %v", result, err)
	}
	if after := prepareTreeSnapshot(t, sessionPath); after != before {
		t.Fatal("missing-binding Inspect changed the session tree")
	}
	if _, err := os.Stat(filepath.Join(sessionPath, "meta", "launch")); !os.IsNotExist(err) {
		t.Fatalf("Inspect created meta/launch: %v", err)
	}
}

type lifecycleCountingBackend struct {
	*TmuxBackend
	unknown             bool
	closeLeavesResource bool
	mutations           int
}

func (backend *lifecycleCountingBackend) Inspect(request InspectRequest) (InspectResult, error) {
	if backend.unknown {
		return InspectResult{Status: InspectUnknown, Evidence: "injected unknown", ActionRequired: true}, nil
	}
	return backend.TmuxBackend.Inspect(request)
}

func (backend *lifecycleCountingBackend) Focus(request FocusRequest) (FocusResult, error) {
	backend.mutations++
	return backend.TmuxBackend.Focus(request)
}

func (backend *lifecycleCountingBackend) Close(request CloseRequest) (CloseResult, error) {
	backend.mutations++
	if backend.closeLeavesResource {
		return CloseResult{Outcome: OutcomeClosed}, nil
	}
	return backend.TmuxBackend.Close(request)
}

func writeLifecycleBinding(t *testing.T, root *fsq.DeliveryRoot, binding BindingRecord) {
	t.Helper()
	lease, err := AcquireLease(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBinding(root, lease, binding); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func writeLifecycleBindingRaw(t *testing.T, sessionRoot string, binding BindingRecord) {
	t.Helper()
	data, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(BindingPath(sessionRoot), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
