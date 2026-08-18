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

func TestTmuxPlacementSessionLayouts(t *testing.T) {
	for _, layout := range []string{PlacementLayoutColumns, PlacementLayoutRows, PlacementLayoutTiled} {
		t.Run(layout, func(t *testing.T) {
			backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
			created, err := backend.Create(CreateRequest{
				ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
				Placement: &Placement{Target: PlacementTargetSession, Layout: layout},
			})
			if err != nil {
				t.Fatal(err)
			}
			windows, panes, sessions := countTmuxResourceKinds(created.Binding)
			if sessions != 1 || windows != 0 || panes != 2 {
				t.Fatalf("session+%s resources windows=%d panes=%d sessions=%d, %#v", layout, windows, panes, sessions, created.Binding.Resources)
			}
			if created.Binding.Placement.Effective.Layout != layout || !created.Binding.Placement.Supported {
				t.Fatalf("preview = %#v", created.Binding.Placement)
			}
			closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
			if err != nil || closed.Outcome != OutcomeClosed {
				t.Fatalf("Close = %#v, %v", closed, err)
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

func TestTmuxPlacementUnsupportedRefusesBeforeMutation(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude")
	_, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns},
	})
	var definite *DefinitePreCreateError
	if err == nil || !errors.As(err, &definite) || !strings.Contains(err.Error(), PlacementUnsupportedReason) {
		t.Fatalf("unsupported Create error = %v", err)
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

func TestTmuxPlacementCrashRestartRecoversExactTarget(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	for _, tc := range []struct {
		name      string
		target    string
		needsHost bool
	}{
		{"session", PlacementTargetSession, false},
		{"new_window", PlacementTargetNewWindow, true},
		{"current_window", PlacementTargetCurrentWindow, true},
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

func TestTmuxPlacementJoinPaneDoesNotInspectPresentOrCloseWindow(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	hostPane, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{pane_id}")...)
	if err != nil {
		t.Fatal(err)
	}
	hostPane = strings.TrimSpace(hostPane)
	created, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parseTmuxWindowOwned(created.Binding); !ok {
		t.Fatalf("binding has no owned window: %#v", created.Binding)
	}
	for _, pane := range tmuxOwnedPaneIDs(created.Binding) {
		if _, joinErr := backend.run(ctx, backend.args("join-pane", "-d", "-s", pane, "-t", hostPane)...); joinErr != nil {
			t.Fatal(joinErr)
		}
	}
	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status == InspectPresent {
		t.Fatalf("Inspect after join-pane reported present-as-created: %#v, %v", inspection, err)
	}
	for _, pane := range tmuxOwnedPaneIDs(created.Binding) {
		still, paneErr := backend.paneExists(ctx, pane)
		if paneErr != nil || !still {
			t.Fatalf("owned pane %s missing after join-pane: %v", pane, paneErr)
		}
	}
	if inspection.Status == InspectAbsent || !inspection.ActionRequired {
		t.Fatalf("Inspect after join-pane treated live panes as absent: %#v", inspection)
	}
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close after join-pane = %#v, %v", closed, err)
	}
	stillHost, err := backend.paneExists(ctx, hostPane)
	if err != nil || !stillHost {
		t.Fatalf("host pane missing after Close of joined-out owned panes: %v", err)
	}
	for _, pane := range tmuxOwnedPaneIDs(created.Binding) {
		still, paneErr := backend.paneExists(ctx, pane)
		if paneErr != nil || still {
			t.Fatalf("owned pane %s still live after Close: exists=%v err=%v", pane, still, paneErr)
		}
	}
}

func TestTmuxPlacementCurrentWindowInspectRejectsPanesMovedFromLauncher(t *testing.T) {
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
	other, err := backend.run(ctx, backend.args("new-window", "-d", "-P", "-F", "#{pane_id}")...)
	if err != nil {
		t.Fatal(err)
	}
	other = strings.TrimSpace(other)
	for _, pane := range tmuxOwnedPaneIDs(created.Binding) {
		if _, joinErr := backend.run(ctx, backend.args("join-pane", "-d", "-s", pane, "-t", other)...); joinErr != nil {
			t.Fatal(joinErr)
		}
	}
	stillLauncher, err := backend.paneExists(ctx, launcher)
	if err != nil || !stillLauncher {
		t.Fatalf("launcher missing after join: %v", err)
	}
	inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
	if err != nil || inspection.Status == InspectPresent {
		t.Fatalf("Inspect after moving panes out of launcher window = %#v, %v", inspection, err)
	}
	for _, pane := range tmuxOwnedPaneIDs(created.Binding) {
		still, paneErr := backend.paneExists(ctx, pane)
		if paneErr != nil || !still {
			t.Fatalf("owned pane %s missing: %v", pane, paneErr)
		}
	}
	if inspection.Status == InspectAbsent || !inspection.ActionRequired {
		t.Fatalf("Inspect treated live panes as absent: %#v", inspection)
	}
}

func TestTmuxPlacementNewWindowCrashRestartDoesNotAdoptHostAfterJoin(t *testing.T) {
	backend, req, hostPane := startPlacementCrash(t, PlacementTargetNewWindow)
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	journal, err := LoadJournal(req.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, pane := range listNoncePanes(t, backend, journal.LaunchNonce) {
		if _, joinErr := backend.run(ctx, backend.args("join-pane", "-d", "-s", pane, "-t", hostPane)...); joinErr != nil {
			t.Fatal(joinErr)
		}
	}
	req.CrashHook = nil
	recovered, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Recovery == nil || recovered.Recovery.Status == ReclaimAdoptable || recovered.AggregateCode != 6 {
		t.Fatalf("recovery after join into host = %#v", recovered)
	}
	if _, err := LoadJournal(req.Root); err != nil {
		t.Fatalf("journal was cleared after incomplete reclaim: %v", err)
	}
	if _, err := backend.run(ctx, backend.args("has-session", "-t", "=host")...); err != nil {
		t.Fatalf("host session missing after fail-closed recovery: %v", err)
	}
}

func TestTmuxPlacementCurrentWindowCrashRestartFailsClosedWithoutLauncherOrMembership(t *testing.T) {
	t.Run("killed_launcher", func(t *testing.T) {
		backend, req, launcher := startPlacementCrash(t, PlacementTargetCurrentWindow)
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		defer cancel()
		if _, err := backend.run(ctx, backend.args("kill-pane", "-t", launcher)...); err != nil {
			t.Fatal(err)
		}
		assertPlacementCrashFailClosed(t, req)
	})
	t.Run("moved_panes", func(t *testing.T) {
		backend, req, _ := startPlacementCrash(t, PlacementTargetCurrentWindow)
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		defer cancel()
		journal, err := LoadJournal(req.Root)
		if err != nil {
			t.Fatal(err)
		}
		other, err := backend.run(ctx, backend.args("new-window", "-d", "-P", "-F", "#{pane_id}")...)
		if err != nil {
			t.Fatal(err)
		}
		other = strings.TrimSpace(other)
		for _, pane := range listNoncePanes(t, backend, journal.LaunchNonce) {
			if _, joinErr := backend.run(ctx, backend.args("join-pane", "-d", "-s", pane, "-t", other)...); joinErr != nil {
				t.Fatal(joinErr)
			}
		}
		assertPlacementCrashFailClosed(t, req)
	})
}

func startPlacementCrash(t *testing.T, target string) (*TmuxBackend, ReconcileRequest, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	backend := NewTmuxBackend("tmux")
	backend.socketName = fmt.Sprintf("amq-place-hostile-%s-%d-%d", target, os.Getpid(), time.Now().UnixNano())
	backend.focus = func(context.Context, string) error { return nil }
	req := reconcileFixture(t, backend)
	fakeAMQ := writeTmuxSleepAMQ(t)
	t.Cleanup(func() { stopTmuxTestServer(t, backend) })
	req.AMQPath = fakeAMQ
	req.HostIdentity = backend.Detect().HostIdentity
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	hostPane, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{pane_id}")...)
	if err != nil {
		t.Fatal(err)
	}
	hostPane = strings.TrimSpace(hostPane)
	placement := &Placement{Target: target, Layout: PlacementLayoutColumns}
	if target == PlacementTargetCurrentWindow {
		placement.LauncherPane = hostPane
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
	return backend, req, hostPane
}

func assertPlacementCrashFailClosed(t *testing.T, req ReconcileRequest) {
	t.Helper()
	req.CrashHook = nil
	recovered, err := Reconcile(req)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Recovery == nil || recovered.Recovery.Status == ReclaimAdoptable || recovered.AggregateCode != 6 {
		t.Fatalf("expected fail-closed recovery, got %#v", recovered)
	}
	if _, err := LoadJournal(req.Root); err != nil {
		t.Fatalf("journal was cleared after incomplete reclaim: %v", err)
	}
}

func listNoncePanes(t *testing.T, backend *TmuxBackend, nonce string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	out, err := backend.run(ctx, backend.args("list-panes", "-a", "-F", "#{pane_id}\t#{@amq_launch_nonce}")...)
	if err != nil {
		t.Fatal(err)
	}
	panes := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 2 && fields[1] == nonce && fields[0] != "" {
			panes = append(panes, fields[0])
		}
	}
	if len(panes) == 0 {
		t.Fatal("no nonce-marked panes after crash")
	}
	return panes
}

func TestTmuxPlacementStaggerDeadlineCoversSleep(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	var sleeps []time.Duration
	backend.sleep = func(ctx context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < delay {
			return fmt.Errorf("create ctx does not cover stagger %s remaining=%v ok=%v", delay, time.Until(deadline), ok)
		}
		return nil
	}
	created, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetSession, Layout: PlacementLayoutColumns, StaggerMS: 60_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 1 || sleeps[0] != 60*time.Second {
		t.Fatalf("stagger sleeps = %v", sleeps)
	}
	if _, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root}); err != nil {
		t.Fatal(err)
	}
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

func TestTmuxWindowInventoryStatusExactSet(t *testing.T) {
	present, _ := tmuxWindowInventoryStatus(map[string]bool{"%1": true, "%2": true}, []string{"%1", "%2"}, nil)
	if present != InspectPresent {
		t.Fatalf("exact owned set = %s, want present", present)
	}
	unknown, evidence := tmuxWindowInventoryStatus(map[string]bool{"%1": true, "%2": true, "%3": true}, []string{"%1", "%2"}, nil)
	if unknown != InspectUnknown || !strings.Contains(evidence, "unowned pane") {
		t.Fatalf("extra pane = %s %q", unknown, evidence)
	}
	incomplete, _ := tmuxWindowInventoryStatus(map[string]bool{"%1": true}, []string{"%1", "%2"}, nil)
	if incomplete != InspectUnknown {
		t.Fatalf("missing owned pane = %s, want unknown", incomplete)
	}
	absent, _ := tmuxWindowInventoryStatus(map[string]bool{}, []string{"%1", "%2"}, nil)
	if absent != InspectAbsent {
		t.Fatalf("no owned panes = %s, want absent", absent)
	}
	current, _ := tmuxWindowInventoryStatus(map[string]bool{"%1": true, "%2": true, "%9": true}, []string{"%1", "%2"}, []string{"%9"})
	if current != InspectPresent {
		t.Fatalf("owned plus launcher = %s, want present", current)
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

func TestTmuxPlacementCloseDoesNotKillWindowAfterForeignJoinInterleaving(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	foreign, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{pane_id}")...)
	if err != nil {
		t.Fatal(err)
	}
	foreign = strings.TrimSpace(foreign)
	if _, err := backend.run(ctx, backend.args("new-window", "-d", "-t", "=host", "/bin/sleep", "60")...); err != nil {
		t.Fatal(err)
	}
	created, err := backend.Create(CreateRequest{
		ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
		Placement: &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns},
	})
	if err != nil {
		t.Fatal(err)
	}
	owned := tmuxOwnedPaneIDs(created.Binding)
	if len(owned) == 0 {
		t.Fatal("binding has no owned panes")
	}
	real := backend.run
	joined := false
	sawWindow := false
	sawSession := false
	backend.run = func(runCtx context.Context, args ...string) (string, error) {
		if tmuxArgsHas(args, "kill-window") {
			sawWindow = true
		}
		if tmuxArgsHas(args, "kill-session") {
			sawSession = true
		}
		if !joined && (tmuxArgsHas(args, "kill-window") || tmuxArgsHas(args, "kill-pane")) {
			joined = true
			if _, joinErr := real(runCtx, backend.args("join-pane", "-d", "-s", foreign, "-t", owned[0])...); joinErr != nil {
				return "", joinErr
			}
		}
		return real(runCtx, args...)
	}
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("Close = %#v, %v", closed, err)
	}
	if !joined {
		t.Fatal("Close issued neither kill-pane nor kill-window")
	}
	if sawWindow || sawSession {
		t.Fatalf("Close issued container kill kill-window=%v kill-session=%v", sawWindow, sawSession)
	}
	still, err := backend.paneExists(ctx, foreign)
	if err != nil || !still {
		t.Fatalf("foreign pane joined between inspect and kill was destroyed: exists=%v err=%v", still, err)
	}
}

func TestTmuxPlacementOmittedClosePreservesLateForeignPane(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	foreign, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{pane_id}")...)
	if err != nil {
		t.Fatal(err)
	}
	foreign = strings.TrimSpace(foreign)
	if _, err := backend.run(ctx, backend.args("new-window", "-d", "-t", "=host", "/bin/sleep", "60")...); err != nil {
		t.Fatal(err)
	}
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	owned := tmuxOwnedPaneIDs(created.Binding)
	if len(owned) == 0 {
		t.Fatal("omitted create persisted no pane IDs")
	}
	if _, err := backend.run(ctx, backend.args("join-pane", "-d", "-s", foreign, "-t", owned[0])...); err != nil {
		t.Fatal(err)
	}
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeClosed {
		t.Fatalf("omitted Close after late foreign join = %#v, %v", closed, err)
	}
	still, err := backend.paneExists(ctx, foreign)
	if err != nil || !still {
		t.Fatalf("foreign pane destroyed by omitted Close: exists=%v err=%v", still, err)
	}
}

func TestTmuxPlacementSessionFallbackCloseDoesNotKillUnmarkedPane(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	foreign, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{pane_id}")...)
	if err != nil {
		t.Fatal(err)
	}
	foreign = strings.TrimSpace(foreign)
	if _, err := backend.run(ctx, backend.args("new-window", "-d", "-t", "=host", "/bin/sleep", "60")...); err != nil {
		t.Fatal(err)
	}
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	owned := tmuxOwnedPaneIDs(created.Binding)
	legacy := bindingWithoutPaneResources(created.Binding)
	if len(tmuxOwnedPaneIDs(legacy)) != 0 {
		t.Fatal("legacy binding still has pane IDs")
	}
	if _, err := backend.run(ctx, backend.args("join-pane", "-d", "-s", foreign, "-t", owned[0])...); err != nil {
		t.Fatal(err)
	}
	closed, err := backend.Close(CloseRequest{Binding: legacy, Root: root})
	if err != nil || closed.Outcome != OutcomeActionRequired || closed.Reason != ownedPaneIdentityUnavailable {
		t.Fatalf("session-fallback Close = %#v, %v", closed, err)
	}
	still, err := backend.paneExists(ctx, foreign)
	if err != nil || !still {
		t.Fatalf("unmarked foreign pane destroyed via session membership: exists=%v err=%v", still, err)
	}
	for _, pane := range owned {
		stillOwned, paneErr := backend.paneExists(ctx, pane)
		if paneErr != nil || !stillOwned {
			t.Fatalf("session-fallback Close killed pane %s: exists=%v err=%v", pane, stillOwned, paneErr)
		}
	}
}

func TestTmuxPlacementCloseFailsClosedWhenOwnedPaneIdentityUnavailable(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	foreign, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{pane_id}")...)
	if err != nil {
		t.Fatal(err)
	}
	foreign = strings.TrimSpace(foreign)
	if _, err := backend.run(ctx, backend.args("new-window", "-d", "-t", "=host", "/bin/sleep", "60")...); err != nil {
		t.Fatal(err)
	}
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	owned := tmuxOwnedPaneIDs(created.Binding)
	for _, pane := range owned {
		if _, err := backend.run(ctx, backend.args("set-option", "-p", "-u", "-t", pane, "@amq_pane_agent")...); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.run(ctx, backend.args("set-option", "-p", "-u", "-t", pane, tmuxPaneNonceOption)...); err != nil {
			t.Fatal(err)
		}
	}
	legacy := bindingWithoutPaneResources(created.Binding)
	if _, err := backend.run(ctx, backend.args("join-pane", "-d", "-s", foreign, "-t", owned[0])...); err != nil {
		t.Fatal(err)
	}
	closed, err := backend.Close(CloseRequest{Binding: legacy, Root: root})
	if err != nil || closed.Outcome != OutcomeActionRequired || closed.Reason != ownedPaneIdentityUnavailable {
		t.Fatalf("Close without owned identity = %#v, %v", closed, err)
	}
	still, err := backend.paneExists(ctx, foreign)
	if err != nil || !still {
		t.Fatalf("foreign pane destroyed when identity unavailable: exists=%v err=%v", still, err)
	}
	for _, pane := range owned {
		stillOwned, paneErr := backend.paneExists(ctx, pane)
		if paneErr != nil || !stillOwned {
			t.Fatalf("Close killed pane %s without owned identity: exists=%v err=%v", pane, stillOwned, paneErr)
		}
	}
}

func bindingWithoutPaneResources(binding BindingRecord) BindingRecord {
	filtered := make([]ResourceIdentity, 0, len(binding.Resources.Resources))
	for _, resource := range binding.Resources.Resources {
		if _, ok := parseTmuxPaneResource(resource.OpaqueID); ok {
			continue
		}
		filtered = append(filtered, resource)
	}
	binding.Resources.Resources = filtered
	return binding
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

func TestTmuxPlacementCloseRefusesReusedPaneIDsAfterServerRestart(t *testing.T) {
	backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	created, err := backend.Create(CreateRequest{ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if parseTmuxEpoch(created.Binding) == "" {
		t.Fatal("create did not persist server epoch")
	}
	name, err := tmuxSessionName(project, "collab")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.run(ctx, backend.args("kill-server")...); err != nil {
		t.Fatal(err)
	}
	first, err := backend.run(ctx, backend.args("new-session", "-d", "-s", name, "-P", "-F", "#{pane_id}", "/bin/sleep", "60")...)
	if err != nil {
		t.Fatal(err)
	}
	first = strings.TrimSpace(first)
	second, err := backend.run(ctx, backend.args("split-window", "-t", first, "-P", "-F", "#{pane_id}", "/bin/sleep", "60")...)
	if err != nil {
		t.Fatal(err)
	}
	second = strings.TrimSpace(second)
	closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
	if err != nil || closed.Outcome != OutcomeActionRequired ||
		(closed.Reason != tmuxServerEpochMismatch && closed.Reason != ownedPaneIdentityUnavailable) {
		t.Fatalf("Close after server restart = %#v, %v", closed, err)
	}
	for _, pane := range []string{first, second} {
		still, paneErr := backend.paneExists(ctx, pane)
		if paneErr != nil || !still {
			t.Fatalf("reused pane %s destroyed after server restart Close: exists=%v err=%v", pane, still, paneErr)
		}
	}
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

func TestTmuxPlacementNewWindowCrashRestartRejectsForeignPane(t *testing.T) {
	backend, req, foreign := startPlacementCrash(t, PlacementTargetNewWindow)
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
	defer cancel()
	journal, err := LoadJournal(req.Root)
	if err != nil {
		t.Fatal(err)
	}
	owned := listNoncePanes(t, backend, journal.LaunchNonce)
	if _, err := backend.run(ctx, backend.args("join-pane", "-d", "-s", foreign, "-t", owned[0])...); err != nil {
		t.Fatal(err)
	}
	assertPlacementCrashFailClosed(t, req)
	still, err := backend.paneExists(ctx, foreign)
	if err != nil || !still {
		t.Fatalf("foreign pane missing after fail-closed recovery: exists=%v err=%v", still, err)
	}
}

func TestTmuxPlacementOwnedWindowAdversarialMatrix(t *testing.T) {
	t.Run("owned_pane_killed", func(t *testing.T) {
		backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		defer cancel()
		if _, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host")...); err != nil {
			t.Fatal(err)
		}
		created, err := backend.Create(CreateRequest{
			ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
			Placement: &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns},
		})
		if err != nil {
			t.Fatal(err)
		}
		owned := tmuxOwnedPaneIDs(created.Binding)
		if _, err := backend.run(ctx, backend.args("kill-pane", "-t", owned[0])...); err != nil {
			t.Fatal(err)
		}
		inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
		if err != nil || inspection.Status != InspectUnknown || !inspection.ActionRequired {
			t.Fatalf("Inspect after killing owned pane = %#v, %v", inspection, err)
		}
		closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
		if err != nil || closed.Outcome != OutcomeClosed {
			t.Fatalf("Close after partial kill = %#v, %v", closed, err)
		}
		still, err := backend.paneExists(ctx, owned[1])
		if err != nil || still {
			t.Fatalf("remaining owned pane still live: exists=%v err=%v", still, err)
		}
	})
	t.Run("session_renamed", func(t *testing.T) {
		backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		defer cancel()
		if _, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host")...); err != nil {
			t.Fatal(err)
		}
		created, err := backend.Create(CreateRequest{
			ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
			Placement: &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.run(ctx, backend.args("rename-session", "-t", "=host", "renamed")...); err != nil {
			t.Fatal(err)
		}
		inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
		if err != nil || inspection.Status != InspectPresent {
			t.Fatalf("Inspect after host session rename = %#v, %v", inspection, err)
		}
	})
	t.Run("window_moved_between_sessions", func(t *testing.T) {
		backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		defer cancel()
		if _, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host")...); err != nil {
			t.Fatal(err)
		}
		created, err := backend.Create(CreateRequest{
			ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
			Placement: &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns},
		})
		if err != nil {
			t.Fatal(err)
		}
		windowID, ok := parseTmuxWindowOwned(created.Binding)
		if !ok {
			t.Fatal("missing owned window")
		}
		if _, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "other")...); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.run(ctx, backend.args("move-window", "-s", windowID, "-t", "other:")...); err != nil {
			t.Fatal(err)
		}
		inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
		if err != nil || inspection.Status != InspectPresent {
			t.Fatalf("Inspect after move-window = %#v, %v", inspection, err)
		}
	})
	t.Run("nonce_mismatch_pane_with_marker", func(t *testing.T) {
		backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		defer cancel()
		if _, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host")...); err != nil {
			t.Fatal(err)
		}
		created, err := backend.Create(CreateRequest{
			ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
			Placement: &Placement{Target: PlacementTargetNewWindow, Layout: PlacementLayoutColumns},
		})
		if err != nil {
			t.Fatal(err)
		}
		owned := tmuxOwnedPaneIDs(created.Binding)
		extra, err := backend.run(ctx, backend.args("split-window", "-d", "-t", owned[0], "-P", "-F", "#{pane_id}", "/bin/sleep", "60")...)
		if err != nil {
			t.Fatal(err)
		}
		extra = strings.TrimSpace(extra)
		if err := backend.markPane(ctx, extra, "claude", "019c5a10-75d8-7eef-8db7-000000000099"); err != nil {
			t.Fatal(err)
		}
		inspection, err := backend.Inspect(InspectRequest{Binding: created.Binding, Root: root})
		if err != nil || inspection.Status == InspectPresent {
			t.Fatalf("Inspect with nonce-mismatch pane = %#v, %v", inspection, err)
		}
		closed, err := backend.Close(CloseRequest{Binding: created.Binding, Root: root})
		if err != nil || closed.Outcome != OutcomeClosed {
			t.Fatalf("Close with nonce-mismatch pane = %#v, %v", closed, err)
		}
		still, err := backend.paneExists(ctx, extra)
		if err != nil || !still {
			t.Fatalf("nonce-mismatch pane was destroyed: %v", err)
		}
		for _, pane := range owned {
			stillOwned, paneErr := backend.paneExists(ctx, pane)
			if paneErr != nil || stillOwned {
				t.Fatalf("owned pane %s still live after Close: exists=%v err=%v", pane, stillOwned, paneErr)
			}
		}
	})
	t.Run("two_launches_in_one_window", func(t *testing.T) {
		backend, project, root, plan := newTmuxPlacementFixture(t, "claude", "codex")
		ctx, cancel := context.WithTimeout(context.Background(), tmuxCommandTimeout)
		defer cancel()
		launcher, err := backend.run(ctx, backend.args("new-session", "-d", "-s", "host", "-P", "-F", "#{pane_id}")...)
		if err != nil {
			t.Fatal(err)
		}
		launcher = strings.TrimSpace(launcher)
		first, err := backend.Create(CreateRequest{
			ProjectRoot: project, Session: "collab", Plan: plan, AMQPath: writeTmuxSleepAMQ(t), Root: root,
			Placement: &Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns, LauncherPane: launcher},
		})
		if err != nil {
			t.Fatal(err)
		}
		plan2 := plan
		plan2.Agents = append([]AgentPlan(nil), plan.Agents...)
		nonce2 := "019c5a10-75d8-7eef-8db7-5ee77f70e811"
		for i := range plan2.Agents {
			plan2.Agents[i].LaunchNonce = nonce2
			plan2.Agents[i].ConversationID = fmt.Sprintf("019c5a10-75d8-7eef-8db7-5ee77f70e8%02d", i+10)
		}
		second, err := backend.Create(CreateRequest{
			ProjectRoot: project, Session: "collab", Plan: plan2, AMQPath: writeTmuxSleepAMQ(t), Root: root,
			Placement: &Placement{Target: PlacementTargetCurrentWindow, Layout: PlacementLayoutColumns, LauncherPane: launcher},
		})
		if err != nil {
			t.Fatal(err)
		}
		inspection, err := backend.Inspect(InspectRequest{Binding: first.Binding, Root: root})
		if err != nil || inspection.Status == InspectPresent {
			t.Fatalf("first launch Inspect after second launch = %#v, %v", inspection, err)
		}
		inspection2, err := backend.Inspect(InspectRequest{Binding: second.Binding, Root: root})
		if err != nil || inspection2.Status == InspectPresent {
			t.Fatalf("second launch Inspect with first launch panes = %#v, %v", inspection2, err)
		}
		closed, err := backend.Close(CloseRequest{Binding: first.Binding, Root: root})
		if err != nil || closed.Outcome != OutcomeClosed {
			t.Fatalf("Close of first launch while second panes share the window = %#v, %v", closed, err)
		}
		stillLauncher, err := backend.paneExists(ctx, launcher)
		if err != nil || !stillLauncher {
			t.Fatalf("launcher destroyed: %v", err)
		}
		for _, pane := range tmuxOwnedPaneIDs(first.Binding) {
			still, paneErr := backend.paneExists(ctx, pane)
			if paneErr != nil || still {
				t.Fatalf("first launch pane %s still live: exists=%v err=%v", pane, still, paneErr)
			}
		}
		for _, pane := range tmuxOwnedPaneIDs(second.Binding) {
			still, paneErr := backend.paneExists(ctx, pane)
			if paneErr != nil || !still {
				t.Fatalf("second launch pane %s destroyed: %v", pane, paneErr)
			}
		}
	})
}

func tmuxArgsHas(args []string, token string) bool {
	for _, arg := range args {
		if arg == token {
			return true
		}
	}
	return false
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
