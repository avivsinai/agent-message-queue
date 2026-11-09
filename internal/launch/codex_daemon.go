package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// CodexDaemonVersion is the one Codex release a new native execution runs on.
const CodexDaemonVersion = "0.167.0"

// codexDaemonMinimumVersion is the oldest release whose native tickets and
// evidence remain readable, so inspection and cleanup survive an upgrade.
const codexDaemonMinimumVersion = "0.167.0"

// ErrCodexConversationProvenance marks a daemon-captured conversation whose
// capturing endpoint does not prove its thread. Resume refuses it rather than
// move the conversation to another backend.
var ErrCodexConversationProvenance = errors.New("codex conversation provenance is not established")

const CodexDaemonThreadV1 CaptureEvidenceSource = "codex_daemon_thread_v1"

// PlanFreshDaemon resumes a thread acquired under the execution ticket's
// lease on the daemon at socket. Eligibility and startup checks must succeed
// again at acquisition.
func (adapter *CodexAdapter) PlanFreshDaemon(request PlanRequest, socket string) (AgentPlan, error) {
	return adapter.planDaemon(request, "", socket)
}

// PlanResumeDaemon resumes a daemon-acquired thread on the daemon. The thread
// stays in argv; acquisition verifies it on the live daemon instead of
// starting one.
func (adapter *CodexAdapter) PlanResumeDaemon(request ResumeRequest, socket string) (AgentPlan, error) {
	if err := adapter.ValidateResumeRequest(request); err != nil {
		return AgentPlan{}, err
	}
	if request.Capture.Source != CodexDaemonThreadV1 {
		return AgentPlan{}, fmt.Errorf("codex daemon resume requires a daemon-acquired conversation")
	}
	return adapter.planDaemon(request.PlanRequest, request.Conversation.ID, socket)
}

// codexDaemonTemplateStart is the index of the first committed argument in
// the daemon template: codex --remote unix://<socket> -c <no update prompt>
// resume -C <cwd>. An explicit --remote equal to Codex's own control socket
// attaches the TUI to that local daemon with no embedded fallback, and the -c
// suppresses the startup update prompt without a user configuration change
// (codex-rs tui/src/lib.rs app_server_target_for_launch and
// startup_orchestration.rs at rust-v0.167.0).
const codexDaemonTemplateStart = 8

// codexDaemonLegacyTemplateStart is the template earlier releases planned,
// codex resume -C <cwd>. Its tickets stay readable for inspection and cleanup
// and never run again.
const codexDaemonLegacyTemplateStart = 4

// planDaemon builds the one daemon template on the daemon at socket, spelled
// as Codex computes its control socket. An empty thread leaves the
// conversation slot for acquisition to fill.
func (adapter *CodexAdapter) planDaemon(request PlanRequest, thread, socket string) (AgentPlan, error) {
	executable, err := validatePlanRequest(request, adapter.executable, CodexProvider, codexEnvRules(), codexArgRules(), codexBypassArgs())
	if err != nil {
		return AgentPlan{}, err
	}
	if reason := CodexDaemonOptionsReason(request); reason != "" {
		return AgentPlan{}, fmt.Errorf("%s", reason)
	}
	slot := preSpawnConversationPlaceholder
	if thread != "" {
		slot = thread
	}
	if !filepath.IsAbs(socket) {
		return AgentPlan{}, fmt.Errorf("codex daemon socket must be absolute")
	}
	argv := append([]string{executable, "--remote", "unix://" + socket, "-c", CodexNoUpdatePromptOverride, "resume", "-C", request.Cwd}, request.CommittedArgs...)
	identityIndex := len(argv)
	argv = append(argv, slot)
	plan := AgentPlan{
		Handle: request.Handle, Argv: argv, Cwd: request.Cwd,
		AdapterMode: AdapterModeCapture, ResumePolicy: request.ResumePolicy, LaunchNonce: request.LaunchNonce, ConversationID: thread,
		PreSpawnAcquire: true, DynamicArgv: []DynamicArg{{Index: identityIndex, Kind: DynamicArgConversationID}},
	}
	if thread == "" {
		if err := appendInitialInput(&plan, request.InitialInput); err != nil {
			return AgentPlan{}, err
		}
	}
	return plan, ValidateAdapterPlan(adapter, plan)
}

