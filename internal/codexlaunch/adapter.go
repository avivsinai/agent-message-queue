// Package codexlaunch connects managed launch tickets to Codex's native daemon.
package codexlaunch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	endpoint := daemonEndpoint()
	if reason == "" || endpoint.reason != "" {
		reason = endpoint.reason
	}
	if reason == "" {
		reason = admission(a.executable, endpoint, request.Cwd)
	}
	if reason == "" {
		return a.PlanFreshDaemon(request, endpoint.remote)
	}
	plan, err := a.CodexAdapter.PlanFresh(request)
	plan.TerminalOnlyReason = reason
	return plan, err
}

// PlanResume keeps each conversation on the backend that created it. A
// conversation captured by notify resumes embedded. A daemon thread resumes
// only on the daemon at its captured endpoint; any failure to do that refuses
// the resume, because no other backend is proven to read its rollout store.
// Static admission precedes every probe.
func (a *adapter) PlanResume(request launch.ResumeRequest) (launch.AgentPlan, error) {
	if err := a.ValidateResumeRequest(request); err != nil {
		return launch.AgentPlan{}, err
	}
	if request.Capture.Source == launch.CodexDaemonThreadV1 {
		remote, err := daemonResumeEndpoint(a.executable, request)
		if err != nil {
			return launch.AgentPlan{}, err
		}
		return a.PlanResumeDaemon(request, remote)
	}
	plan, err := a.CodexAdapter.PlanResume(request)
	if err != nil {
		return launch.AgentPlan{}, err
	}
	plan.TerminalOnlyReason = "this conversation runs on an embedded app-server"
	return plan, nil
}

// daemonResumeEndpoint returns the socket a daemon conversation resumes on.
// It checks the captured endpoint before any probe that runs the provider,
// then the options, the seat's admission, and the thread.
func daemonResumeEndpoint(executable string, request launch.ResumeRequest) (string, error) {
	endpoint := daemonEndpoint()
	reason := endpoint.reason
	if reason == "" {
		reason = endpointReason(request.Capture, endpoint.socket)
	}
	if reason == "" {
		reason = launch.CodexDaemonOptionsReason(request.PlanRequest)
	}
	if reason == "" {
		reason = admission(executable, endpoint, request.Cwd)
	}
	if reason == "" {
		reason = threadReason(endpoint.socket, request.Conversation.ID, request.Cwd)
	}
	return endpoint.remote, provenanceRefusal(reason)
}

// endpointReason requires the endpoint that captured the thread. A restarted
// daemon there is its successor; another endpoint is not, whatever it answers.
func endpointReason(capture launch.ConversationCapture, sock string) string {
	if sock != capture.Socket {
		return "the Codex daemon endpoint changed since this conversation was captured"
	}
	return ""
}

// threadReason requires the daemon at sock, at the verified version on the
// same session, to read the thread in its launch directory.
func threadReason(sock, thread, cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := codex.ReadThread(ctx, sock, thread, launch.CodexDaemonVersion)
	if err != nil {
		return "the Codex daemon that captured this conversation cannot prove its thread: " + err.Error()
	}
	if got != cwd {
		return "the Codex daemon that captured this conversation reads its thread in another directory"
	}
	return ""
}

func provenanceRefusal(reason string) error {
	if reason == "" {
		return nil
	}
	return fmt.Errorf("%w: %s; this conversation lives in the Codex app-server daemon that captured it: fix that and resume, or pass --allow-fresh-fallback for a new conversation",
		launch.ErrCodexConversationProvenance, reason)
}

