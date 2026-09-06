package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestPlacementOmittedMatchesV061TmuxWindows(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	windows, panes, sessions := countTmuxResourceKinds(created.Binding)
	if sessions != 1 || windows != 0 || panes != 2 {
		t.Fatalf("omitted tmux resources windows=%d panes=%d sessions=%d want session+panes, %#v", windows, panes, sessions, created.Binding.Resources)
	}
	if created.Binding.Placement.Requested != nil || created.Binding.Placement.Effective.Target != PlacementTargetSession {
		t.Fatalf("omitted preview = %#v", created.Binding.Placement)
	}
	if got := countLiveTmuxWindows(t, backend); got != 2 {
		t.Fatalf("omitted live windows = %d, want v0.61 one window per agent", got)
	}
	for _, pane := range tmuxOwnedPaneIDs(created.Binding) {
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		agent, agentErr := backend.run(ctx, backend.args("display-message", "-p", "-t", pane, "#{@amq_pane_agent}")...)
		nonce, nonceErr := backend.run(ctx, backend.args("display-message", "-p", "-t", pane, "#{@amq_launch_nonce}")...)
		cancel()
		if agentErr != nil || nonceErr != nil || strings.TrimSpace(agent) == "" || strings.TrimSpace(nonce) != created.Binding.LaunchNonce {
			t.Fatalf("omitted pane %s markers agent=%q nonce=%q err=%v %v", pane, agent, nonce, agentErr, nonceErr)
		}
	}
}

func TestTmuxJoinCrashAfterEachWindowRetriesWithoutDuplicates(t *testing.T) {
	for _, crashAfter := range []int{1, 2} {
		t.Run(fmt.Sprintf("window_%d", crashAfter), func(t *testing.T) {
			backend, project, root, allPlan := newTmuxPlacementFixture(t, "claude", "codex", "operator")
			initialPlan := Plan{Version: PlanVersion, Agents: []AgentPlan{allPlan.Agents[0]}}
			created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: initialPlan, AMQPath: writeTmuxSleepAMQ(t), Root: root})
			if err != nil {
				t.Fatal(err)
			}
			nonce := created.Binding.LaunchNonce
			joinPlan := Plan{Version: PlanVersion, Agents: []AgentPlan{
				{Handle: "codex", Argv: []string{"/bin/sleep", "60"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e901"},
				{Handle: "operator", Argv: []string{"/bin/sleep", "60"}, Cwd: project, AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh, LaunchNonce: nonce, ConversationID: "019c5a10-75d8-7eef-8db7-5ee77f70e902"},
			}}
			var deltas []JoinDelta
			calls := 0
			crash := errors.New("crash after joined window")
			first, err := backend.Create(CreateRequest{
				ProjectRoot: project, Session: "collab", Plan: joinPlan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
				JoinBinding: &created.Binding, JoinProgress: func(delta JoinDelta) error {
					calls++
					deltas = append(deltas, delta)
					if calls == crashAfter {
						return crash
					}
					return nil
				},
			})
			if !errors.Is(err, crash) || first.Outcome != "" || len(deltas) != crashAfter {
				t.Fatalf("crashed join result=%#v err=%v deltas=%#v", first, err, deltas)
			}
			second, err := backend.Create(CreateRequest{
				ProjectRoot: project, Session: "collab", Plan: joinPlan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
				JoinBinding: &created.Binding, JoinDeltas: deltas,
				JoinProgress: func(delta JoinDelta) error { deltas = append(deltas, delta); return nil },
			})
			if err != nil || second.Outcome != OutcomeCreated || len(deltas) != 2 {
				t.Fatalf("recovered join result=%#v err=%v deltas=%#v", second, err, deltas)
			}
			if got := countLiveTmuxWindows(t, backend); got != 3 {
				t.Fatalf("joined windows=%d, want original plus two unique additions", got)
			}
			inspection, err := backend.Inspect(InspectRequest{Binding: second.Binding, Root: root})
			if err != nil || inspection.Status != InspectPresent {
				t.Fatalf("joined inspection=%#v err=%v", inspection, err)
			}
		})
	}
}

func TestTmuxPlacementCurrentWindowCloseLeavesLauncher(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	launcher, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{pane_id}")...)
	if err != nil {
		t.Fatal(err)
	}
	launcher = strings.TrimSpace(launcher)
	created, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns, LauncherPane: launcher},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, panes, sessions := countTmuxResourceKinds(created.Binding)
	if sessions != 0 || panes != 2 {
		t.Fatalf("current_window resources panes=%d sessions=%d, %#v", panes, sessions, created.Binding.Resources)
	}
	for _, resource := range created.Binding.Resources.Resources {
		if id, ok := parseTmuxPaneResource(resource.OpaqueID); ok && id == launcher {
			t.Fatalf("launcher pane was owned: %#v", created.Binding.Resources)
		}
	}
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, %v", closed, err)
	}
	exists, err := backend.paneExists(ctx, launcher)
	if err != nil || !exists {
		t.Fatalf("launcher pane after Close exists=%v err=%v", exists, err)
	}
}

