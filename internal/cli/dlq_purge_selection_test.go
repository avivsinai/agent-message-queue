package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDLQPurgeWithoutAgeFilterRemovesCorruptEnvelope(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	path, _ := writeFreshCorruptDLQEnvelope(t, root, "alice", "corrupt-unconditional-purge.md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{"--root", root, "--me", "alice", "--yes", "--json"})
	})
	if err != nil {
		t.Fatalf("unconditional purge of corrupt envelope: %v", err)
	}
	var result struct {
		Removed int `json:"removed"`
	}
	if err := unmarshalJSONOutput(stdout, &result); err != nil {
		t.Fatalf("decode unconditional purge output: %v (output: %s)", err, stdout)
	}
	if result.Removed != 1 {
		t.Fatalf("unconditional purge removed = %d, want 1", result.Removed)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("corrupt envelope remains after unconditional purge: %v", err)
	}
}

func writeOldValidDLQEnvelope(t *testing.T, root, agent, filename string) string {
	t.Helper()
	return writeDLQEnvelopeWithFailureTime(t, root, agent, filename, time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339))
}

func writeDLQEnvelopeWithFailureTime(t *testing.T, root, agent, filename, failureTime string) string {
	t.Helper()
	env := fsq.DLQEnvelope{
		Schema:        fsq.DLQSchemaVersion,
		ID:            strings.TrimSuffix(filename, ".md"),
		OriginalID:    "original-" + strings.TrimSuffix(filename, ".md"),
		OriginalFile:  "original.md",
		FailureReason: "test_failure",
		FailureDetail: "old deterministic fixture",
		FailureTime:   failureTime,
		SourceDir:     fsq.BoxNew,
	}
	header, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal valid DLQ envelope: %v", err)
	}
	data := append([]byte("---\n"), header...)
	data = append(data, []byte("\n---\nfixture body")...)
	path := filepath.Join(fsq.AgentDLQNew(root, agent), filename)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write valid old DLQ envelope: %v", err)
	}
	return path
}

func writeFreshCorruptDLQEnvelope(t *testing.T, root, agent, filename string) (string, []byte) {
	t.Helper()
	data := []byte("newly-created corrupt DLQ envelope")
	path := filepath.Join(fsq.AgentDLQNew(root, agent), filename)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write corrupt DLQ envelope: %v", err)
	}
	return path, data
}
