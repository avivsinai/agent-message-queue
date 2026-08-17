package launch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	EvidenceVersion   = 1
	evidenceDirectory = "meta/launch/evidence"
)

type EvidenceKind string

const (
	EvidenceProviderCapture EvidenceKind = "provider_capture"
	EvidenceRetainedCapture EvidenceKind = "retained_capture"
	EvidenceFixture         EvidenceKind = "fixture"
	EvidenceManual          EvidenceKind = "manual"
)

type EvidenceRecord struct {
	EvidenceVersion int             `json:"evidence_version"`
	Kind            EvidenceKind    `json:"kind"`
	Handle          string          `json:"handle"`
	ObservedAt      time.Time       `json:"observed_at"`
	PayloadSHA256   string          `json:"payload_sha256"`
	Payload         json.RawMessage `json:"payload"`
}

type EvidenceRef struct {
	EvidenceVersion int          `json:"evidence_version"`
	ID              string       `json:"id"`
	Kind            EvidenceKind `json:"kind"`
	SHA256          string       `json:"sha256"`
	ObservedAt      time.Time    `json:"observed_at"`
}

type EvidenceWriteRequest struct {
	Kind       EvidenceKind
	Handle     string
	ObservedAt time.Time
	Payload    []byte
}

type EvidenceCorruptError struct {
	ID     string
	Reason string
}

func (e *EvidenceCorruptError) Error() string {
	return fmt.Sprintf("evidence_corrupt: %s: %s", e.ID, e.Reason)
}

type EvidenceExistsError struct{ ID string }

func (e *EvidenceExistsError) Error() string {
	return fmt.Sprintf("immutable evidence already exists: %s", e.ID)
}

func WriteEvidence(root *fsq.DeliveryRoot, lease *Lease, request EvidenceWriteRequest) (EvidenceRef, error) {
	if err := lease.authorizeWrite(root); err != nil {
		return EvidenceRef{}, err
	}
	if !lease.holdsHandle(request.Handle) {
		return EvidenceRef{}, fmt.Errorf("evidence handle %q is not locked by the session lease", request.Handle)
	}
	record, ref, data, err := prepareEvidence(request)
	if err != nil {
		return EvidenceRef{}, err
	}
	_, err = root.WriteFileExclusive(evidenceDirectory, evidenceFilename(ref.ID), data, 0o600)
	if errors.Is(err, os.ErrExist) {
		return EvidenceRef{}, &EvidenceExistsError{ID: ref.ID}
	}
	if err != nil {
		return EvidenceRef{}, err
	}
	return evidenceRef(ref.ID, record), nil
}

func prepareEvidence(request EvidenceWriteRequest) (EvidenceRecord, EvidenceRef, []byte, error) {
	payload, err := canonicalEvidencePayload(request.Payload)
	if err != nil {
		return EvidenceRecord{}, EvidenceRef{}, nil, err
	}
	payloadDigest := digestBytes(payload)
	record := EvidenceRecord{
		EvidenceVersion: EvidenceVersion, Kind: request.Kind, Handle: request.Handle,
		ObservedAt: request.ObservedAt.UTC(), PayloadSHA256: payloadDigest, Payload: payload,
	}
	if err := record.Validate(); err != nil {
		return EvidenceRecord{}, EvidenceRef{}, nil, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return EvidenceRecord{}, EvidenceRef{}, nil, err
	}
	id := digestBytes(data)
	return record, evidenceRef(id, record), data, nil
}

func ReadEvidence(root *fsq.DeliveryRoot, id string) (EvidenceRecord, EvidenceRef, error) {
	record, ref, _, err := readEvidence(root, id)
	return record, ref, err
}

