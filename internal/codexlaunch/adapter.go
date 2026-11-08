// Package codexlaunch connects managed launch tickets to Codex's native daemon.
package codexlaunch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/codexidentity"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
)

type adapter struct {
	*launch.CodexAdapter
	executable string
}

// New retains the embedded adapter when a daemon seat cannot be proven safe.
func New(executable string) launch.HarnessAdapter {
	return &adapter{CodexAdapter: launch.NewCodexAdapter(executable), executable: executable}
}

func (a *adapter) Capabilities(ctx context.Context) launch.AdapterCapabilities {
	capabilities := a.CodexAdapter.Capabilities(ctx)
	capabilities.PreSpawnAcquire = capabilities.Capture && capabilities.ProviderVersion == launch.CodexDaemonVersion
	return capabilities
}

func (a *adapter) PlanFresh(request launch.PlanRequest) (launch.AgentPlan, error) {
	reason := eligibility(a.executable, request.Cwd)
	if reason == "" && (len(request.CommittedArgs) != 0 || len(request.BypassArgs) != 0 || len(request.EnvOverlay) != 0 || request.Wrapper != nil) {
		reason = "launch options require verified TUI resume equivalence"
	}
	if reason == "" {
		return a.PlanFreshDaemon(request)
	}
	plan, err := a.CodexAdapter.PlanFresh(request)
	plan.TerminalOnlyReason = reason
	return plan, err
}

func (a *adapter) PlanResume(request launch.ResumeRequest) (launch.AgentPlan, error) {
	plan, err := a.CodexAdapter.PlanResume(request)
	plan.TerminalOnlyReason = eligibility(a.executable, request.Cwd)
	if plan.TerminalOnlyReason == "" {
		plan.TerminalOnlyReason = "resumed launch settings require verified TUI resume equivalence"
	}
	return plan, err
}

func codexHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(harness.CodexHome(home))
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return absolute, nil
	}
	return canonical, err
}

// EmbeddedEnvironment reports provider exclusions shared by ordinary TUI
// discovery and managed launch admission. Presence, including empty, matters.
func EmbeddedEnvironment() bool {
	for _, key := range []string{"CODEX_EXEC_SERVER_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE"} {
		if _, set := os.LookupEnv(key); set {
			return true
		}
	}
	return false
}

func eligibility(executable, cwd string) string {
	if EmbeddedEnvironment() {
		return "the configured executor requires an embedded app-server"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	home, err := codexHome()
	if err != nil {
		return "Codex home cannot be verified"
	}
	sock, err := codex.ControlSocket(home)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "no Codex app-server daemon is running (start one with codex app-server daemon start)"
		}
		return "Codex app-server daemon cannot be verified"
	}
	capabilities := launch.NewCodexAdapter(executable).Capabilities(ctx)
	if !capabilities.Available || capabilities.ProviderVersion != launch.CodexDaemonVersion {
		return "Codex daemon launch requires a verified provider version"
	}
	login := exec.CommandContext(ctx, executable, "login", "status")
	login.Dir = cwd
	if err := login.Run(); err != nil {
		return "Codex sign-in status is not established"
	}
	if !TrustsDir(home, cwd) {
		return "Codex has not established directory trust"
	}
	state, err := codex.ReadStartupState(ctx, sock, cwd)
	if err == nil && state.Version != launch.CodexDaemonVersion {
		return "the running Codex daemon version is not verified"
	}
	if err != nil || !HooksListClear(state.Hooks, cwd) {
		return "Codex startup hook review is unresolved"
	}
	var config struct {
		Config struct {
			Features map[string]bool `json:"features"`
		} `json:"config"`
	}
	if json.Unmarshal(state.Config, &config) != nil || config.Config.Features["terminal_visualization_instructions"] {
		return "Codex terminal instructions require verified TUI resume equivalence"
	}
	if !ConfigUpdateCheckOff(state.Config) || !RequirementsAllowNoUpdateCheck(state.Requirements) {
		return "Codex startup update prompt is not ruled out"
	}
	return ""
}

