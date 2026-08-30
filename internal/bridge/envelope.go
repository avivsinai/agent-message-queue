package bridge

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	EnvelopeVersion = 2

	// MaxPayloadBytes and MaxObjectBytes are the v2 protocol limits. The
	// payload limit applies after raw-standard-base64 decoding; the object
	// limit applies to the exact JSON file bytes on the wire.
	MaxPayloadBytes = 8 * 1024 * 1024
	MaxObjectBytes  = 12 * 1024 * 1024

	bridgeIdentifierMaxBytes = 63
	bridgeAliasMaxBytes      = 127
	bridgeMessageIDMaxBytes  = 200
	transferIDBytes          = 52
)

var (
	rawBase32Encoding = base32.StdEncoding.WithPadding(base32.NoPadding)
	rawBase64Encoding = base64.RawStdEncoding.Strict()
)

// Envelope is the v2 amq-bridge wire unit. Extra JSON fields are rejected.
// Payload remains []byte for callers that apply the decoded message locally;
// its wire representation is the payload_b64 JSON string below.
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
	Payload         []byte `json:"-"`
}

// envelopeJSON fixes both the v2 field name and the writer's key order. It is
// deliberately separate from Envelope so encoding/json cannot apply its
// padded []byte convention to Payload.
type envelopeJSON struct {
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
	PayloadB64      string `json:"payload_b64"`
}

func (env Envelope) MarshalJSON() ([]byte, error) {
	if err := ValidateEnvelope(env); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(envelopeJSON{
		Version:         env.Version,
		TransferID:      env.TransferID,
		SourceHost:      env.SourceHost,
		SourceHandle:    env.SourceHandle,
		DestAlias:       env.DestAlias,
		SourceMessageID: env.SourceMessageID,
		ThreadID:        env.ThreadID,
		PayloadSHA256:   env.PayloadSHA256,
		KeyGeneration:   env.KeyGeneration,
		Signature:       env.Signature,
		PayloadB64:      rawBase64Encoding.EncodeToString(env.Payload),
	})
	if err != nil {
		return nil, fmt.Errorf("bridge envelope: %w", err)
	}
	if len(raw) > MaxObjectBytes {
		return nil, fmt.Errorf("bridge envelope object exceeds %d bytes", MaxObjectBytes)
	}
	return raw, nil
}