// codexHome reads CODEX_HOME once, the way Codex 0.167 reads it (codex-rs
// utils/home-dir find_codex_home): a nonempty value taken literally and
// canonicalized, otherwise $HOME/.codex as written. canonical binds evidence
// and probes; spelled is the home Codex derives its default control socket
// from, which the seat's --remote must equal.
func codexHome() (canonical, spelled string, err error) {
	if set := os.Getenv("CODEX_HOME"); set != "" {
		absolute, err := filepath.Abs(set)
		if err != nil {
			return "", "", err
		}
		canonical, err := filepath.EvalSymlinks(absolute)
		return canonical, canonical, err
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	spelled = filepath.Join(user, ".codex")
	canonical, err = filepath.EvalSymlinks(spelled)
	if errors.Is(err, os.ErrNotExist) {
		return spelled, spelled, nil
	}
	return canonical, spelled, err
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

// endpoint is the local Codex daemon a seat attaches to. home and socket are
// canonical and bind evidence; remote is the same socket spelled as Codex
// computes it, which the seat's --remote names. reason is set when no
// verified local daemon is there.
type endpoint struct {
	home, socket, remote, reason string
}

func daemonEndpoint() endpoint {
	if EmbeddedEnvironment() {
		return endpoint{reason: "the configured executor requires an embedded app-server"}
	}
	home, spelled, err := codexHome()
	if err != nil {
		return endpoint{reason: "Codex home cannot be verified"}
	}
	socket, err := codex.ControlSocket(home)
	if errors.Is(err, os.ErrNotExist) {
		return endpoint{reason: "no Codex app-server daemon is running (start one with codex app-server daemon start)"}
	}
	if err != nil {
		return endpoint{reason: "Codex app-server daemon cannot be verified"}
	}
	remote := filepath.Join(spelled, "app-server-control", "app-server-control.sock")
	if !sameDirectory(filepath.Dir(remote), filepath.Dir(socket)) || !verifiedDaemonLink(remote) {
		return endpoint{reason: "the Codex daemon socket is not the verified local daemon link"}
	}
	return endpoint{home: home, socket: socket, remote: remote}
}

// sameDirectory reports whether a and b resolve to one directory.
func sameDirectory(a, b string) bool {
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && resolvedA == resolvedB
}

// sharedDaemonSocketDirectory is the fixed directory that holds Codex's
// private daemon sockets (codex-rs uds daemon_directory.rs).
var sharedDaemonSocketDirectory = func() (string, error) {
	tmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return "", err
	}
	return filepath.Join(tmp, fmt.Sprintf("codex-daemon-%d", os.Geteuid())), nil
}

// daemonLinkTarget is the private socket a local daemon links socket to: the
// SHA-256 of the canonical socket path under the shared directory (codex-rs
// app-server-transport unix_socket.rs protected_socket_path).
func daemonLinkTarget(socket string) (string, error) {
	shared, err := sharedDaemonSocketDirectory()
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(socket))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(filepath.Join(parent, filepath.Base(socket))))
	return filepath.Join(shared, hex.EncodeToString(sum[:])), nil
}

// verifiedDaemonLink is Codex's verified_local_daemon_socket: socket links to
// its private target in a user-owned 0700 shared directory. Only then does an
// explicit --remote attach to the local daemon.
func verifiedDaemonLink(socket string) bool {
	target, err := daemonLinkTarget(socket)
	if err != nil {
		return false
	}
	link, err := os.Readlink(socket)
	return err == nil && link == target && sharedDirectoryPrivate(filepath.Dir(target))
}

// admission checks the startup facts a seat meets on the daemon at the
// endpoint: provider version, sign-in, directory trust, daemon version, hook
// review, terminal instructions, and managed update requirements. The seat's
// own -c turns the update check off, so no user setting is needed.
func admission(executable string, endpoint endpoint, cwd string) string {
	home, sock := endpoint.home, endpoint.socket
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
	if !RequirementsAllowNoUpdateCheck(state.Requirements) {
		return "a managed Codex requirement turns the startup update check on"
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
	endpoint := daemonEndpoint()
	reason := endpoint.reason
	if reason == "" {
		reason = plannedEndpointReason(ticket, endpoint)
	}
	if reason == "" {
		reason = admission(ticket.ProviderExecutable, endpoint, ticket.Cwd)
	}
	if reason != "" {
		return launch.CaptureEvidence{}, fmt.Errorf("%w: Buzz approvals: terminal only: %s; replan this launch", launch.ErrIdentityNotRequested, reason)
	}
	sock := endpoint.socket
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
		reason := endpointReason(capture, sock)
		if reason == "" {
			reason = threadReason(sock, thread, ticket.Cwd)
		}
		if err := provenanceRefusal(reason); err != nil {
			return launch.CaptureEvidence{}, fmt.Errorf("%w: %w", launch.ErrIdentityNotRequested, err)
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

// plannedEndpointReason requires the seat's --remote to name the verified
// local daemon in this environment, so the TUI attaches where AMQ checked.
func plannedEndpointReason(ticket launch.ExecutionTicket, endpoint endpoint) string {
	if launch.CodexDaemonSocket(ticket) != endpoint.remote {
		return "the Codex daemon endpoint differs from the one this launch planned"
	}
	return ""
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

// RecordIdentity publishes routing only after the acquired thread, the current
// daemon, and the seat's startup admission have been checked again on the
// bound endpoint, while the ticket's generation still holds its lease. A
// recovered execution therefore meets the same admission as a new one without
// replacing its thread.
func RecordIdentity(root *fsq.DeliveryRoot, ticket launch.ExecutionTicket, env []string) error {
	if EmbeddedEnvironment() {
		return fmt.Errorf("the current executor environment requires an embedded app-server; restore the original environment before retrying this native execution")
	}
	endpoint := daemonEndpoint()
	if endpoint.reason != "" {
		return fmt.Errorf("%s", endpoint.reason)
	}
	if reason := plannedEndpointReason(ticket, endpoint); reason != "" {
		return fmt.Errorf("%s", reason)
	}
	home, sock := endpoint.home, endpoint.socket
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
	if reason := admission(ticket.ProviderExecutable, endpoint, ticket.Cwd); reason != "" {
		return fmt.Errorf("%s; resolve it and retry the original emitted command", reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cwd, err := codex.ReadThread(ctx, sock, ticket.ConversationID, launch.CodexDaemonVersion)
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
