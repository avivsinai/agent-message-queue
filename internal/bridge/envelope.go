package bridge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const EnvelopeVersion = 1

// Envelope is the v1 amq-bridge wire unit. Extra JSON fields are rejected.
type Envelope struct {
	Version         int    `json:"version"`
	TransferID      string `json:"transfer_id"`
	SourceHost      string `json:"source_host"`
	SourceHandle    string `json:"source_handle"`
	DestAlias       string `json:"dest_alias"`
	SourceMessageID string `json:"source_message_id"`
	ThreadID        string `json:"thread_id"`
	PayloadSHA256   string `json:"payload_sha256"`
	KeyGeneration   string `json:"key_generation"`
	Signature       string `json:"signature"`
	Payload         []byte `json:"payload"`
}

func MarshalEnvelope(env Envelope) ([]byte, error) {
	if err := ValidateEnvelope(env); err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

func UnmarshalEnvelope(raw []byte) (Envelope, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var env Envelope
	if err := dec.Decode(&env); err != nil {
		return Envelope{}, fmt.Errorf("bridge envelope: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Envelope{}, fmt.Errorf("bridge envelope has trailing JSON")
		}
		return Envelope{}, fmt.Errorf("bridge envelope trailer: %w", err)
	}
	if err := ValidateEnvelope(env); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

func ValidateEnvelope(env Envelope) error {
	if env.Version != EnvelopeVersion {
		return fmt.Errorf("bridge envelope version %d is unsupported", env.Version)
	}
	for _, field := range []struct {
		name, value string
	}{
		{"transfer_id", env.TransferID},
		{"source_host", env.SourceHost},
		{"source_handle", env.SourceHandle},
		{"dest_alias", env.DestAlias},
		{"source_message_id", env.SourceMessageID},
		{"thread_id", env.ThreadID},
		{"payload_sha256", env.PayloadSHA256},
		{"key_generation", env.KeyGeneration},
		{"signature", env.Signature},
	} {
		if strings.TrimSpace(field.value) == "" || field.value != strings.TrimSpace(field.value) {
			return fmt.Errorf("bridge envelope %s is invalid", field.name)
		}
	}
	if err := fsq.ValidateHandle(env.SourceHost); err != nil {
		return fmt.Errorf("bridge envelope source_host: %w", err)
	}
	if err := fsq.ValidateHandle(env.SourceHandle); err != nil {
		return fmt.Errorf("bridge envelope source_handle: %w", err)
	}
	host, agent, err := ParseAlias(env.DestAlias)
	if err != nil {
		return err
	}
	_ = host
	_ = agent
	if err := validateTransferID(env.TransferID); err != nil {
		return err
	}
	sum := sha256.Sum256(env.Payload)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(env.PayloadSHA256, got) {
		return fmt.Errorf("bridge envelope payload_sha256 does not match payload")
	}
	if len(env.Payload) == 0 {
		return fmt.Errorf("bridge envelope payload is empty")
	}
	sig, err := hex.DecodeString(env.Signature)
	if err != nil || len(sig) != 64 {
		return fmt.Errorf("bridge envelope signature is invalid")
	}
	return nil
}

func ParseAlias(alias string) (host, agent string, err error) {
	host, agent, ok := strings.Cut(alias, "/")
	if !ok || strings.Contains(agent, "/") {
		return "", "", fmt.Errorf("bridge dest_alias %q must be host/agent", alias)
	}
	if err := fsq.ValidateHandle(host); err != nil {
		return "", "", fmt.Errorf("bridge dest_alias host: %w", err)
	}
	if err := fsq.ValidateHandle(agent); err != nil {
		return "", "", fmt.Errorf("bridge dest_alias agent: %w", err)
	}
	return host, agent, nil
}

func TransferFilename(sourceHost, transferID string) string {
	return "xfer-" + sourceHost + "-" + transferID + ".md"
}

func validateTransferID(id string) error {
	if strings.ContainsAny(id, "/\\.") || strings.HasPrefix(id, "-") || strings.HasPrefix(id, ".") {
		return fmt.Errorf("bridge envelope transfer_id is invalid")
	}
	if err := fsq.ValidateHandle(id); err != nil {
		return fmt.Errorf("bridge envelope transfer_id: %w", err)
	}
	return nil
}