// CodexDaemonResume reports a daemon ticket whose argv names the existing
// thread it resumes. Such a ticket holds its thread from creation and never
// asks the daemon to create one.
func CodexDaemonResume(ticket ExecutionTicket) bool {
	if ticket.Provider != CodexProvider || !ticket.PreSpawnAcquire || len(ticket.DynamicArgv) != 1 {
		return false
	}
	index := ticket.DynamicArgv[0].Index
	return index > 0 && index < len(ticket.TargetArgv) && ticket.TargetArgv[index] != preSpawnConversationPlaceholder
}

type codexDaemonPayload struct {
	Source          CaptureEvidenceSource `json:"source"`
	Provider        string                `json:"provider"`
	ProviderVersion string                `json:"provider_version"`
	LaunchNonce     string                `json:"launch_nonce"`
	Handle          string                `json:"handle"`
	ConversationID  string                `json:"conversation_id"`
	Cwd             string                `json:"cwd"`
	SessionIdentity string                `json:"session_identity"`
	Socket          string                `json:"socket"`
	Daemon          HolderIdentity        `json:"daemon"`
}

// ObserveCodexDaemonThread binds a protocol-acquired thread to the ticket
// whose live lease owns the observation. It is not a notify event.
func ObserveCodexDaemonThread(ticket ExecutionTicket, thread, socket string, daemon HolderIdentity) (CaptureEvidence, error) {
	payload := codexDaemonPayload{
		Source: CodexDaemonThreadV1, Provider: CodexProvider, ProviderVersion: ticket.ProviderVersion,
		LaunchNonce: ticket.LaunchNonce, Handle: ticket.Handle, ConversationID: thread,
		Cwd: ticket.Cwd, SessionIdentity: ticket.SessionIdentity, Socket: socket, Daemon: daemon,
	}
	if !ticket.PreSpawnAcquire || ticket.Provider != CodexProvider {
		return CaptureEvidence{}, fmt.Errorf("codex daemon observation requires a pre-spawn Codex ticket")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return CaptureEvidence{}, err
	}
	if _, err := decodeCodexDaemonPayload(raw); err != nil {
		return CaptureEvidence{}, err
	}
	return CaptureEvidence{
		source: CodexDaemonThreadV1, provider: CodexProvider, providerVersion: ticket.ProviderVersion,
		launchNonce: ticket.LaunchNonce, handle: ticket.Handle, conversationID: thread,
		verified: true, observedAt: time.Now().UTC(), payload: raw,
	}, nil
}

func decodeCodexDaemonPayload(raw []byte) (codexDaemonPayload, error) {
	var payload codexDaemonPayload
	if err := decodeStrict(raw, &payload); err != nil {
		return payload, fmt.Errorf("decode Codex daemon evidence: %w", err)
	}
	if payload.Source != CodexDaemonThreadV1 || payload.Provider != CodexProvider ||
		!codexDaemonVersionReadable(payload.ProviderVersion) || !validUUID(payload.LaunchNonce) || !validUUIDv7(payload.ConversationID) ||
		!filepath.IsAbs(payload.Cwd) || !filepath.IsAbs(payload.Socket) || strings.TrimSpace(payload.SessionIdentity) == "" {
		return payload, fmt.Errorf("codex daemon evidence binding is invalid")
	}
	if err := fsq.ValidateHandle(payload.Handle); err != nil {
		return payload, err
	}
	if payload.Daemon.PID <= 0 || payload.Daemon.ProcessStart == "" || payload.Daemon.BootID == "" {
		return payload, fmt.Errorf("codex daemon evidence process is incomplete")
	}
	return payload, nil
}

