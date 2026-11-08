package launch

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const CodexDaemonVersion = "0.167.0"

const CodexDaemonThreadV1 CaptureEvidenceSource = "codex_daemon_thread_v1"

// PlanFreshDaemon resumes a thread acquired under the execution ticket's
// lease. Eligibility and startup checks must succeed again at acquisition.
func (adapter *CodexAdapter) PlanFreshDaemon(request PlanRequest) (AgentPlan, error) {
	executable, err := validatePlanRequest(request, adapter.executable, CodexProvider, codexEnvRules(), codexArgRules(), codexBypassArgs())
	if err != nil {
		return AgentPlan{}, err
	}
	if len(request.CommittedArgs) != 0 || len(request.BypassArgs) != 0 || len(request.EnvOverlay) != 0 || request.Wrapper != nil {
		return AgentPlan{}, fmt.Errorf("codex daemon launch options require verified resume equivalence")
	}
	plan := AgentPlan{
		Handle: request.Handle, Argv: []string{executable, "resume", "-C", request.Cwd, preSpawnConversationPlaceholder}, Cwd: request.Cwd,
		AdapterMode: AdapterModeCapture, ResumePolicy: request.ResumePolicy, LaunchNonce: request.LaunchNonce,
		PreSpawnAcquire: true, DynamicArgv: []DynamicArg{{Index: 4, Kind: DynamicArgConversationID}},
	}
	if err := appendInitialInput(&plan, request.InitialInput); err != nil {
		return AgentPlan{}, err
	}
	return plan, ValidateAdapterPlan(adapter, plan)
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
		payload.ProviderVersion != CodexDaemonVersion || !validUUID(payload.LaunchNonce) || !validUUIDv7(payload.ConversationID) ||
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

func validateCodexDaemonTargetArgv(ticket ExecutionTicket) error {
	if ticket.ProviderVersion != CodexDaemonVersion || len(ticket.TargetEnv) != 0 || ticket.Wrapper != nil ||
		len(ticket.TargetArgv) < 5 ||
		ticket.TargetArgv[1] != "resume" || ticket.TargetArgv[2] != "-C" || ticket.TargetArgv[3] != ticket.Cwd || ticket.TargetArgv[4] != preSpawnConversationPlaceholder ||
		len(ticket.DynamicArgv) != 1 || ticket.DynamicArgv[0] != (DynamicArg{Index: 4, Kind: DynamicArgConversationID}) {
		return fmt.Errorf("codex daemon execution template is invalid")
	}
	if len(ticket.TargetArgv) != 5 && (len(ticket.TargetArgv) != 6 || ticket.InitialInput == nil || ticket.InitialInput.ArgvIndex != 5) {
		return fmt.Errorf("codex daemon execution options require verified resume equivalence")
	}
	return nil
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