// Acquire creates a native thread only after launch has persisted its attempt
// barrier under the exact generation's lease. An uncertain attempt is not retried.
func Acquire(ticket launch.ExecutionTicket) (launch.CaptureEvidence, error) {
	if ticket.Provider != launch.CodexProvider || !ticket.PreSpawnAcquire {
		return launch.CaptureEvidence{}, fmt.Errorf("not a Codex daemon acquisition ticket")
	}
	if reason := eligibility(ticket.ProviderExecutable, ticket.Cwd); reason != "" {
		return launch.CaptureEvidence{}, fmt.Errorf("%w: Buzz approvals: terminal only: %s; replan this launch", launch.ErrIdentityNotRequested, reason)
	}
	home, err := codexHome()
	if err != nil {
		return launch.CaptureEvidence{}, errors.Join(launch.ErrIdentityNotRequested, err)
	}
	sock, err := codex.ControlSocket(home)
	if err != nil {
		return launch.CaptureEvidence{}, errors.Join(launch.ErrIdentityNotRequested, err)
	}
	pid, err := codex.DaemonPID(sock)
	if err != nil {
		return launch.CaptureEvidence{}, errors.Join(launch.ErrIdentityNotRequested, err)
	}
	holder, err := launch.HolderOf(pid)
	if err != nil {
		return launch.CaptureEvidence{}, errors.Join(launch.ErrIdentityNotRequested, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	thread, err := codex.StartThread(ctx, sock, ticket.Cwd, "", launch.CodexDaemonVersion)
	if err != nil {
		return launch.CaptureEvidence{}, err
	}
	currentPID, err := codex.DaemonPID(sock)
	if err != nil || currentPID != pid {
		return launch.CaptureEvidence{}, fmt.Errorf("codex daemon changed during thread acquisition")
	}
	current, err := launch.HolderOf(currentPID)
	if err != nil || current != holder {
		return launch.CaptureEvidence{}, fmt.Errorf("codex daemon identity changed during thread acquisition")
	}
	return launch.ObserveCodexDaemonThread(ticket, thread, sock, holder)
}

// RecordIdentity publishes routing only after the acquired thread and current
// daemon have been checked, while the ticket's generation still holds its lease.
func RecordIdentity(root *fsq.DeliveryRoot, ticket launch.ExecutionTicket, env []string) error {
	if reason := eligibility(ticket.ProviderExecutable, ticket.Cwd); reason != "" {
		return fmt.Errorf("cannot prepare native seat: Buzz approvals: terminal only: %s; replan this launch", reason)
	}
	home, err := codexHome()
	if err != nil {
		return err
	}
	sock, err := codex.ControlSocket(home)
	if err != nil {
		return err
	}
	pid, err := codex.DaemonPID(sock)
	if err != nil {
		return err
	}
	holder, err := launch.HolderOf(pid)
	if err != nil {
		return err
	}
	if err := launch.ValidateCodexDaemonEndpoint(root, ticket, sock, holder); err != nil {
		return err
	}
	cwd, _, err := codex.ThreadCwd(sock, ticket.ConversationID)
	if err != nil || cwd != ticket.Cwd {
		return fmt.Errorf("codex daemon thread does not match the launch directory")
	}
	get := func(key string) string {
		for _, entry := range env {
			if value, ok := strings.CutPrefix(entry, key+"="); ok {
				return value
			}
		}
		return ""
	}
	if get("AM_ROOT") != ticket.SessionRoot || get("AM_ME") != ticket.Handle {
		return fmt.Errorf("codex daemon routing does not match the execution ticket")
	}
	currentPID, err := codex.DaemonPID(sock)
	if err != nil || currentPID != pid {
		return fmt.Errorf("codex daemon changed during routing publication")
	}
	currentHolder, err := launch.HolderOf(currentPID)
	if err != nil || currentHolder != holder {
		return fmt.Errorf("codex daemon identity changed during routing publication")
	}
	return codexidentity.Publish(codexidentity.Record{
		Thread: ticket.ConversationID, CodexHome: home, Root: get("AM_ROOT"), BaseRoot: get("AM_BASE_ROOT"),
		Session: get("AM_SESSION"), Me: get("AM_ME"), RootID: get("AM_ROOT_ID"), BaseRootID: get("AM_BASE_ROOT_ID"),
	})
}