// CodexDaemonSocket returns the control socket a daemon ticket's argv names
// with --remote, or "" for a ticket an earlier release planned without one.
func CodexDaemonSocket(ticket ExecutionTicket) string {
	argv := ticket.TargetArgv
	if len(argv) < codexDaemonTemplateStart || argv[1] != "--remote" {
		return ""
	}
	socket, ok := strings.CutPrefix(argv[2], "unix://")
	if !ok || !filepath.IsAbs(socket) {
		return ""
	}
	return socket
}

func validateCodexDaemonTargetArgv(ticket ExecutionTicket) error {
	argv := ticket.TargetArgv
	start := codexDaemonLegacyTemplateStart
	if CodexDaemonSocket(ticket) != "" {
		start = codexDaemonTemplateStart
		if argv[3] != "-c" || argv[4] != CodexNoUpdatePromptOverride {
			return fmt.Errorf("codex daemon execution template is invalid")
		}
	}
	if !codexDaemonVersionReadable(ticket.ProviderVersion) || len(ticket.TargetEnv) != 0 || ticket.Wrapper != nil ||
		len(argv) < start+1 ||
		argv[start-3] != "resume" || argv[start-2] != "-C" || argv[start-1] != ticket.Cwd ||
		len(ticket.DynamicArgv) != 1 || ticket.DynamicArgv[0].Kind != DynamicArgConversationID {
		return fmt.Errorf("codex daemon execution template is invalid")
	}
	identityIndex := ticket.DynamicArgv[0].Index
	if identityIndex < start || identityIndex >= len(argv) {
		return fmt.Errorf("codex daemon conversation slot is invalid")
	}
	if slot := ticket.TargetArgv[identityIndex]; slot != preSpawnConversationPlaceholder && (!validUUIDv7(slot) || slot != ticket.ConversationID) {
		return fmt.Errorf("codex daemon conversation slot is invalid")
	}
	if reason := codexDaemonArgumentsReason(argv[start:identityIndex]); reason != "" {
		return fmt.Errorf("%s", reason)
	}
	if len(ticket.TargetArgv) != identityIndex+1 && (len(ticket.TargetArgv) != identityIndex+2 || ticket.InitialInput == nil || ticket.InitialInput.ArgvIndex != identityIndex+1) {
		return fmt.Errorf("codex daemon execution options require verified resume equivalence")
	}
	return nil
}

// CodexDaemonOptionsReason admits only invocation settings that Codex 0.167
// forwards to a local-daemon TUI resume. A caller's configuration or search
// override is not shown to take effect there, so it keeps the embedded path.
func CodexDaemonOptionsReason(request PlanRequest) string {
	if request.Wrapper != nil || len(request.EnvOverlay) != 0 || len(request.BypassArgs) != 0 {
		return "wrapper, environment, or bypass options require verified TUI resume equivalence"
	}
	return codexDaemonArgumentsReason(request.CommittedArgs)
}

func codexDaemonArgumentsReason(args []string) string {
	if err := validateCommittedArgs(args, codexArgRules()); err != nil {
		return "launch arguments do not match the verified Codex resume grammar"
	}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if seen[flag] {
			return "duplicate launch options require verified TUI resume equivalence"
		}
		seen[flag] = true
		switch flag {
		case "--no-alt-screen":
		case "--model":
			i++
		case "--sandbox":
			i++
			if !oneOf("read-only", "workspace-write")(args[i]) {
				return "sandbox option requires verified TUI resume equivalence"
			}
		case "--ask-for-approval":
			i++
			if args[i] != "on-request" {
				return "approval option requires verified TUI resume equivalence"
			}
		case "-c", "--search":
			return "command-line configuration or search overrides require verified TUI resume equivalence"
		default:
			return "launch option requires verified TUI resume equivalence"
		}
	}
	return ""
}

// codexDaemonVersionReadable admits a strict release at or above
// codexDaemonMinimumVersion. Only CodexDaemonVersion runs a new execution.
func codexDaemonVersionReadable(version string) bool {
	return releaseAtLeast(version, codexDaemonMinimumVersion)
}