func TestTmuxPlacementNewWindowCloseLeavesHostSession(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	if _, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{session_id}")...); err != nil {
		t.Fatal(err)
	}
	created, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutRows},
	})
	if err != nil {
		t.Fatal(err)
	}
	windows, panes, sessions := countTmuxResourceKinds(created.Binding)
	if sessions != 0 || windows != 1 || panes != 2 {
		t.Fatalf("new_window resources windows=%d panes=%d sessions=%d, %#v", windows, panes, sessions, created.Binding.Resources)
	}
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, %v", closed, err)
	}
	if _, err := backend.run(ctx, backend.args("has-session", "-t", "=host")...); err != nil {
		t.Fatalf("host session missing after Close: %v", err)
	}
}

func newTmuxPlacementFixture(t *testing.T, handles ...string) (*TmuxBackend, string, *fsq.DeliveryRoot, Plan) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	project := t.TempDir()
	root := tmuxTestRoot(t, handles...)
	backend := NewTmuxBackend("tmux")
	backend.socketName = fmt.Sprintf("amq-place-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { stopTmuxTestServer(t, backend) })
	nonce := "019c5a10-75d8-7eef-8db7-5ee77f70e801"
	agents := make([]AgentPlan, 0, len(handles))
	for i, handle := range handles {
		agents = append(agents, AgentPlan{
			Handle: handle, Argv: []string{"/bin/sleep", "60"}, Cwd: project,
			AdapterMode: AdapterModeMint, ResumePolicy: ResumeFresh,
			LaunchNonce: nonce, ConversationID: fmt.Sprintf("019c5a10-75d8-7eef-8db7-5ee77f70e8%02d", i+1),
		})
	}
	return backend, project, root, Plan{Version: PlanVersion, Agents: agents}
}

func writeTmuxSleepAMQ(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func countTmuxResourceKinds(binding BindingRecord) (windows, panes, sessions int) {
	for _, resource := range binding.Resources.Resources {
		switch {
		case strings.HasPrefix(resource.OpaqueID, "tmux:v1:session:"):
			sessions++
		case strings.HasPrefix(resource.OpaqueID, "tmux:v1:window:"):
			windows++
		case strings.HasPrefix(resource.OpaqueID, "tmux:v1:pane:"):
			panes++
		}
	}
	return windows, panes, sessions
}

func TestTmuxPlacementStaggerSleepsRealDelay(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	started := time.Now()
	created, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns, StaggerMS: 250},
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Fatalf("session stagger elapsed %s, want at least 250ms", elapsed)
	}
	if _, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root}); err != nil {
		t.Fatal(err)
	}
}

func TestTmuxPlacementNewWindowRejectsForeignPaneJoinedIn(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	foreign, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{pane_id}")...)
	if err != nil {
		t.Fatal(err)
	}
	foreign = strings.TrimSpace(foreign)
	created, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns},
	})
	if err != nil {
		t.Fatal(err)
	}
	windowID, ok := parseTmuxWindowOwned(created.Binding)
	if !ok {
		t.Fatalf("binding has no owned window: %#v", created.Binding)
	}
	owned := tmuxOwnedPaneIDs(created.Binding)
	if _, err := backend.run(ctx, backend.args("join-pane", "-d", "-s", foreign, "-t", owned[0])...); err != nil {
		t.Fatal(err)
	}
	foreignWindow, err := backend.paneWindowID(ctx, foreign)
	if err != nil || foreignWindow != windowID {
		t.Fatalf("foreign pane window=%s owned=%s err=%v", foreignWindow, windowID, err)
	}
	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status == InspectPresent {
		t.Fatalf("Inspect with foreign pane in owned window = %#v, %v", inspection, err)
	}
	if inspection.Status == InspectAbsent || !inspection.ActionRequired {
		t.Fatalf("Inspect treated mixed window as absent: %#v", inspection)
	}
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close with foreign pane in owned window = %#v, %v", closed, err)
	}
	still, err := backend.paneExists(ctx, foreign)
	if err != nil || !still {
		t.Fatalf("foreign pane was destroyed by Close: exists=%v err=%v", still, err)
	}
	for _, pane := range owned {
		stillOwned, paneErr := backend.paneExists(ctx, pane)
		if paneErr != nil || stillOwned {
			t.Fatalf("owned pane %s still live after Close: exists=%v err=%v", pane, stillOwned, paneErr)
		}
	}
}

func bindingWithEpoch(binding BindingRecord, epoch string) BindingRecord {
	resources := make([]ResourceIdentity, 0, len(binding.Resources.Resources))
	replaced := false
	for _, resource := range binding.Resources.Resources {
		if resource.Agent == "" && strings.HasPrefix(resource.OpaqueID, tmuxEpochPrefix) {
			resources = append(resources, ResourceIdentity{OpaqueID: tmuxEpochPrefix + epoch})
			replaced = true
			continue
		}
		resources = append(resources, resource)
	}
	if !replaced {
		resources = append(resources, ResourceIdentity{OpaqueID: tmuxEpochPrefix + epoch})
	}
	binding.Resources.Resources = resources
	return binding
}

