package launch

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

const CodexNotifyPayloadLimit = 1 << 20

type codexNotifyEvent struct {
	Type                 string   `json:"type"`
	ThreadID             string   `json:"thread-id"`
	TurnID               string   `json:"turn-id"`
	Cwd                  string   `json:"cwd"`
	Client               *string  `json:"client,omitempty"`
	InputMessages        []string `json:"input-messages"`
	LastAssistantMessage *string  `json:"last-assistant-message,omitempty"`
}

type codexNotifyPayload struct {
	Source          CaptureEvidenceSource `json:"source"`
	Provider        string                `json:"provider"`
	ProviderVersion string                `json:"provider_version"`
	LaunchNonce     string                `json:"launch_nonce"`
	Handle          string                `json:"handle"`
	ConversationID  string                `json:"conversation_id"`
	Cwd             string                `json:"cwd"`
	Notification    string                `json:"notification"`
}

func (result CaptureResult) CanPersist() bool {
	return result.State == CaptureReady && !result.Degraded && result.Identity.Provider != "" && result.Identity.ID != ""
}

// ParseCodexNotifyEvidence verifies the Codex legacy notify wire shape for a
// supported provider version and binds it to the exact managed launch
// generation. It does not scan the Codex session store or accept newest-file
// evidence.
func ParseCodexNotifyEvidence(raw []byte, launchNonce, handle, expectedVersion, expectedCwd string) (CaptureEvidence, error) {
	if len(raw) == 0 || len(raw) > CodexNotifyPayloadLimit {
		return CaptureEvidence{}, fmt.Errorf("codex notify payload size is invalid")
	}
	if !validUUID(launchNonce) {
		return CaptureEvidence{}, fmt.Errorf("launch nonce must be a UUID")
	}
	if err := fsq.ValidateHandle(handle); err != nil {
		return CaptureEvidence{}, fmt.Errorf("invalid capture handle: %w", err)
	}
	if !codexCaptureVersionSupported(expectedVersion) {
		return CaptureEvidence{}, fmt.Errorf("codex capture version %q is unsupported", expectedVersion)
	}
	var event codexNotifyEvent
	if err := decodeStrict(raw, &event); err != nil {
		return CaptureEvidence{}, fmt.Errorf("decode Codex notify evidence: %w", err)
	}
	if event.Type != "agent-turn-complete" {
		return CaptureEvidence{}, fmt.Errorf("codex notify type is %q, want agent-turn-complete", event.Type)
	}
	if !validUUIDv7(event.ThreadID) {
		return CaptureEvidence{}, fmt.Errorf("codex notify thread identity must be a UUIDv7")
	}
	if strings.TrimSpace(event.TurnID) == "" {
		return CaptureEvidence{}, fmt.Errorf("codex notify turn identity is empty")
	}
	if event.Cwd != expectedCwd {
		return CaptureEvidence{}, fmt.Errorf("codex notify cwd does not match execution ticket")
	}
	if event.InputMessages == nil {
		return CaptureEvidence{}, fmt.Errorf("codex notify input-messages is missing")
	}
	payload, err := json.Marshal(codexNotifyPayload{
		Source: CodexNotifyV1, Provider: CodexProvider, ProviderVersion: expectedVersion,
		LaunchNonce: launchNonce, Handle: handle, ConversationID: event.ThreadID,
		Cwd: expectedCwd, Notification: string(raw),
	})
	if err != nil {
		return CaptureEvidence{}, err
	}
	return CaptureEvidence{
		source: CodexNotifyV1, provider: CodexProvider,
		providerVersion: expectedVersion, launchNonce: launchNonce, handle: handle,
		conversationID: event.ThreadID,
		verified:       true, observedAt: time.Now().UTC(), payload: payload,
	}, nil
}

func decodeCodexNotifyPayload(raw []byte) (codexNotifyPayload, error) {
	var payload codexNotifyPayload
	if err := decodeStrict(raw, &payload); err != nil {
		return payload, fmt.Errorf("decode codex notify evidence: %w", err)
	}
	if payload.Source != CodexNotifyV1 || payload.Provider != CodexProvider ||
		!codexCaptureVersionSupported(payload.ProviderVersion) || !validUUID(payload.LaunchNonce) {
		return payload, fmt.Errorf("codex notify evidence metadata is invalid")
	}
	if err := fsq.ValidateHandle(payload.Handle); err != nil {
		return payload, fmt.Errorf("codex notify evidence handle: %w", err)
	}
	parsed, err := ParseCodexNotifyEvidence([]byte(payload.Notification), payload.LaunchNonce, payload.Handle, payload.ProviderVersion, payload.Cwd)
	if err != nil || parsed.conversationID != payload.ConversationID {
		return payload, fmt.Errorf("codex notify evidence identity is invalid")
	}
	return payload, nil
}

// codexRolloutMetaLimit bounds the first rollout line, which carries the
// session's base instructions.
const codexRolloutMetaLimit = 1 << 20

// codexTopLevelThread reports whether threadID is the top-level interactive
// thread of a Codex session in cwd. Codex also sends notify for side threads
// (title generation writes no rollout) and subagent threads (their
// session_meta source is an object), so arrival order never identifies the
// seat's conversation. Older releases may omit thread_source. The check reads
// only the first line of the rollout named by the exact thread id, in the day
// directories around the UUIDv7 creation time; it never selects a newest
// file. A missing rollout is not proof and reports false.
func codexTopLevelThread(codexHome, threadID, cwd string) (bool, error) {
	if !validUUIDv7(threadID) {
		return false, nil
	}
	millis, err := strconv.ParseInt(strings.ReplaceAll(threadID[:13], "-", ""), 16, 64)
	if err != nil {
		return false, nil
	}
	created := time.UnixMilli(millis).Local()
	var matches []string
	for _, offset := range []int{-1, 0, 1} {
		dir := filepath.Join(codexHome, "sessions", created.AddDate(0, 0, offset).Format("2006/01/02"))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read codex sessions: %w", err)
		}
		for _, entry := range entries {
			if name := entry.Name(); strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, "-"+threadID+".jsonl") {
				matches = append(matches, filepath.Join(dir, name))
			}
		}
	}
	if len(matches) != 1 {
		return false, nil
	}
	file, err := os.Open(matches[0])
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open codex rollout: %w", err)
	}
	defer func() { _ = file.Close() }()
	line, err := bufio.NewReader(io.LimitReader(file, codexRolloutMetaLimit)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read codex rollout: %w", err)
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID           string          `json:"id"`
			Cwd          string          `json:"cwd"`
			Source       json.RawMessage `json:"source"`
			ThreadSource string          `json:"thread_source"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil {
		return false, nil
	}
	return meta.Type == "session_meta" && meta.Payload.ID == threadID && meta.Payload.Cwd == cwd &&
		string(meta.Payload.Source) == `"cli"` && (meta.Payload.ThreadSource == "" || meta.Payload.ThreadSource == "user"), nil
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
		if evidence.source != CodexNotifyV1 {
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
