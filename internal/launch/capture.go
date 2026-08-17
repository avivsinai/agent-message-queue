package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type CaptureState string

const (
	CapturePending     CaptureState = "pending"
	CaptureReady       CaptureState = "ready"
	CaptureStale       CaptureState = "stale"
	CaptureUnsupported CaptureState = "unsupported"
)

type CaptureEvidenceSource string

type CaptureReason string

const (
	CaptureReasonAdapterMintsIdentity CaptureReason = "adapter_mints_identity"
	CaptureReasonEvidenceMissing      CaptureReason = "evidence_missing"
	CaptureReasonEvidenceAmbiguous    CaptureReason = "evidence_ambiguous"
	CaptureReasonProviderMismatch     CaptureReason = "provider_mismatch"
	CaptureReasonProviderVersion      CaptureReason = "provider_version_mismatch"
	CaptureReasonLaunchNonceMismatch  CaptureReason = "launch_nonce_mismatch"
	CaptureReasonEvidenceSource       CaptureReason = "evidence_source_unsupported"
	CaptureReasonEvidenceUnverified   CaptureReason = "evidence_unverified"
	CaptureReasonInvalidIdentity      CaptureReason = "invalid_conversation_identity"
	CaptureReasonConversationActive   CaptureReason = "conversation_active_elsewhere"
)

type CaptureRequest struct {
	LaunchNonce             string
	ExpectedProviderVersion string
	Final                   bool
	Evidence                []CaptureEvidence
}

// CaptureEvidence is an observer-correlated envelope around provider-owned
// evidence. Source names the provider protocol event; LaunchNonce binds the
// observation to the launch generation held by the caller's session lease.
type CaptureEvidence struct {
	source          CaptureEvidenceSource
	provider        string
	providerVersion string
	launchNonce     string
	handle          string
	conversationID  string
	activeElsewhere bool
	verified        bool
	observedAt      time.Time
	payload         []byte
}

type CaptureResult struct {
	State    CaptureState
	Identity ConversationIdentity
	Degraded bool
	Reason   CaptureReason
}

type cursorCreateChatPayload struct {
	Source          CaptureEvidenceSource `json:"source"`
	Provider        string                `json:"provider"`
	ProviderVersion string                `json:"provider_version"`
	LaunchNonce     string                `json:"launch_nonce"`
	Handle          string                `json:"handle"`
	ConversationID  string                `json:"conversation_id"`
	Stdout          string                `json:"stdout"`
}

func (result CaptureResult) CanPersist() bool {
	return result.State == CaptureReady && !result.Degraded && result.Identity.Provider != "" && result.Identity.ID != ""
}

