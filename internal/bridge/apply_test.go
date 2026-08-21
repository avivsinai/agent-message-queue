package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func testEnvelope(payload []byte) Envelope {
	sum := sha256.Sum256(payload)
	return Envelope{
		Version:         EnvelopeVersion,
		TransferID:      "t1",
		SourceHost:      "grok-host",
		SourceHandle:    "codex",
		DestAlias:       "mac/claude",
		SourceMessageID: "msg-1",
		ThreadID:        "thread-1",
		PayloadSHA256:   hex.EncodeToString(sum[:]),
		KeyGeneration:   "1",
		Signature:       strings.Repeat("00", 64),
		Payload:         payload,
	}
}

func TestUnmarshalEnvelopeRejectsUnknownFields(t *testing.T) {
	_, err := UnmarshalEnvelope([]byte(`{"version":1,"transfer_id":"t1","source_host":"grok-host","source_handle":"codex","dest_alias":"mac/claude","source_message_id":"m","thread_id":"t","payload_sha256":"00","key_generation":"1","signature":"` + strings.Repeat("0", 128) + `","payload":"YQ==","root":"/etc"}`))
	if err == nil {
		t.Fatal("expected unknown field rejection")
	}
}

func TestUnmarshalEnvelopeRejectsForbiddenRoutingFields(t *testing.T) {
	base := `{"version":1,"transfer_id":"t1","source_host":"grok-host","source_handle":"codex","dest_alias":"mac/claude","source_message_id":"m","thread_id":"t","payload_sha256":"00","key_generation":"1","signature":"` + strings.Repeat("0", 128) + `","payload":"YQ=="`
	for _, field := range []string{"path", "argv", "env", "executable", "endpoint", "session"} {
		raw := []byte(base + `,"` + field + `":"x"}`)
		if _, err := UnmarshalEnvelope(raw); err == nil {
			t.Fatalf("field %q accepted", field)
		}
	}
}

func TestApplyEnvelopeIgnoresUntrustedPayloadRoutingHints(t *testing.T) {
	base := t.TempDir()
	if err := fsq.EnsureAgentDirs(base, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(base, "attacker"); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	payload, err := (format.Message{
		Header: format.Header{
			Schema:       1,
			ID:           "msg-untrusted",
			From:         "attacker",
			To:           []string{"attacker"},
			Thread:       "session/other",
			Created:      "2026-08-20T00:00:00Z",
			ReplyTo:      "attacker@foreign",
			FromProject:  "evil",
			ReplyProject: "evil",
			Context: map[string]any{
				"root":    "/tmp/stolen-root",
				"session": "session9",
			},
		},
		Body: "do not route from payload",
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyEnvelope(root, "mac", "claude", testEnvelope(payload))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename("grok-host", "t1"))
	if result.Path != want {
		t.Fatalf("path = %q, want dest_alias mailbox %q", result.Path, want)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(base, "attacker"), TransferFilename("grok-host", "t1"))); !os.IsNotExist(err) {
		t.Fatalf("payload routed into attacker mailbox: %v", err)
	}
}

func TestValidateEnvelopeRejectsDigestMismatchAndAlias(t *testing.T) {
	env := testEnvelope([]byte("hello"))
	env.PayloadSHA256 = strings.Repeat("0", 64)
	if err := ValidateEnvelope(env); err == nil {
		t.Fatal("expected digest mismatch")
	}
	env = testEnvelope([]byte("hello"))
	env.DestAlias = "mac/claude/extra"
	if err := ValidateEnvelope(env); err == nil {
		t.Fatal("expected alias rejection")
	}
	env = testEnvelope([]byte("hello"))
	env.TransferID = "../t1"
	if err := ValidateEnvelope(env); err == nil {
		t.Fatal("expected transfer_id rejection")
	}
}

func TestApplyEnvelopeReplayAndConflict(t *testing.T) {
	base := t.TempDir()
	if err := fsq.EnsureAgentDirs(base, "claude"); err != nil {
		t.Fatal(err)
	}
	identity, err := fsq.SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	root, err := fsq.OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	payload := []byte("hello-bridge")
	first, err := ApplyEnvelope(root, "mac", "claude", testEnvelope(payload))
	if err != nil || first.Replayed {
		t.Fatalf("first apply: %#v %v", first, err)
	}
	if _, statErr := os.Stat(first.Path); statErr != nil {
		t.Fatalf("missing dest: %v", statErr)
	}

	replay, err := ApplyEnvelope(root, "mac", "claude", testEnvelope(payload))
	if err != nil || !replay.Replayed {
		t.Fatalf("replay: %#v %v", replay, err)
	}

	conflict := testEnvelope([]byte("other-digest"))
	conflict.TransferID = "t1"
	_, err = ApplyEnvelope(root, "mac", "claude", conflict)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("conflict = %v, want EEXIST", err)
	}
	got, readErr := os.ReadFile(filepath.Join(fsq.AgentInboxNew(base, "claude"), TransferFilename("grok-host", "t1")))
	if readErr != nil || string(got) != string(payload) {
		t.Fatalf("conflict overwrote dest: %q %v", got, readErr)
	}

	_, err = ApplyEnvelope(root, "mac", "codex", testEnvelope(payload))
	if err == nil {
		t.Fatal("expected local agent mismatch")
	}

	_, err = ApplyEnvelope(root, "grok", "claude", testEnvelope(payload))
	if err == nil {
		t.Fatal("expected foreign dest host refusal")
	}

	otherHost := testEnvelope(payload)
	otherHost.SourceHost = "other-host"
	other, err := ApplyEnvelope(root, "mac", "claude", otherHost)
	if err != nil || other.Replayed {
		t.Fatalf("distinct source host: %#v %v", other, err)
	}
	if other.Path == first.Path {
		t.Fatal("distinct source hosts shared a transfer filename")
	}
}