func readEvidence(root *fsq.DeliveryRoot, id string) (EvidenceRecord, EvidenceRef, []byte, error) {
	if !validDigest(id) {
		return EvidenceRecord{}, EvidenceRef{}, nil, &EvidenceCorruptError{ID: id, Reason: "invalid evidence id"}
	}
	file, info, err := root.OpenRegularNoFollow(filepath.Join(evidenceDirectory, evidenceFilename(id)))
	if err != nil {
		return EvidenceRecord{}, EvidenceRef{}, nil, &EvidenceCorruptError{ID: id, Reason: err.Error()}
	}
	defer func() { _ = file.Close() }()
	if info.Mode().Perm() != 0o600 {
		return EvidenceRecord{}, EvidenceRef{}, nil, &EvidenceCorruptError{ID: id, Reason: fmt.Sprintf("permissions are %04o, want 0600", info.Mode().Perm())}
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return EvidenceRecord{}, EvidenceRef{}, nil, err
	}
	if digestBytes(data) != id {
		return EvidenceRecord{}, EvidenceRef{}, nil, &EvidenceCorruptError{ID: id, Reason: "record digest mismatch"}
	}
	var record EvidenceRecord
	if err := decodeStrict(data, &record); err != nil {
		return EvidenceRecord{}, EvidenceRef{}, nil, &EvidenceCorruptError{ID: id, Reason: err.Error()}
	}
	if err := record.Validate(); err != nil {
		return EvidenceRecord{}, EvidenceRef{}, nil, &EvidenceCorruptError{ID: id, Reason: err.Error()}
	}
	return record, evidenceRef(id, record), data, nil
}

func (record EvidenceRecord) Validate() error {
	if record.EvidenceVersion != EvidenceVersion {
		return fmt.Errorf("unsupported evidence version %d", record.EvidenceVersion)
	}
	if !slices.Contains([]EvidenceKind{EvidenceProviderCapture, EvidenceRetainedCapture, EvidenceFixture, EvidenceManual}, record.Kind) {
		return fmt.Errorf("invalid evidence kind %q", record.Kind)
	}
	if err := fsq.ValidateHandle(record.Handle); err != nil {
		return fmt.Errorf("invalid evidence handle: %w", err)
	}
	if record.ObservedAt.IsZero() || record.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("observed_at must be a non-zero UTC timestamp")
	}
	payload, err := canonicalEvidencePayload(record.Payload)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, record.Payload) {
		return fmt.Errorf("payload is not canonical JSON")
	}
	if record.PayloadSHA256 != digestBytes(payload) {
		return fmt.Errorf("payload digest mismatch")
	}
	return nil
}

func canonicalEvidencePayload(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode evidence payload: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode evidence payload: trailing JSON value")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode canonical evidence payload: %w", err)
	}
	return data, nil
}

func evidenceRef(id string, record EvidenceRecord) EvidenceRef {
	return EvidenceRef{EvidenceVersion: EvidenceVersion, ID: id, Kind: record.Kind, SHA256: id, ObservedAt: record.ObservedAt}
}

func evidenceFilename(id string) string { return strings.TrimPrefix(id, "sha256:") + ".json" }

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func EvidencePath(sessionRoot, id string) string {
	return filepath.Join(sessionRoot, evidenceDirectory, evidenceFilename(id))
}

func persistProviderCaptureEvidence(root *fsq.DeliveryRoot, lease *Lease, handle string, evidence []CaptureEvidence) ([]string, error) {
	refs := make([]string, 0, len(evidence))
	for _, item := range evidence {
		if !item.verified || len(item.payload) == 0 {
			return nil, fmt.Errorf("provider capture evidence is not persistable")
		}
		switch item.source {
		case CodexNotifyV1:
			payload, err := decodeCodexNotifyPayload(item.payload)
			if err != nil || payload.Handle != handle || item.handle != handle || payload.LaunchNonce != item.launchNonce ||
				payload.ConversationID != item.conversationID || payload.ProviderVersion != item.providerVersion {
				return nil, fmt.Errorf("codex provider capture evidence is not persistable")
			}
		case CursorCreateChatV1:
			payload, err := decodeCursorCreateChatPayload(item.payload)
			if err != nil || payload.Handle != handle || item.handle != handle || payload.LaunchNonce != item.launchNonce ||
				payload.ConversationID != item.conversationID || payload.ProviderVersion != item.providerVersion {
				return nil, fmt.Errorf("cursor provider capture evidence is not persistable")
			}
		default:
			return nil, fmt.Errorf("provider capture evidence source is not persistable")
		}
		request := EvidenceWriteRequest{
			Kind: EvidenceProviderCapture, Handle: handle, ObservedAt: item.observedAt, Payload: item.payload,
		}
		ref, err := WriteEvidence(root, lease, request)
		var exists *EvidenceExistsError
		if errors.As(err, &exists) {
			_, expectedRef, expectedData, prepareErr := prepareEvidence(request)
			if prepareErr != nil {
				return nil, prepareErr
			}
			record, existingRef, existingData, readErr := readEvidence(root, exists.ID)
			if readErr != nil {
				return nil, readErr
			}
			if existingRef.ID != expectedRef.ID || !bytes.Equal(existingData, expectedData) || record.Handle != handle || record.Kind != EvidenceProviderCapture {
				return nil, &EvidenceCorruptError{ID: exists.ID, Reason: "existing record does not match provider capture"}
			}
			ref, err = existingRef, nil
		}
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref.ID)
	}
	return refs, nil
}

