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

// PlanFresh admits the request statically before any probe runs the provider
// executable or reads the daemon.
func (a *adapter) PlanFresh(request launch.PlanRequest) (launch.AgentPlan, error) {
	if err := a.ValidatePlanRequest(request); err != nil {
		return launch.AgentPlan{}, err
	}
	reason := launch.CodexDaemonOptionsReason(request)
	if reason != "" {
		if _, _, endpointReason := daemonEndpoint(); endpointReason != "" {
			reason = endpointReason
		}
	} else {
		reason = eligibility(a.executable, request.Cwd)
	}
	if reason == "" {
		return a.PlanFreshDaemon(request)
	}
	plan, err := a.CodexAdapter.PlanFresh(request)
	plan.TerminalOnlyReason = reason
	return plan, err
}

// PlanResume keeps each conversation on the backend that created it. A
// daemon thread resumes on the daemon only while the daemon still proves it;
// otherwise the seat resumes embedded and names the cause. Static admission
// precedes every probe.
func (a *adapter) PlanResume(request launch.ResumeRequest) (launch.AgentPlan, error) {
	if err := a.ValidateResumeRequest(request); err != nil {
		return launch.AgentPlan{}, err
	}
	reason := resumeReason(a.executable, request)
	if reason == "" {
		return a.PlanResumeDaemon(request)
	}
	plan, err := a.CodexAdapter.PlanResume(request)
	if err != nil {
		return launch.AgentPlan{}, err
	}
	plan.TerminalOnlyReason = reason
	return plan, nil
}

func resumeReason(executable string, request launch.ResumeRequest) string {
	if request.Capture.Source != launch.CodexDaemonThreadV1 {
		return "this conversation runs on an embedded app-server"
	}
	if reason := launch.CodexDaemonOptionsReason(request.PlanRequest); reason != "" {
		return reason
	}
	if reason := eligibility(executable, request.Cwd); reason != "" {
		return reason
	}
	_, sock, _ := daemonEndpoint()
	return threadReason(request.Capture, sock, request.Conversation.ID, request.Cwd)
}

// threadReason requires the endpoint that captured the thread and a daemon
// there that reads the thread in its launch directory. A restarted daemon at
// that endpoint is its successor; another endpoint is not, whatever it answers.
func threadReason(capture launch.ConversationCapture, sock, thread, cwd string) string {
	if sock != capture.Socket {
		return "the Codex daemon endpoint changed since this conversation was captured"
	}
	if got, _, err := codex.ThreadCwd(sock, thread); err != nil || got != cwd {
		return "the running Codex daemon does not know this conversation's thread"
	}
	return ""
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

func daemonEndpoint() (home, socket, reason string) {
	if EmbeddedEnvironment() {
		return "", "", "the configured executor requires an embedded app-server"
	}
	home, err := codexHome()
	if err != nil {
		return "", "", "Codex home cannot be verified"
	}
	socket, err = codex.ControlSocket(home)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", "no Codex app-server daemon is running (start one with codex app-server daemon start)"
	}
	if err != nil {
		return "", "", "Codex app-server daemon cannot be verified"
	}
	return home, socket, ""
}

func eligibility(executable, cwd string) string {
	home, sock, reason := daemonEndpoint()
	if reason != "" {
		return reason
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
		return "the Codex update prompt is not ruled out; set check_for_update_on_startup = false in your Codex config to let this seat use the shared daemon"
	}
	return ""
}

// Acquire creates a native thread only after launch has persisted its attempt
// barrier under the exact generation's lease. An uncertain attempt is not
// retried. A resume ticket creates nothing: it binds its existing thread once
// the endpoint that captured it reads it.
func Acquire(root *fsq.DeliveryRoot, ticket launch.ExecutionTicket) (launch.CaptureEvidence, error) {
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
	thread := ticket.ConversationID
	if launch.CodexDaemonResume(ticket) {
		capture, err := resumedCapture(root, ticket)
		if err != nil {
			return launch.CaptureEvidence{}, errors.Join(launch.ErrIdentityNotRequested, err)
		}
		if reason := threadReason(capture, sock, thread, ticket.Cwd); reason != "" {
			return launch.CaptureEvidence{}, fmt.Errorf("%w: Buzz approvals: terminal only: %s; replan this launch", launch.ErrIdentityNotRequested, reason)
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), codex.ThreadStartTimeout)
		defer cancel()
		thread, err = codex.StartThread(ctx, sock, ticket.Cwd, "", launch.CodexDaemonVersion)
		if err != nil {
			if errors.Is(err, codex.ErrThreadNotStarted) {
				return launch.CaptureEvidence{}, errors.Join(launch.ErrIdentityNotRequested, err)
			}
			return launch.CaptureEvidence{}, err
		}
	}
	currentPID, err := codex.DaemonPID(sock)
	if err != nil || currentPID != pid {
		return launch.CaptureEvidence{}, fmt.Errorf("codex daemon changed after acquiring thread %s; preserve this attempt for operator inspection", thread)
	}
	current, err := launch.HolderOf(currentPID)
	if err != nil || current != holder {
		return launch.CaptureEvidence{}, fmt.Errorf("codex daemon identity changed after acquiring thread %s; preserve this attempt for operator inspection", thread)
	}
	return launch.ObserveCodexDaemonThread(ticket, thread, sock, holder)
}

// resumedCapture reads the capture of the conversation a resume ticket names.
func resumedCapture(root *fsq.DeliveryRoot, ticket launch.ExecutionTicket) (launch.ConversationCapture, error) {
	record, err := launch.LoadConversation(root, ticket.Handle)
	if err != nil {
		return launch.ConversationCapture{}, err
	}
	if record.State != launch.CaptureReady || record.Identity != (launch.ConversationIdentity{Provider: launch.CodexProvider, ID: ticket.ConversationID}) {
		return launch.ConversationCapture{}, fmt.Errorf("ready conversation does not match the resumed daemon thread")
	}
	return launch.ReadConversationCapture(root, record)
}

// RecordIdentity publishes routing only after the acquired thread and current
// daemon have been checked, while the ticket's generation still holds its lease.
func RecordIdentity(root *fsq.DeliveryRoot, ticket launch.ExecutionTicket, env []string) error {
	if EmbeddedEnvironment() {
		return fmt.Errorf("the current executor environment requires an embedded app-server; restore the original environment before retrying this native execution")
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
	if get("AM_ROOT") != ticket.SessionRoot || get("AM_ME") != ticket.Handle || get("AM_ROOT_ID") != ticket.SessionIdentity {
		return fmt.Errorf("codex daemon routing does not match the execution ticket")
	}
	base := ticket.SessionRoot
	if session := get("AM_SESSION"); session != "" {
		if filepath.Base(ticket.SessionRoot) != session {
			return fmt.Errorf("codex daemon routing session does not match the execution ticket")
		}
		base = filepath.Dir(ticket.SessionRoot)
	}
	baseIdentity, err := fsq.StableTreeIdentity(base)
	if err != nil || get("AM_BASE_ROOT") != base || get("AM_BASE_ROOT_ID") != baseIdentity {
		return fmt.Errorf("codex daemon routing base does not match the execution ticket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hooks, err := codex.ReadHooks(ctx, sock, ticket.Cwd)
	if err != nil || !HooksListClear(hooks, ticket.Cwd) {
		return fmt.Errorf("codex hook review is unresolved; resolve it and retry the original emitted command")
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