func validateCodexDaemonExecutable(ticket ExecutionTicket) error {
	executable, identity, err := canonicalFile(ticket.TargetArgv[0])
	if err != nil || executable != ticket.ProviderExecutable || identity != ticket.ProviderExecutableIdentity {
		return fmt.Errorf("codex daemon executable identity changed")
	}
	return nil
}

func capturePreSpawnIdentity(provider string, request CaptureRequest) CaptureResult {
	if provider == CodexProvider {
		return captureCodexIdentity(request)
	}
	return captureCursorIdentity(request)
}

func validateCodexDaemonExecutionEvidence(root *fsq.DeliveryRoot, ticket ExecutionTicket) error {
	_, err := codexDaemonExecutionPayload(root, ticket)
	return err
}

func codexDaemonExecutionPayload(root *fsq.DeliveryRoot, ticket ExecutionTicket) (codexDaemonPayload, error) {
	if len(ticket.EvidenceRefs) != 1 {
		return codexDaemonPayload{}, fmt.Errorf("codex daemon execution requires one evidence ref")
	}
	record, _, err := ReadEvidence(root, ticket.EvidenceRefs[0])
	if err != nil {
		return codexDaemonPayload{}, err
	}
	payload, err := decodeCodexDaemonPayload(record.Payload)
	if err != nil {
		return payload, err
	}
	if record.Kind != EvidenceProviderCapture || record.Handle != ticket.Handle || payload.Handle != ticket.Handle ||
		payload.LaunchNonce != ticket.LaunchNonce || payload.ProviderVersion != ticket.ProviderVersion ||
		payload.ConversationID != ticket.ConversationID || payload.Cwd != ticket.Cwd || payload.SessionIdentity != ticket.SessionIdentity ||
		!reflect.DeepEqual(record.CallerContext, ticket.CallerContext) {
		return payload, fmt.Errorf("codex daemon execution evidence binding mismatch")
	}
	return payload, nil
}

// ValidateCodexDaemonEndpoint requires the same endpoint and live daemon that
// acquired this ticket. A changed daemon requires explicit recovery.
func ValidateCodexDaemonEndpoint(root *fsq.DeliveryRoot, ticket ExecutionTicket, socket string, daemon HolderIdentity) error {
	payload, err := codexDaemonExecutionPayload(root, ticket)
	if err != nil {
		return err
	}
	if payload.Socket != socket || payload.Daemon != daemon {
		return fmt.Errorf("codex daemon endpoint changed since thread acquisition")
	}
	return nil
}

func validateCodexConversationCapture(root *fsq.DeliveryRoot, raw []byte, record ConversationRecord) error {
	var envelope struct {
		Source CaptureEvidenceSource `json:"source"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	var handle, nonce, provider, version, thread string
	if envelope.Source == CodexDaemonThreadV1 {
		payload, err := decodeCodexDaemonPayload(raw)
		if err != nil {
			return err
		}
		if err := root.VerifyBase(); err != nil {
			return err
		}
		_, identity, err := canonicalDirectory(root.Base())
		if err != nil || payload.SessionIdentity != identity {
			return fmt.Errorf("codex conversation evidence belongs to another physical session root")
		}
		handle, nonce, provider, version, thread = payload.Handle, payload.LaunchNonce, payload.Provider, payload.ProviderVersion, payload.ConversationID
	} else {
		payload, err := decodeCodexNotifyPayload(raw)
		if err != nil {
			return err
		}
		handle, nonce, provider, version, thread = payload.Handle, payload.LaunchNonce, payload.Provider, payload.ProviderVersion, payload.ConversationID
	}
	if handle != record.Handle || nonce != record.LaunchNonce || provider != record.Identity.Provider || version != record.ProviderVersion || thread != record.Identity.ID {
		return fmt.Errorf("codex conversation evidence binding mismatch")
	}
	return nil
}