func findProviderCaptureEvidence(root *fsq.DeliveryRoot, provider, handle, nonce, providerVersion string) (CaptureEvidence, string, bool, error) {
	entries, err := root.ReadDir(evidenceDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return CaptureEvidence{}, "", false, nil
	}
	if err != nil {
		return CaptureEvidence{}, "", false, err
	}
	var found CaptureEvidence
	foundID := ""
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := "sha256:" + strings.TrimSuffix(entry.Name(), ".json")
		record, _, err := ReadEvidence(root, id)
		if err != nil {
			return CaptureEvidence{}, "", false, err
		}
		if record.Kind != EvidenceProviderCapture || record.Handle != handle {
			continue
		}
		var envelope struct {
			Source CaptureEvidenceSource `json:"source"`
		}
		if err := json.Unmarshal(record.Payload, &envelope); err != nil {
			return CaptureEvidence{}, "", false, fmt.Errorf("decode provider capture source: %w", err)
		}
		var candidate CaptureEvidence
		switch provider {
		case CursorProvider:
			if envelope.Source != CursorCreateChatV1 {
				continue
			}
			payload, err := decodeCursorCreateChatPayload(record.Payload)
			if err != nil {
				return CaptureEvidence{}, "", false, err
			}
			if payload.Handle != handle || payload.LaunchNonce != nonce || payload.ProviderVersion != providerVersion {
				continue
			}
			candidate = CaptureEvidence{
				source: CursorCreateChatV1, provider: CursorProvider, providerVersion: payload.ProviderVersion,
				launchNonce: payload.LaunchNonce, handle: payload.Handle, conversationID: payload.ConversationID,
				verified: true, observedAt: record.ObservedAt, payload: bytes.Clone(record.Payload),
			}
		case CodexProvider:
			if envelope.Source != CodexNotifyV1 {
				continue
			}
			payload, err := decodeCodexNotifyPayload(record.Payload)
			if err != nil {
				return CaptureEvidence{}, "", false, err
			}
			if payload.Handle != handle || payload.LaunchNonce != nonce || payload.ProviderVersion != providerVersion {
				continue
			}
			candidate = CaptureEvidence{
				source: CodexNotifyV1, provider: CodexProvider, providerVersion: payload.ProviderVersion,
				launchNonce: payload.LaunchNonce, handle: payload.Handle, conversationID: payload.ConversationID,
				verified: true, observedAt: record.ObservedAt, payload: bytes.Clone(record.Payload),
			}
		default:
			return CaptureEvidence{}, "", false, fmt.Errorf("pre-spawn capture provider %q is unsupported", provider)
		}
		if foundID != "" && foundID != id {
			return CaptureEvidence{}, "", false, fmt.Errorf("%s acquisition evidence is ambiguous for handle %q", provider, handle)
		}
		found = candidate
		foundID = id
	}
	return found, foundID, foundID != "", nil
}

func findCursorCaptureEvidence(root *fsq.DeliveryRoot, handle, nonce, providerVersion string) (CaptureEvidence, string, bool, error) {
	return findProviderCaptureEvidence(root, CursorProvider, handle, nonce, providerVersion)
}

func CollectEvidenceRefs(root *fsq.DeliveryRoot, handles []string) ([]EvidenceRef, error) {
	seen := make(map[string]struct{})
	refs := make([]EvidenceRef, 0)
	for _, handle := range handles {
		record, err := LoadConversation(root, handle)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, id := range record.EvidenceRefs {
			if _, exists := seen[id]; exists {
				continue
			}
			_, ref, err := ReadEvidence(root, id)
			if err != nil {
				return nil, err
			}
			seen[id] = struct{}{}
			refs = append(refs, ref)
		}
	}
	slices.SortFunc(refs, func(a, b EvidenceRef) int { return strings.Compare(a.ID, b.ID) })
	return refs, nil
}
