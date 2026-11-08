//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

// Claude S1 F2: a lost thread/start reply had no operator exit. The command
// reports the exact attempt, leaves it on dry run, and explicitly abandons it.
func TestCleanupUncertainLaunchExecution(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	session := filepath.Join(project, "session")
	if err := fsq.EnsureRootDirs(session); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(session, "codex"); err != nil {
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
	provider := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(provider, []byte("provider"), 0o700); err != nil {
		t.Fatal(err)
	}
	amq, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nonce := "abababab-abab-4bab-8bab-abababababab"
	plan, err := launch.NewCodexAdapter(provider).PlanFreshDaemon(launch.PlanRequest{Handle: "codex", LaunchNonce: nonce,
		ProjectRoot: project, SessionRoot: session, Cwd: project, AMQExecutable: amq, ResumePolicy: launch.ResumeEnabled})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := launch.NewExecutionTicket(launch.ExecutionTicketRequest{Handle: "codex", LaunchNonce: nonce,
		Mode: plan.AdapterMode, Provider: launch.CodexProvider, ProviderVersion: launch.CodexDaemonVersion,
		PreSpawnAcquire: true, Backend: launch.CommandsBackendName, Profile: launch.CommandsProfile().Identity(),
		ProjectRoot: project, SessionRoot: session, Cwd: project, ProviderExecutable: provider, AMQExecutable: amq,
		TargetArgv: plan.Argv, DynamicArgv: plan.DynamicArgv, State: launch.ExecutionIdentityRequested, Reason: "identity_requested"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := launch.AcquireLease(root, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.LockHandles("codex"); err != nil {
		t.Fatal(err)
	}
	if err := launch.WriteExecutionTicket(root, lease, ticket); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	args := []string{"--root", session, "--handle", "codex", "--nonce", nonce, "--target", provider, "--"}
	err = runLaunchExec(append(args, plan.Argv...))
	if GetExitCode(err) != ExitActionRequired || !strings.Contains(err.Error(), "cleanup --launch-execution codex") {
		t.Fatalf("uncertain execution does not explain recovery with exit 6: %v", err)
	}
	if err := runCleanup([]string{"--launch-execution", "codex", "--dry-run"}); GetExitCode(err) != ExitUsage {
		t.Fatalf("implicit-root cleanup = %v", err)
	}
	var report struct {
		Removed bool `json:"removed"`
		Ticket  struct {
			Handle string `json:"handle"`
			Nonce  string `json:"launch_nonce"`
		} `json:"launch_execution"`
	}
	stdout, _, err := captureEnvOutput(t, func() error {
		return runCleanup([]string{"--launch-execution", "codex", "--root", session, "--dry-run", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || report.Removed || report.Ticket.Handle != "codex" || report.Ticket.Nonce != nonce {
		t.Fatalf("dry run = %s, %v", stdout, err)
	}
	if _, err := launch.LoadExecutionTicket(root, "codex"); err != nil {
		t.Fatalf("dry run removed ticket: %v", err)
	}
	stdout, _, err = captureEnvOutput(t, func() error {
		return runCleanup([]string{"--launch-execution", "codex", "--root", session, "--yes", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || !report.Removed {
		t.Fatalf("cleanup = %s, %v", stdout, err)
	}
	if _, err := launch.LoadExecutionTicket(root, "codex"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ticket after cleanup = %v", err)
	}
}
