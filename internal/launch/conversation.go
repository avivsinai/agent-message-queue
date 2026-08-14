package launch

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	ConversationVersion = 1
	conversationDir     = "meta/launch/conversations"
)

// ConversationRecord is provider-qualified runtime state for one
// (session, handle). It carries no execution authority.
type ConversationRecord struct {
	Version           int                            `json:"version"`
	Handle            string                         `json:"handle"`
	State             CaptureState                   `json:"state"`
	Identity          ConversationIdentity           `json:"identity,omitempty"`
	ProviderVersion   string                         `json:"provider_version,omitempty"`
	LaunchNonce       string                         `json:"launch_nonce"`
	ExecutionEvidence *ConversationExecutionEvidence `json:"execution_evidence,omitempty"`
	Reason            CaptureReason                  `json:"reason,omitempty"`
}

// ConversationExecutionEvidence records the managed backend result that
// proves a planned agent process started. It does not grant execution
// authority; it prevents a minted identity from becoming resumable from plan
// output alone.
type ConversationExecutionEvidence struct {
	Backend        string  `json:"backend"`
	Profile        string  `json:"profile"`
	Outcome        Outcome `json:"outcome"`
	LaunchNonce    string  `json:"launch_nonce"`
	ConversationID string  `json:"conversation_id,omitempty"`
}

func (record ConversationRecord) Validate() error {
	if record.Version != ConversationVersion {
		return fmt.Errorf("unsupported conversation record version %d", record.Version)
	}
	if err := fsq.ValidateHandle(record.Handle); err != nil {
		return fmt.Errorf("invalid conversation handle: %w", err)
	}
	if !validUUID(record.LaunchNonce) {
		return fmt.Errorf("conversation launch nonce must be a UUID")
	}
	if record.ExecutionEvidence != nil {
		if strings.TrimSpace(record.ExecutionEvidence.Backend) == "" || strings.TrimSpace(record.ExecutionEvidence.Profile) == "" {
			return fmt.Errorf("conversation execution evidence is incomplete")
		}
		if record.ExecutionEvidence.Outcome != OutcomeCreated {
			return fmt.Errorf("conversation execution evidence outcome must be created")
		}
		if record.ExecutionEvidence.LaunchNonce != record.LaunchNonce {
			return fmt.Errorf("conversation execution evidence launch nonce mismatch")
		}
	}
	switch record.State {
	case CapturePending:
		if record.Identity.Provider != "" || record.Identity.ID != "" {
			return fmt.Errorf("pending conversation must not contain an identity")
		}
		if record.ExecutionEvidence != nil {
			return fmt.Errorf("pending conversation must not contain execution evidence")
		}
	case CaptureReady:
		if strings.TrimSpace(record.Identity.Provider) == "" || !validUUID(record.Identity.ID) {
			return fmt.Errorf("ready conversation requires a provider-qualified UUID")
		}
		if record.ExecutionEvidence == nil {
			return fmt.Errorf("ready conversation requires execution evidence")
		}
		if record.ExecutionEvidence.ConversationID != record.Identity.ID {
			return fmt.Errorf("conversation execution evidence identity mismatch")
		}
	case CaptureStale, CaptureUnsupported:
		if record.Reason == "" {
			return fmt.Errorf("terminal conversation state %q requires a reason", record.State)
		}
	default:
		return fmt.Errorf("invalid conversation state %q", record.State)
	}
	return nil
}

func WriteConversation(root *fsq.DeliveryRoot, lease *Lease, record ConversationRecord) error {
	if err := lease.authorizeWrite(root); err != nil {
		return err
	}
	if !lease.holdsHandle(record.Handle) {
		return fmt.Errorf("conversation handle %q is not locked by the session lease", record.Handle)
	}
	if err := record.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = root.WriteFileAtomic(conversationDir, record.Handle+".json", data, 0o600)
	return err
}

func LoadConversation(root *fsq.DeliveryRoot, handle string) (ConversationRecord, error) {
	if root == nil {
		return ConversationRecord{}, fmt.Errorf("missing pinned session root")
	}
	if err := fsq.ValidateHandle(handle); err != nil {
		return ConversationRecord{}, err
	}
	file, info, err := root.OpenRegularNoFollow(filepath.Join(conversationDir, handle+".json"))
	if err != nil {
		return ConversationRecord{}, err
	}
	defer func() { _ = file.Close() }()
	if info.Mode().Perm() != 0o600 {
		return ConversationRecord{}, fmt.Errorf("conversation record permissions are %04o, want 0600", info.Mode().Perm())
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return ConversationRecord{}, err
	}
	var record ConversationRecord
	if err := decodeStrict(data, &record); err != nil {
		return ConversationRecord{}, fmt.Errorf("decode conversation record: %w", err)
	}
	if err := record.Validate(); err != nil {
		return ConversationRecord{}, err
	}
	if record.Handle != handle {
		return ConversationRecord{}, fmt.Errorf("conversation record handle %q does not match %q", record.Handle, handle)
	}
	return record, nil
}

func ConversationPath(sessionRoot, handle string) string {
	return filepath.Join(sessionRoot, conversationDir, handle+".json")
}
