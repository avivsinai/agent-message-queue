package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestTmuxBackendLifecycleAndRecovery(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	fakeAMQ := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(fakeAMQ, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70e7a5"
	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{
		{Handle: "claude", Argv: []string{"/bin/sleep", "60"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7a6"},
		{Handle: "codex", Argv: []string{"/bin/sleep", "60"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7a7"},
	}}
	backend := NewTmuxBackend("tmux")
	backend.socketName = fmt.Sprintf("amq-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { stopTmuxTestServer(t, backend) })

	detect := backend.Detect()
	if !detect.Available {
		t.Fatalf("Detect = %#v", detect)
	}
	if detect.Profile.VersionRange != tmuxProfileVersionRange || !detect.Profile.Has(CapReclaim) {
		t.Fatalf("profile = %#v", detect.Profile)
	}
	created, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Outcome != OutcomeCreated || len(created.Binding.Resources.Resources) != 3 {
		t.Fatalf("Create = %#v", created)
	}
	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status != InspectPresent {
		t.Fatalf("Inspect = %#v, %v", inspection, err)
	}

	journal := LaunchJournal{
		ProjectIdentity: project, Session: "collab", Plan: plan, LaunchNonce: nonce,
		HostIdentity: detect.HostIdentity, InstanceIdentity: detect.InstanceIdentity,
		Backend: LauncherTMux, Profile: detect.Profile.Identity(),
	}
	journal.RootIdentity, err = canonicalIdentity(root.Base())
	if err != nil {
		t.Fatal(err)
	}
	journal.RootPhysical, _ = fsq.StableTreeIdentityInfo(root.FileInfo())
	reclaimed, err := backend.Reclaim(ReclaimRequest{Context: context.Background(), Journal: journal, Root: root})
	if err != nil || reclaimed.Status != ReclaimAdoptable || len(reclaimed.Resources) != 3 {
		t.Fatalf("Reclaim = %#v, %v", reclaimed, err)
	}

	if _, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root}); err == nil {
		t.Fatal("duplicate Create succeeded")
	}
	afterDuplicate, _ := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if afterDuplicate.Status != InspectPresent {
		t.Fatalf("duplicate Create changed live resource: %#v", afterDuplicate)
	}

	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	name, err := tmuxSessionName(project, "collab")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.run(ctx, backend.args("set-environment", "-t", "="+name, tmuxNonceEnvironment, "foreign")...); err != nil {
		t.Fatal(err)
	}
	foreign, _ := backend.Reclaim(ReclaimRequest{Context: ctx, Journal: journal, Root: root})
	if foreign.Status != ReclaimForeign {
		t.Fatalf("foreign Reclaim = %#v", foreign)
	}
	if _, err := backend.run(ctx, backend.args("set-environment", "-t", "="+name, tmuxNonceEnvironment, nonce)...); err != nil {
		t.Fatal(err)
	}
	foreignBinding := created.Binding
	foreignBinding.InstanceIdentity = "tmux-socket:/foreign"
	foreignInspection, err := backend.Inspect(InspectRequest{Binding: foreignBinding, Root: root})
	if err != nil || foreignInspection.Status != InspectUnknown {
		t.Fatalf("foreign Inspect = %#v, %v", foreignInspection, err)
	}

	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, %v", closed, err)
	}
	absent, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || absent.Status != InspectAbsent {
		t.Fatalf("Inspect after Close = %#v, %v", absent, err)
	}
}

func TestTmuxBackendConformance(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	backend := NewTmuxBackend("tmux")
	backend.socketName = fmt.Sprintf("amq-conformance-%d-%d", os.Getpid(), time.Now().UnixNano())
	backend.focus = func(context.Context, string) error { return nil }
	t.Cleanup(func() { stopTmuxTestServer(t, backend) })
	RunConformance(t, backend)
}

func TestTmuxReconcileCrashRestartThenRelaunchResumes(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	backend := NewTmuxBackend("tmux")
	backend.socketName = fmt.Sprintf("amq-restart-%d-%d", os.Getpid(), time.Now().UnixNano())
	backend.focus = func(context.Context, string) error { return nil }
	req := reconcileFixture(t, backend)
	fakeAMQ := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(fakeAMQ, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Register after every TempDir used by the live pane so process teardown
	// runs before Go removes those directories.
	t.Cleanup(func() { stopTmuxTestServer(t, backend) })
	req.AMQPath = fakeAMQ
	req.HostIdentity = backend.Detect().HostIdentity
	crash := fmt.Errorf("injected process crash")
	req.CrashHook = func(stage string) error {
		if stage == "backend_created" {
			return crash
		}
		return nil
	}
	first, err := Reconcile(req)
	if !errors.Is(err, crash) || first.Plan == nil {
		t.Fatalf("crashed Reconcile = %#v, %v", first, err)
	}
	if _, err := LoadJournal(req.Root); err != nil {
		t.Fatalf("journal after crash: %v", err)
	}
	req.CrashHook = nil
	recovered, err := Reconcile(req)
	if err != nil || recovered.AggregateCode != 0 || recovered.Recovery == nil || recovered.Recovery.Status != ReclaimAdoptable {
		t.Fatalf("recovered Reconcile = %#v, %v", recovered, err)
	}
	binding, err := LoadBinding(req.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJournal(req.Root); !os.IsNotExist(err) {
		t.Fatalf("journal after recovery: %v", err)
	}
	relaunched, err := Reconcile(req)
	if err != nil || relaunched.AggregateCode != 0 || relaunched.Outcome != OutcomeAttached || relaunched.Agents[0].ConversationDisposition != DispositionResumed {
		t.Fatalf("resume Reconcile = %#v, %v", relaunched, err)
	}
	inspection, err := backend.Inspect(InspectRequest{Binding: binding, Root: req.Root})
	if err != nil || inspection.Status != InspectPresent {
		t.Fatalf("resource after resume = %#v, %v", inspection, err)
	}
}

func TestTmuxBackendReclaimReportsExactPartialInventory(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude", "codex")
	fakeAMQ := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(fakeAMQ, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70e7b5"
	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{
		{Handle: "claude", Argv: []string{"/bin/sleep", "60"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7b6"},
		{Handle: "codex", Argv: []string{"/bin/sleep", "60"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7b7"},
	}}
	backend := NewTmuxBackend("tmux")
	backend.socketName = fmt.Sprintf("amq-partial-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { stopTmuxTestServer(t, backend) })
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: fakeAMQ, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	var removed string
	for _, resource := range created.Binding.Resources.Resources {
		if resource.Agent == "codex" {
			removed = strings.TrimPrefix(resource.OpaqueID, "tmux:v1:window:")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	if _, err := backend.run(ctx, backend.args("kill-window", "-t", removed)...); err != nil {
		t.Fatal(err)
	}
	detect := backend.Detect()
	rootIdentity, err := canonicalIdentity(root.Base())
	if err != nil {
		t.Fatal(err)
	}
	rootPhysical, _ := fsq.StableTreeIdentityInfo(root.FileInfo())
	journal := LaunchJournal{ProjectIdentity: project, RootIdentity: rootIdentity, RootPhysical: rootPhysical, Session: "collab", Plan: plan, LaunchNonce: nonce, HostIdentity: detect.HostIdentity, InstanceIdentity: detect.InstanceIdentity, Backend: LauncherTMux, Profile: detect.Profile.Identity()}
	reclaimed, err := backend.Reclaim(ReclaimRequest{Context: ctx, Journal: journal, Root: root})
	if err != nil || reclaimed.Status != ReclaimIncomplete || !strings.Contains(reclaimed.Evidence, "codex") || len(reclaimed.Resources) != 2 {
		t.Fatalf("partial Reclaim = %#v, %v", reclaimed, err)
	}
}

func TestSupportedTmuxVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{{"tmux 3.2", true}, {"tmux 3.7b", true}, {"tmux 3.1c", false}, {"tmux 4.0", false}, {"screen 3.7", false}} {
		if got := supportedTmuxVersion(tc.version); got != tc.want {
			t.Errorf("supportedTmuxVersion(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestTmuxCreateMissingAMQRefusesBeforeServerMutation(t *testing.T) {
	project := t.TempDir()
	root := tmuxTestRoot(t, "claude")
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70e7c5"
	plan := Plan{Version: PlanVersion, Agents: []AgentPlan{{
		Handle: "claude", Argv: []string{"/bin/sleep", "60"}, Cwd: project,
		AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh,
		LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e7c6",
	}}}
	backend := NewTmuxBackend("tmux")
	backend.socketName = fmt.Sprintf("amq-missing-%d-%d", os.Getpid(), time.Now().UnixNano())
	_, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan,
		AMQPath: filepath.Join(t.TempDir(), "missing-amq"), Root: root,
	})
	var definite *DefinitePreCreateError
	if !errors.As(err, &definite) {
		t.Fatalf("Create error = %v, want definite pre-create refusal", err)
	}
	if _, statErr := os.Lstat(backend.socketPath()); !os.IsNotExist(statErr) {
		t.Fatalf("missing amq created a tmux socket: %v", statErr)
	}
}

func TestTmuxSessionNameBindsFullSessionName(t *testing.T) {
	project := t.TempDir()
	first, err := tmuxSessionName(project, "session-with-a-very-long-shared-prefix-alpha")
	if err != nil {
		t.Fatal(err)
	}
	second, err := tmuxSessionName(project, "session-with-a-very-long-shared-prefix-bravo")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("long session names collided: %q", first)
	}
}

func tmuxTestRoot(t *testing.T, handles ...string) *fsq.DeliveryRoot {
	t.Helper()
	path := t.TempDir()
	if err := fsq.EnsureRootDirs(path); err != nil {
		t.Fatal(err)
	}
	config := `{"version":1,"agents":["` + strings.Join(handles, `","`) + `"]}`
	if err := os.WriteFile(filepath.Join(path, "meta", "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	repaired := fsq.RepairMailboxLayoutForAgents(root, handles)
	if repaired.Status != "repaired" {
		t.Fatalf("repair mailbox root: %#v", repaired)
	}
	return root
}

func stopTmuxTestServer(t *testing.T, backend *TmuxBackend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	output, err := backend.run(ctx, backend.args("list-panes", "-a", "-F", "#{pane_pid}")...)
	if err != nil {
		return
	}
	pids := make([]int, 0, 2)
	for _, field := range strings.Fields(output) {
		pid, parseErr := strconv.Atoi(field)
		if parseErr != nil || pid <= 0 {
			t.Fatalf("parse tmux test pane pid %q", field)
		}
		pids = append(pids, pid)
	}
	if len(pids) == 0 {
		t.Fatal("tmux test server reported no panes")
	}
	if output, err := backend.run(ctx, backend.args("kill-server")...); err != nil {
		t.Fatalf("kill tmux test server: %v\n%s", err, output)
	}
	deadline := time.Now().Add(tmuxCommandTimeout)
	for {
		live := pids[:0]
		for _, pid := range pids {
			if processAlive(pid) {
				live = append(live, pid)
			}
		}
		pids = live
		if len(pids) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tmux test teardown left pane pids alive after %s: %v", tmuxCommandTimeout, pids)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