func TestTmuxPlacementCloseRefusesEpochMismatch(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	owned := tmuxOwnedPaneIDs(created.Binding)
	closed, err := backend.Close(CloseRequest{Binding: bindingWithEpoch(created.Binding, "1"), Root: root})
	if err != nil || closed.Outcome != OutcomeActionRequired || closed.Reason != tmuxServerEpochMismatch {
		t.Fatalf("Close with forged epoch = %#v, %v", closed, err)
	}
	for _, pane := range owned {
		still, paneErr := backend.paneExists(ctx, pane)
		if paneErr != nil || !still {
			t.Fatalf("epoch mismatch Close killed pane %s: exists=%v err=%v", pane, still, paneErr)
		}
	}
}

func countLiveTmuxWindows(t *testing.T, backend *TmuxBackend) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	out, err := backend.run(ctx, backend.args("list-windows", "-a", "-F", "#{window_id}")...)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// TestTmuxNewSessionRetriesOnceOnServerExitRace is the az7 positive: when the
// first `new-session` hits the transient "server exited unexpectedly" server
// race, the create path retries exactly once and succeeds, yielding a working
// session (one window). Verifies the retry is bounded to a single attempt.
func TestTmuxNewSessionRetriesOnceOnServerExitRace(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude")
	realRun := backend.run
	newSessionCalls := 0
	backend.run = func(ctx context.Context, args ...string) (string, error) {
		if tmuxArgsIsNewSession(args) {
			newSessionCalls++
			if newSessionCalls == 1 {
				// Transient server startup race on the first attempt only.
				return "", fmt.Errorf("tmux new-session: %w: server exited unexpectedly", errors.New("exit status 1"))
			}
		}
		return realRun(ctx, args...)
	}
	_, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "az7-transient", Plan: plan,
		AMQPath: writeTmuxSleepAMQ(t), Root: root,
	})
	if err != nil {
		t.Fatalf("create after transient retry failed: %v", err)
	}
	if newSessionCalls != 2 {
		t.Fatalf("new-session calls = %d, want exactly 2 (one transient + one retry)", newSessionCalls)
	}
	if got := countLiveTmuxWindows(t, backend); got != 1 {
		t.Fatalf("live windows = %d after transient retry, want 1", got)
	}
}

// tmuxArgsIsNewSession reports whether a tmux arg slice (which may be prefixed
// by -L/-S socket flags from TmuxBackend.args) is a new-session command.
func tmuxArgsIsNewSession(args []string) bool {
	for _, a := range args {
		if a == "new-session" {
			return true
		}
	}
	return false
}

func TestTmuxPlacementCrashRestartRecoversExactTarget(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	for _, tc := range []struct {
		name      string
		target    string
		needsHost bool
	}{
		{"new_window", PlacementTargetNewWindow, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := NewTmuxBackend("tmux")
			backend.socketName = fmt.Sprintf("amq-place-crash-%s-%d-%d", tc.name, os.Getpid(), time.Now().UnixNano())
			backend.focus = func(context.Context, string) error { return nil }
			req := reconcileFixture(t, backend)
			fakeAMQ := writeTmuxSleepAMQ(t)
			t.Cleanup(func() { stopTmuxTestServer(t, backend) })
			req.AMQPath = fakeAMQ
			req.HostIdentity = backend.Detect().HostIdentity
			placement := &Placement{Target: tc.target, Layout: PlacementLayoutColumns}
			if tc.needsHost {
				ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
				defer cancel()
				pane, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{pane_id}")...)
				if err != nil {
					t.Fatal(err)
				}
				if tc.target == PlacementTargetCurrentWindow {
					placement.LauncherPane = strings.TrimSpace(pane)
				}
			}
			req.Placement = placement
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
			journal, err := LoadJournal(req.Root)
			if err != nil {
				t.Fatalf("journal after crash: %v", err)
			}
			if journal.Placement.Effective.Target != tc.target {
				t.Fatalf("journal placement = %#v, want target %s", journal.Placement, tc.target)
			}
			windowsBefore := countLiveTmuxWindows(t, backend)
			req.CrashHook = nil
			recovered, err := Reconcile(req)
			if err != nil || recovered.AggregateCode != 0 || recovered.Recovery == nil || recovered.Recovery.Status != ReclaimAdoptable {
				t.Fatalf("recovered Reconcile = %#v, %v", recovered, err)
			}
			if _, err := LoadJournal(req.Root); !os.IsNotExist(err) {
				t.Fatalf("journal after recovery: %v", err)
			}
			windowsAfter := countLiveTmuxWindows(t, backend)
			if windowsAfter != windowsBefore {
				t.Fatalf("recovery recreated resources: windows before=%d after=%d", windowsBefore, windowsAfter)
			}
			binding, err := LoadBinding(req.Root)
			if err != nil {
				t.Fatal(err)
			}
			inspection, err := backend.Inspect(InspectRequest{Binding: binding, Root: req.Root})
			if err != nil || inspection.Status != InspectPresent {
				t.Fatalf("resource after recovery = %#v, %v", inspection, err)
			}
		})
	}
}