func (env *Envelope) UnmarshalJSON(data []byte) error {
	if env == nil {
		return fmt.Errorf("bridge envelope is required")
	}
	if len(data) > MaxObjectBytes {
		return fmt.Errorf("bridge envelope object exceeds %d bytes", MaxObjectBytes)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire envelopeJSON
	if err := dec.Decode(&wire); err != nil {
		return fmt.Errorf("bridge envelope: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("bridge envelope has trailing JSON")
		}
		return fmt.Errorf("bridge envelope trailer: %w", err)
	}
	payload, err := decodePayloadB64(wire.PayloadB64)
	if err != nil {
		return err
	}
	candidate := Envelope{
		Version:         wire.Version,
		TransferID:      wire.TransferID,
		SourceHost:      wire.SourceHost,
		SourceHandle:    wire.SourceHandle,
		DestAlias:       wire.DestAlias,
		SourceMessageID: wire.SourceMessageID,
		ThreadID:        wire.ThreadID,
		PayloadSHA256:   wire.PayloadSHA256,
		KeyGeneration:   wire.KeyGeneration,
		Signature:       wire.Signature,
		Payload:         payload,
	}
	if err := ValidateEnvelope(candidate); err != nil {
		return err
	}
	*env = candidate
	return nil
}

func MarshalEnvelope(env Envelope) ([]byte, error) {
	if err := ValidateEnvelope(env); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxObjectBytes {
		return nil, fmt.Errorf("bridge envelope object exceeds %d bytes", MaxObjectBytes)
	}
	return raw, nil
}

func UnmarshalEnvelope(raw []byte) (Envelope, error) {
	if len(raw) > MaxObjectBytes {
		return Envelope{}, fmt.Errorf("bridge envelope object exceeds %d bytes", MaxObjectBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var env Envelope
	if err := dec.Decode(&env); err != nil {
		return Envelope{}, err
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
	return validateEnvelope(env, true)
}

func validateEnvelope(env Envelope, requireSignature bool) error {
	if env.Version != EnvelopeVersion {
		return fmt.Errorf("bridge envelope version %d is unsupported", env.Version)
	}
	if err := validateBridgeIdentifier("source_host", env.SourceHost); err != nil {
		return fmt.Errorf("bridge envelope %w", err)
	}
	if err := validateBridgeIdentifier("source_handle", env.SourceHandle); err != nil {
		return fmt.Errorf("bridge envelope %w", err)
	}
	if _, _, err := ParseAlias(env.DestAlias); err != nil {
		return err
	}
	if err := validateBridgeMessageID("source_message_id", env.SourceMessageID); err != nil {
		return fmt.Errorf("bridge envelope %w", err)
	}
	if err := validateBridgeMessageID("thread_id", env.ThreadID); err != nil {
		return fmt.Errorf("bridge envelope %w", err)
	}
	if err := validateLowerHex("payload_sha256", env.PayloadSHA256, sha256.Size*2); err != nil {
		return fmt.Errorf("bridge envelope %w", err)
	}
	if err := validateBridgeIdentifier("key_generation", env.KeyGeneration); err != nil {
		return fmt.Errorf("bridge envelope %w", err)
	}
	if err := validateTransferID(env.TransferID); err != nil {
		return err
	}
	wantTransferID := DeriveTransferID(env.SourceHost, env.SourceHandle, env.SourceMessageID, env.DestAlias)
	if env.TransferID != wantTransferID {
		return fmt.Errorf("bridge envelope transfer_id does not match routing fields")
	}
	if len(env.Payload) == 0 {
		return fmt.Errorf("bridge envelope payload is empty")
	}
	if len(env.Payload) > MaxPayloadBytes {
		return fmt.Errorf("bridge envelope payload exceeds %d bytes", MaxPayloadBytes)
	}
	sum := sha256.Sum256(env.Payload)
	if env.PayloadSHA256 != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("bridge envelope payload_sha256 does not match payload")
	}
	if requireSignature {
		if err := validateLowerHex("signature", env.Signature, ed25519SignatureHexBytes); err != nil {
			return fmt.Errorf("bridge envelope %w", err)
		}
	}
	return nil
}

const ed25519SignatureHexBytes = 64 * 2

func ParseAlias(alias string) (host, agent string, err error) {
	if len(alias) == 0 || len(alias) > bridgeAliasMaxBytes || strings.Count(alias, "/") != 1 {
		return "", "", fmt.Errorf("bridge dest_alias %q must be host/agent", alias)
	}
	host, agent, _ = strings.Cut(alias, "/")
	if err := validateBridgeIdentifier("dest_alias host", host); err != nil {
		return "", "", fmt.Errorf("bridge %w", err)
	}
	if err := validateBridgeIdentifier("dest_alias agent", agent); err != nil {
		return "", "", fmt.Errorf("bridge %w", err)
	}
	return host, agent, nil
}

func TransferFilename(sourceHost, transferID string) string {
	return "xfer-" + sourceHost + "-" + transferID + ".md"
}

// DeriveTransferID returns the v2 transfer id for the immutable routing
// claims. Callers that accept untrusted values must validate those values
// before using the result in a path.
func DeriveTransferID(sourceHost, sourceHandle, sourceMessageID, destAlias string) string {
	preimage := []byte("amq-xfer-v2\x00" + sourceHost + "\x00" + sourceHandle + "\x00" + sourceMessageID + "\x00" + destAlias)
	sum := sha256.Sum256(preimage)
	return strings.ToLower(rawBase32Encoding.EncodeToString(sum[:]))
}

func validateTransferID(id string) error {
	if len(id) != transferIDBytes {
		return fmt.Errorf("bridge envelope transfer_id must be %d lowercase base32 characters", transferIDBytes)
	}
	if id != strings.ToLower(id) {
		return fmt.Errorf("bridge envelope transfer_id must be lowercase")
	}
	if _, err := rawBase32Encoding.DecodeString(strings.ToUpper(id)); err != nil {
		return fmt.Errorf("bridge envelope transfer_id is invalid: %w", err)
	}
	return nil
}

func decodePayloadB64(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, fmt.Errorf("bridge envelope payload_b64 is empty")
	}
	if strings.TrimSpace(encoded) != encoded || strings.ContainsAny(encoded, " \t\r\n") {
		return nil, fmt.Errorf("bridge envelope payload_b64 contains whitespace")
	}
	if strings.Contains(encoded, "=") {
		return nil, fmt.Errorf("bridge envelope payload_b64 must not be padded")
	}
	payload, err := rawBase64Encoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("bridge envelope payload_b64 is invalid: %w", err)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("bridge envelope payload is empty")
	}
	if len(payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("bridge envelope payload exceeds %d bytes", MaxPayloadBytes)
	}
	if rawBase64Encoding.EncodeToString(payload) != encoded {
		return nil, fmt.Errorf("bridge envelope payload_b64 is not canonical raw standard base64")
	}
	return payload, nil
}

func validateBridgeIdentifier(name, value string) error {
	if len(value) == 0 {
		return fmt.Errorf("%s is invalid", name)
	}
	if len(value) > bridgeIdentifierMaxBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, bridgeIdentifierMaxBytes)
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if i == 0 {
			if !isLowerASCII(b) && !isASCIIDigit(b) {
				return fmt.Errorf("%s is invalid", name)
			}
			continue
		}
		if !isLowerASCII(b) && !isASCIIDigit(b) && b != '_' && b != '-' {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	return nil
}

func validateBridgeMessageID(name, value string) error {
	if len(value) == 0 {
		return fmt.Errorf("%s is invalid", name)
	}
	if len(value) > bridgeMessageIDMaxBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, bridgeMessageIDMaxBytes)
	}
	if value[0] == ' ' || value[len(value)-1] == ' ' {
		return fmt.Errorf("%s has surrounding space", name)
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b < 0x20 || b > 0x7e || b == '/' || b == '\\' {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	return nil
}

func validateLowerHex(name, value string, wantLen int) error {
	if len(value) != wantLen {
		return fmt.Errorf("%s must be %d lowercase hex characters", name, wantLen)
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') {
			continue
		}
		return fmt.Errorf("%s must be lowercase hex", name)
	}
	return nil
}

func isLowerASCII(b byte) bool { return b >= 'a' && b <= 'z' }

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }
