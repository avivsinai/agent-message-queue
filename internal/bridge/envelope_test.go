package bridge

import (
	"bytes"
	"strings"
	"testing"
)

func TestEnvelopeV2JSONRoundTripUsesRawStandardPayload(t *testing.T) {
	env := testEnvelope([]byte("f"))
	raw, err := MarshalEnvelope(env)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"payload":`)) {
		t.Fatalf("wire envelope used the v1 payload field: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"payload_b64":"Zg"`)) {
		t.Fatalf("wire envelope did not use raw payload_b64: %s", raw)
	}
	if bytes.Contains(raw, []byte("=")) {
		t.Fatalf("wire envelope contains base64 padding: %s", raw)
	}
	decoded, err := UnmarshalEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Payload, env.Payload) {
		t.Fatalf("decoded payload = %q, want %q", decoded.Payload, env.Payload)
	}
}

func TestUnmarshalEnvelopeRejectsPaddedV1PayloadField(t *testing.T) {
	raw, err := MarshalEnvelope(testEnvelope([]byte("a")))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"payload_b64":"YQ"`), []byte(`"payload":"YQ=="`), 1)
	if _, err := UnmarshalEnvelope(raw); err == nil {
		t.Fatal("padded v1 payload field was accepted")
	}
	raw, err = MarshalEnvelope(testEnvelope([]byte("a")))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"payload_b64":"YQ"`), []byte(`"payload_b64":"YQ=="`), 1)
	if _, err := UnmarshalEnvelope(raw); err == nil {
		t.Fatal("padded payload_b64 was accepted")
	}
}

func TestUnmarshalEnvelopeRejectsV1ByDefault(t *testing.T) {
	env := testEnvelope([]byte("v1"))
	env.Version = 1
	if _, err := MarshalEnvelope(env); err == nil {
		t.Fatal("v1 envelope was marshaled")
	}
}

func TestValidateEnvelopeRejectsLFInThreadID(t *testing.T) {
	env := testEnvelope([]byte("line"))
	env.ThreadID = "thread\nid"
	if err := ValidateEnvelope(env); err == nil || !strings.Contains(err.Error(), "thread_id") {
		t.Fatalf("LF thread_id error = %v, want thread_id rejection", err)
	}
}

func TestValidateEnvelopePayloadLimit(t *testing.T) {
	max := bytes.Repeat([]byte{'x'}, MaxPayloadBytes)
	if err := ValidateEnvelope(testEnvelope(max)); err != nil {
		t.Fatalf("maximum payload rejected: %v", err)
	}
	raw, err := MarshalEnvelope(testEnvelope(max))
	if err != nil {
		t.Fatalf("maximum payload could not be serialized: %v", err)
	}
	if len(raw) > MaxObjectBytes {
		t.Fatalf("maximum payload object length = %d, exceeds %d", len(raw), MaxObjectBytes)
	}
	if decoded, err := UnmarshalEnvelope(raw); err != nil || !bytes.Equal(decoded.Payload, max) {
		t.Fatalf("maximum payload could not be decoded: %v", err)
	}
	tooLarge := append(max, 'x')
	if err := ValidateEnvelope(testEnvelope(tooLarge)); err == nil || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("payload over limit error = %v, want payload rejection", err)
	}
	if _, err := MarshalEnvelope(testEnvelope(tooLarge)); err == nil {
		t.Fatal("payload over limit was serialized")
	}
}

func TestUnmarshalEnvelopeObjectLimit(t *testing.T) {
	raw := bytes.Repeat([]byte{'x'}, MaxObjectBytes+1)
	if _, err := UnmarshalEnvelope(raw); err == nil || !strings.Contains(err.Error(), "object") {
		t.Fatalf("oversized object error = %v, want object limit rejection", err)
	}
}

func TestDeriveTransferIDUsesLowercaseBase32SHA256Shape(t *testing.T) {
	id := DeriveTransferID("grok-host", "codex", "msg-1", "mac/claude")
	if len(id) != 52 {
		t.Fatalf("transfer_id length = %d, want 52", len(id))
	}
	if err := validateTransferID(id); err != nil {
		t.Fatalf("derived transfer_id = %q: %v", id, err)
	}
	const want = "ulpa7ailolhzdzs2safnkkejahkxl2d4arl2iokz6ugbvc3pprjq"
	if id != want {
		t.Fatalf("derived transfer_id = %q, want %q", id, want)
	}
	if strings.ToUpper(id) == id {
		t.Fatalf("derived transfer_id is not lowercase: %q", id)
	}
}
