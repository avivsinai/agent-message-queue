//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

func TestTmuxRealBinaryFreshServerRestartResumeLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes the real amq binary")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	repo, err := cliTestRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	amqBinary := filepath.Join(binDir, "amq")
	buildTestAMQ(t, repo, amqBinary)

	project := t.TempDir()
	canonicalProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	providerLog := filepath.Join(t.TempDir(), "provider.log")
	provider := filepath.Join(binDir, "claude")
	providerScript := fmt.Sprintf(`#!/bin/sh
case "$1" in
  --version) echo "1.0.0 (Claude Code)"; exit 0 ;;
  --help) echo "--session-id <uuid> --resume [value]"; exit 0 ;;
esac
printf '%%s\n' "$*" >> %s
exec /bin/sleep 60
`, shellQuoteArg(providerLog))
	if err := os.WriteFile(provider, []byte(providerScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(project, ".amqrc"), []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".amq"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectConfig := launch.ProjectConfig{
		Schema: launch.ProjectConfigSchema, DefaultSession: "collab", Layout: launch.LayoutIntent{Type: launch.LayoutColumns},
		Agents: []launch.ProjectAgentConfig{{
			Handle: "claude", Adapter: launch.ClaudeProvider, Command: []string{launch.ClaudeProvider}, ResumePolicy: launch.ResumeEnabled,
		}},
	}
	projectData, err := launch.MarshalProjectConfig(projectConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, setupConfigPath), projectData, 0o600); err != nil {
		t.Fatal(err)
	}
	localData, err := launch.MarshalLocalConfig(launch.LocalConfig{
		Schema: launch.LocalConfigSchema, LauncherPreference: []string{launch.LauncherTMux, launch.LauncherCommands},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, setupLocalConfigPath), localData, 0o600); err != nil {
		t.Fatal(err)
	}
	baseRoot := filepath.Join(project, defaultCoopRoot)
	sessionRoot := filepath.Join(baseRoot, "collab")
	for _, rootPath := range []string{baseRoot, sessionRoot} {
		if err := fsq.EnsureRootDirs(rootPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, "meta", "config.json"), []byte(`{"version":1,"agents":["claude"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsq.EnsureAgentDirs(sessionRoot, "claude"); err != nil {
		t.Fatal(err)
	}

	identity, err := fsq.SnapshotDeliveryRoot(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(sessionRoot, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	xdgState := t.TempDir()
	store, err := launch.OpenTrustStore(filepath.Join(xdgState, "amq"), project)
	if err != nil {
		t.Fatal(err)
	}
	adapter := launch.NewClaudeAdapter(launch.ClaudeProvider)
	freshPlan, err := adapter.PlanFresh(launch.PlanRequest{
		Handle: "claude", Session: "collab", ProjectRoot: canonicalProject, Cwd: canonicalProject,
		LaunchNonce: "019c5a10-75d8-7eef-8db7-5ee77f70e8a1", Named: true, ResumePolicy: launch.ResumeEnabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	freshPlan.Execution = &launch.PrepareExecutionOptions{Named: true}
	trustPlan(t, store, launch.Plan{Version: launch.PlanVersion, Agents: []launch.AgentPlan{freshPlan}}, root)

	socketDir, err := os.MkdirTemp("/tmp", "amq-tmux-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	env := append(os.Environ(),
		"HOME="+t.TempDir(), "XDG_STATE_HOME="+xdgState, "TMUX_TMPDIR="+socketDir,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AMQ_NO_UPDATE_CHECK=1",
	)
	serverRunning := false
	t.Cleanup(func() { stopHermeticTmuxServer(t, socketDir, sessionRoot, "claude", serverRunning) })
	firstOutput := runRealAMQ(t, amqBinary, project, env, "launch", "--launcher", "tmux", "--json")
	serverRunning = true
	var first launch.ReconcileResult
	if err := json.Unmarshal(firstOutput, &first); err != nil || first.AggregateCode != 0 || first.Outcome != launch.OutcomeCreated {
		t.Fatalf("fresh launch = %s, decode=%v", firstOutput, err)
	}
	waitForProviderLog(t, providerLog, "--session-id ")
	firstTicket, err := launch.LoadExecutionTicket(root, "claude")
	if err != nil || firstTicket.State != launch.ExecutionAcknowledged {
		t.Fatalf("fresh execution ticket = %#v, %v", firstTicket, err)
	}
	recordData, err := os.ReadFile(launch.ConversationPath(sessionRoot, "claude"))
	if err != nil {
		t.Fatal(err)
	}
	var record launch.ConversationRecord
	if err := json.Unmarshal(recordData, &record); err != nil || record.State != launch.CaptureReady || record.Identity.ID == "" {
		t.Fatalf("fresh conversation = %s, decode=%v", recordData, err)
	}

	serverRunning = false
	stopHermeticTmuxServer(t, socketDir, sessionRoot, "claude", true)
	resumePlan, err := adapter.PlanResume(launch.ResumeRequest{
		PlanRequest: launch.PlanRequest{
			Handle: "claude", ProjectRoot: canonicalProject, Cwd: canonicalProject,
			LaunchNonce: "019c5a10-75d8-7eef-8db7-5ee77f70e8a2", ResumePolicy: launch.ResumeEnabled,
		},
		Conversation: record.Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	trustPlan(t, store, launch.Plan{Version: launch.PlanVersion, Agents: []launch.AgentPlan{resumePlan}}, root)
	secondOutput := runRealAMQ(t, amqBinary, project, env, "session", "resume", "collab", "--launcher", "tmux", "--json")
	serverRunning = true
	var second launch.ReconcileResult
	if err := json.Unmarshal(secondOutput, &second); err != nil || second.AggregateCode != 0 || second.Outcome != launch.OutcomeCreated ||
		len(second.Agents) != 1 || second.Agents[0].ConversationDisposition != launch.DispositionResumed {
		t.Fatalf("resume launch = %s, decode=%v", secondOutput, err)
	}
	waitForProviderLog(t, providerLog, "--resume "+record.Identity.ID)
	secondTicket, err := launch.LoadExecutionTicket(root, "claude")
	if err != nil || secondTicket.State != launch.ExecutionAcknowledged {
		t.Fatalf("resume execution ticket = %#v, %v", secondTicket, err)
	}
	logData, err := os.ReadFile(providerLog)
	if err != nil || !strings.Contains(string(logData), "--session-id "+record.Identity.ID) || !strings.Contains(string(logData), "--resume "+record.Identity.ID) {
		t.Fatalf("provider log = %q, err=%v", logData, err)
	}
}

func trustPlan(t *testing.T, store *launch.TrustStore, plan launch.Plan, root *fsq.DeliveryRoot) {
	t.Helper()
	digest, err := launch.ExecutionTrustDigest(plan, "collab", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Replace(launch.TrustRecord{SemanticDigest: digest}); err != nil {
		t.Fatal(err)
	}
}

func runRealAMQ(t *testing.T, binary, project string, env []string, args ...string) []byte {
	t.Helper()
	stdout, stderr, err := runRealAMQStreams(t, binary, project, env, args...)
	if err != nil {
		t.Fatalf("real amq %v: %v\nstdout=%s\nstderr=%s", args, err, stdout, stderr)
	}
	return stdout
}

func runRealAMQStreams(t *testing.T, binary, project string, env []string, args ...string) ([]byte, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = project, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func TestRunRealAMQJSONParseIgnoresUpdateHintOnStderr(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes the real amq binary")
	}
	repo, err := cliTestRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	amqBinary := filepath.Join(t.TempDir(), "amq")
	buildTestAMQ(t, repo, amqBinary, "-ldflags", "-X main.version=0.62.1-99-gdeadbeef")
	home := t.TempDir()
	cachePayload := []byte(`{"checked_at":"2026-08-17T00:00:00Z","latest_version":"0.63.0"}`)
	for _, cacheDir := range []string{
		filepath.Join(home, "Library", "Caches", "amq"),
		filepath.Join(home, ".cache", "amq"),
	} {
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cacheDir, "update.json"), cachePayload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{
		"HOME=" + home,
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"PATH=" + filepath.Dir(amqBinary) + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	root := t.TempDir()
	if _, stderr, err := runRealAMQStreams(t, amqBinary, root, env, "init", "--root", root, "--agents", "claude"); err != nil {
		t.Fatalf("real amq init: %v\nstderr=%s", err, stderr)
	}
	stdout, stderr, err := runRealAMQStreams(t, amqBinary, root, env, "env", "--root", root, "--json")
	if err != nil {
		t.Fatalf("real amq env --json: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	if !strings.Contains(string(stderr), "amq: update available") {
		t.Fatalf("stderr missing update hint:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(stdout, &payload); err != nil {
		t.Fatalf("stdout JSON parse failed with update hint on stderr: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
}

func waitForProviderLog(t *testing.T, path, needle string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), needle) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("provider did not record %q: %q", needle, data)
}

type hermeticTmuxProcess struct {
	pid  int
	role string
}

func stopHermeticTmuxServer(t *testing.T, socketDir, sessionRoot, handle string, requireServer bool) {
	t.Helper()
	processes, serverFound, wakeFound, err := hermeticTmuxProcesses(socketDir, sessionRoot, handle)
	if err != nil {
		t.Fatal(err)
	}
	if requireServer && !serverFound {
		t.Fatal("hermetic tmux server disappeared before teardown")
	}
	if requireServer && !wakeFound {
		wake, waitErr := waitForHermeticWakeLock(sessionRoot, handle, wakeProcessExitTimeout)
		if waitErr != nil {
			t.Fatal(waitErr)
		}
		processes = append(processes, hermeticTmuxProcess{pid: wake.PID, role: "wake"})
	}
	if serverFound {
		cmd := exec.Command("tmux", "kill-server")
		cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+socketDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("kill hermetic tmux server: %v\n%s", err, output)
		}
	}
	if err := waitForHermeticTmuxProcesses(processes, wakeProcessExitTimeout); err != nil {
		t.Fatal(err)
	}
}

func hermeticTmuxProcesses(socketDir, sessionRoot, handle string) ([]hermeticTmuxProcess, bool, bool, error) {
	cmd := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_pid}")
	cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+socketDir)
	output, err := cmd.CombinedOutput()
	serverFound := err == nil
	processes := make([]hermeticTmuxProcess, 0, 2)
	if serverFound {
		for _, field := range strings.Fields(string(output)) {
			pid, parseErr := strconv.Atoi(field)
			if parseErr != nil || pid <= 0 {
				return nil, true, false, fmt.Errorf("parse hermetic tmux pane pid %q", field)
			}
			processes = append(processes, hermeticTmuxProcess{pid: pid, role: "pane"})
		}
		if len(processes) == 0 {
			return nil, true, false, fmt.Errorf("hermetic tmux server reported no panes")
		}
	}
	wake := inspectWakeLock(sessionRoot, handle)
	if wake.Exists {
		if wake.PID <= 0 {
			return nil, serverFound, true, fmt.Errorf("hermetic wake lock has invalid pid %d", wake.PID)
		}
		processes = append(processes, hermeticTmuxProcess{pid: wake.PID, role: "wake"})
	}
	return processes, serverFound, wake.Exists, nil
}

const hermeticWakeLockPollInterval = 10 * time.Millisecond

func waitForHermeticWakeLock(sessionRoot, handle string, timeout time.Duration) (wakeLockInspection, error) {
	return waitForHermeticWakeLockNow(sessionRoot, handle, timeout, time.Now, time.Sleep)
}

func waitForHermeticWakeLockNow(sessionRoot, handle string, timeout time.Duration, now func() time.Time, sleep func(time.Duration)) (wakeLockInspection, error) {
	deadline := now().Add(timeout)
	var wake wakeLockInspection
	for {
		wake = inspectWakeLock(sessionRoot, handle)
		if wake.Exists && wake.PID > 0 {
			return wake, nil
		}
		if now().After(deadline) {
			if wake.Exists && wake.PID <= 0 {
				return wake, fmt.Errorf("hermetic wake lock has invalid pid %d", wake.PID)
			}
			return wake, fmt.Errorf("hermetic tmux pane had no wake lock for %s", handle)
		}
		sleep(hermeticWakeLockPollInterval)
	}
}

func waitForHermeticTmuxProcesses(processes []hermeticTmuxProcess, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		live := processes[:0]
		for _, process := range processes {
			if processAlive(process.pid) {
				live = append(live, process)
			}
		}
		processes = live
		if len(processes) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			parts := make([]string, 0, len(processes))
			for _, process := range processes {
				parts = append(parts, fmt.Sprintf("%s pid %d", process.role, process.pid))
			}
			return fmt.Errorf("hermetic tmux teardown left processes alive after %s: %s", timeout, strings.Join(parts, ", "))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWaitForHermeticTmuxProcessesReportsLeak(t *testing.T) {
	err := waitForHermeticTmuxProcesses(
		[]hermeticTmuxProcess{{pid: os.Getpid(), role: "test-owner"}},
		20*time.Millisecond,
	)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("test-owner pid %d", os.Getpid())) {
		t.Fatalf("live-process wait error = %v", err)
	}
}

func TestWaitForHermeticWakeLockReportsMissing(t *testing.T) {
	start := time.Date(2026, 8, 17, 19, 0, 0, 0, time.UTC)
	now := start
	timeout := 20 * time.Millisecond
	_, err := waitForHermeticWakeLockNow(t.TempDir(), "claude", timeout, func() time.Time {
		return now
	}, func(d time.Duration) {
		now = now.Add(d)
	})
	if err == nil || !strings.Contains(err.Error(), "hermetic tmux pane had no wake lock for claude") {
		t.Fatalf("missing wake lock error = %v", err)
	}
	assertHermeticWaitBoundedByTimeout(t, start, now, timeout)
}

func TestWaitForHermeticWakeLockSeesLateLock(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatal(err)
	}
	type result struct {
		wake wakeLockInspection
		err  error
	}
	got := make(chan result, 1)
	go func() {
		wake, err := waitForHermeticWakeLock(root, "claude", time.Second)
		got <- result{wake: wake, err: err}
	}()
	time.Sleep(40 * time.Millisecond)
	writeWakeLockForTest(t, root, "claude", wakeLock{PID: os.Getpid()})
	out := <-got
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.wake.PID != os.Getpid() {
		t.Fatalf("late wake lock pid = %d, want %d", out.wake.PID, os.Getpid())
	}
}

func TestWaitForHermeticWakeLockDoesNotAcceptTornPidZero(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatal(err)
	}
	type result struct {
		wake wakeLockInspection
		err  error
	}
	got := make(chan result, 1)
	go func() {
		wake, err := waitForHermeticWakeLock(root, "claude", time.Second)
		got <- result{wake: wake, err: err}
	}()
	lockPath := filepath.Join(fsq.AgentBase(root, "claude"), ".wake.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("write torn wake lock: %v", err)
	}
	select {
	case out := <-got:
		t.Fatalf("waiter returned during torn write: pid=%d err=%v", out.wake.PID, out.err)
	case <-time.After(50 * time.Millisecond):
	}
	writeWakeLockForTest(t, root, "claude", wakeLock{PID: os.Getpid()})
	out := <-got
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.wake.PID != os.Getpid() {
		t.Fatalf("completed wake lock pid = %d, want %d", out.wake.PID, os.Getpid())
	}
}

func TestWaitForHermeticWakeLockReportsInvalidPidAfterTimeout(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, "claude"), ".wake.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatalf("write torn wake lock: %v", err)
	}
	start := time.Date(2026, 8, 17, 19, 0, 0, 0, time.UTC)
	now := start
	timeout := 30 * time.Millisecond
	wake, err := waitForHermeticWakeLockNow(root, "claude", timeout, func() time.Time {
		return now
	}, func(d time.Duration) {
		now = now.Add(d)
	})
	if err == nil || !strings.Contains(err.Error(), "invalid pid 0") {
		t.Fatalf("torn-lock timeout error = %v pid=%d, want invalid pid 0", err, wake.PID)
	}
	assertHermeticWaitBoundedByTimeout(t, start, now, timeout)
}

func assertHermeticWaitBoundedByTimeout(t *testing.T, start, stopped time.Time, timeout time.Duration) {
	t.Helper()
	elapsed := stopped.Sub(start)
	if elapsed <= timeout {
		t.Fatalf("waiter stopped at %s, want after timeout %s", elapsed, timeout)
	}
	if elapsed > timeout+hermeticWakeLockPollInterval {
		t.Fatalf("waiter stopped at %s, want at most timeout+poll %s", elapsed, timeout+hermeticWakeLockPollInterval)
	}
}