// ParseCodexThreadStartedEvidence verifies the provider event shape and binds
// the observation to the launch generation of the channel that received it.
// It does not scan the Codex session store or accept newest-file evidence.
func ParseCodexThreadStartedEvidence(raw []byte, launchNonce string, activeElsewhere bool) (CaptureEvidence, error) {
	if !validUUID(launchNonce) {
		return CaptureEvidence{}, fmt.Errorf("launch nonce must be a UUID")
	}
	var event struct {
		Method string `json:"method"`
		Params struct {
			Thread struct {
				ID         string `json:"id"`
				CLIVersion string `json:"cliVersion"`
			} `json:"thread"`
		} `json:"params"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&event); err != nil {
		return CaptureEvidence{}, fmt.Errorf("decode Codex capture evidence: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return CaptureEvidence{}, fmt.Errorf("decode Codex capture evidence: trailing JSON value")
	}
	if event.Method != "thread/started" {
		return CaptureEvidence{}, fmt.Errorf("codex capture evidence method is %q, want thread/started", event.Method)
	}
	if !validUUIDv7(event.Params.Thread.ID) {
		return CaptureEvidence{}, fmt.Errorf("codex thread/started identity must be a UUIDv7")
	}
	if strings.TrimSpace(event.Params.Thread.CLIVersion) == "" {
		return CaptureEvidence{}, fmt.Errorf("codex thread/started evidence has no CLI version")
	}
	return CaptureEvidence{
		source: CodexThreadStartedV2, provider: CodexProvider,
		providerVersion: event.Params.Thread.CLIVersion, launchNonce: launchNonce,
		conversationID: event.Params.Thread.ID, activeElsewhere: activeElsewhere,
		verified: true, observedAt: time.Now().UTC(), payload: bytes.Clone(raw),
	}, nil
}

// ParseCursorCreateChatEvidence validates the exact stdout returned by the
// pinned cursor-agent create-chat command. The executing channel supplies the
// nonce, handle, and version bindings; provider output is never authority for
// those values.
func ParseCursorCreateChatEvidence(raw []byte, launchNonce, handle, providerVersion string) (CaptureEvidence, error) {
	if !validUUID(launchNonce) {
		return CaptureEvidence{}, fmt.Errorf("launch nonce must be a UUID")
	}
	if err := fsq.ValidateHandle(handle); err != nil {
		return CaptureEvidence{}, fmt.Errorf("invalid capture handle: %w", err)
	}
	if providerVersion != cursorCaptureVersion {
		return CaptureEvidence{}, fmt.Errorf("cursor capture version %q is unsupported", providerVersion)
	}
	stdout := string(raw)
	stdout = strings.TrimSuffix(stdout, "\n")
	if stdout == "" || strings.ContainsAny(stdout, "\r\n") || stdout != strings.ToLower(stdout) || !validUUID(stdout) {
		return CaptureEvidence{}, fmt.Errorf("cursor create-chat output must be one canonical UUID")
	}
	payload, err := json.Marshal(cursorCreateChatPayload{
		Source: CursorCreateChatV1, Provider: CursorProvider, ProviderVersion: providerVersion,
		LaunchNonce: launchNonce, Handle: handle, ConversationID: stdout, Stdout: string(raw),
	})
	if err != nil {
		return CaptureEvidence{}, err
	}
	return CaptureEvidence{
		source: CursorCreateChatV1, provider: CursorProvider, providerVersion: providerVersion,
		launchNonce: launchNonce, handle: handle, conversationID: stdout,
		verified: true, observedAt: time.Now().UTC(), payload: payload,
	}, nil
}

func decodeCursorCreateChatPayload(raw []byte) (cursorCreateChatPayload, error) {
	var payload cursorCreateChatPayload
	if err := decodeStrict(raw, &payload); err != nil {
		return payload, fmt.Errorf("decode cursor create-chat evidence: %w", err)
	}
	if payload.Source != CursorCreateChatV1 || payload.Provider != CursorProvider ||
		payload.ProviderVersion != cursorCaptureVersion || !validUUID(payload.LaunchNonce) {
		return payload, fmt.Errorf("cursor create-chat evidence metadata is invalid")
	}
	if err := fsq.ValidateHandle(payload.Handle); err != nil {
		return payload, fmt.Errorf("cursor create-chat evidence handle: %w", err)
	}
	parsed, err := ParseCursorCreateChatEvidence([]byte(payload.Stdout), payload.LaunchNonce, payload.Handle, payload.ProviderVersion)
	if err != nil || parsed.conversationID != payload.ConversationID {
		return payload, fmt.Errorf("cursor create-chat evidence identity is invalid")
	}
	return payload, nil
}

func captureCodexIdentity(request CaptureRequest) CaptureResult {
	if !validUUID(request.LaunchNonce) {
		return degradedCapture(CaptureStale, CaptureReasonLaunchNonceMismatch)
	}
	if strings.TrimSpace(request.ExpectedProviderVersion) == "" {
		return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonProviderVersion}
	}
	if len(request.Evidence) == 0 {
		if !request.Final {
			return CaptureResult{State: CapturePending}
		}
		return degradedCapture(CaptureStale, CaptureReasonEvidenceMissing)
	}
	identities := make(map[string]struct{}, len(request.Evidence))
	for _, evidence := range request.Evidence {
		if !evidence.verified {
			return degradedCapture(CaptureStale, CaptureReasonEvidenceUnverified)
		}
		if evidence.source != CodexThreadStartedV2 {
			return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonEvidenceSource}
		}
		if evidence.provider != CodexProvider {
			return degradedCapture(CaptureStale, CaptureReasonProviderMismatch)
		}
		if evidence.providerVersion != request.ExpectedProviderVersion {
			return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonProviderVersion}
		}
		if evidence.launchNonce != request.LaunchNonce {
			return degradedCapture(CaptureStale, CaptureReasonLaunchNonceMismatch)
		}
		if evidence.activeElsewhere {
			return degradedCapture(CaptureStale, CaptureReasonConversationActive)
		}
		if !validUUIDv7(evidence.conversationID) {
			return degradedCapture(CaptureStale, CaptureReasonInvalidIdentity)
		}
		identities[evidence.conversationID] = struct{}{}
	}
	if len(identities) != 1 {
		return degradedCapture(CaptureStale, CaptureReasonEvidenceAmbiguous)
	}
	for id := range identities {
		return CaptureResult{
			State:    CaptureReady,
			Identity: ConversationIdentity{Provider: CodexProvider, ID: id},
		}
	}
	return degradedCapture(CaptureStale, CaptureReasonEvidenceMissing)
}

func captureCursorIdentity(request CaptureRequest) CaptureResult {
	if !validUUID(request.LaunchNonce) {
		return degradedCapture(CaptureStale, CaptureReasonLaunchNonceMismatch)
	}
	if request.ExpectedProviderVersion != cursorCaptureVersion {
		return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonProviderVersion}
	}
	if len(request.Evidence) == 0 {
		if !request.Final {
			return CaptureResult{State: CapturePending}
		}
		return degradedCapture(CaptureStale, CaptureReasonEvidenceMissing)
	}
	identities := make(map[string]struct{}, len(request.Evidence))
	for _, evidence := range request.Evidence {
		switch {
		case !evidence.verified:
			return degradedCapture(CaptureStale, CaptureReasonEvidenceUnverified)
		case evidence.source != CursorCreateChatV1:
			return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonEvidenceSource}
		case evidence.provider != CursorProvider:
			return degradedCapture(CaptureStale, CaptureReasonProviderMismatch)
		case evidence.providerVersion != request.ExpectedProviderVersion:
			return CaptureResult{State: CaptureUnsupported, Reason: CaptureReasonProviderVersion}
		case evidence.launchNonce != request.LaunchNonce:
			return degradedCapture(CaptureStale, CaptureReasonLaunchNonceMismatch)
		case !validUUID(evidence.conversationID):
			return degradedCapture(CaptureStale, CaptureReasonInvalidIdentity)
		}
		identities[evidence.conversationID] = struct{}{}
	}
	if len(identities) != 1 {
		return degradedCapture(CaptureStale, CaptureReasonEvidenceAmbiguous)
	}
	for id := range identities {
		return CaptureResult{State: CaptureReady, Identity: ConversationIdentity{Provider: CursorProvider, ID: id}}
	}
	return degradedCapture(CaptureStale, CaptureReasonEvidenceMissing)
}

func degradedCapture(state CaptureState, reason CaptureReason) CaptureResult {
	return CaptureResult{State: state, Degraded: true, Reason: reason}
}
