//go:build !windows

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	amqBinary := filepath.Join(binDir, "amq")
	build := exec.Command("go", "build", "-o", amqBinary, "./cmd/amq")
	build.Dir = repo
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real amq: %v\n%s", err, output)
	}

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
		Handle: "claude", ProjectRoot: canonicalProject, Cwd: canonicalProject,
		LaunchNonce: "019c5a10-75d8-7eef-8db7-5ee77f70e8a1", ResumePolicy: launch.ResumeEnabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	trustPlan(t, store, launch.Plan{Version: launch.PlanVersion, Agents: []launch.AgentPlan{freshPlan}}, root)

	socketDir, err := os.MkdirTemp("/tmp", "amq-tmux-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	env := append(os.Environ(),
		"HOME="+t.TempDir(), "XDG_STATE_HOME="+xdgState, "TMUX_TMPDIR="+socketDir,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	t.Cleanup(func() { killHermeticTmuxServer(socketDir) })
	firstOutput := runRealAMQ(t, amqBinary, project, env, "launch", "--launcher", "tmux", "--json")
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

	killHermeticTmuxServer(socketDir)
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = project, env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real amq %v: %v\n%s", args, err, output)
	}
	return output
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

func killHermeticTmuxServer(socketDir string) {
	cmd := exec.Command("tmux", "kill-server")
	cmd.Env = append(os.Environ(), "TMUX_TMPDIR="+socketDir)
	_ = cmd.Run()
}
