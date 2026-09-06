package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDLQRetryAllSkipsDotfilesEvenWhenTheyParse(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	visible := writeOldValidDLQEnvelope(t, root, "alice", "visible-retry.md")
	data, err := os.ReadFile(visible)
	if err != nil {
		t.Fatalf("read visible envelope: %v", err)
	}
	hidden := filepath.Join(fsq.AgentDLQNew(root, "alice"), ".hidden-retry.md")
	if err := os.WriteFile(hidden, data, 0o600); err != nil {
		t.Fatalf("write hidden envelope: %v", err)
	}

	stdout, _, retryErr := captureEnvOutput(t, func() error {
		return runDLQRetry([]string{"--root", root, "--me", "alice", "--all", "--json"})
	})
	if retryErr != nil {
		t.Fatalf("retry-all: %v (output: %s)", retryErr, stdout)
	}
	var result struct {
		Retried []string `json:"retried"`
		Skipped []string `json:"skipped"`
		Count   int      `json:"count"`
	}
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode retry-all: %v (output: %s)", decodeErr, stdout)
	}
	if result.Count != 1 || !containsString(result.Retried, "visible-retry") {
		t.Fatalf("retry-all result = %#v, want visible envelope retried once", result)
	}
	if containsString(result.Retried, ".hidden-retry") || containsString(result.Skipped, ".hidden-retry.md") {
		t.Fatalf("hidden envelope was scanned: %#v", result)
	}
	if _, err := os.Stat(visible); !os.IsNotExist(err) {
		t.Fatalf("visible DLQ envelope was not moved from new: %v", err)
	}
	if _, err := os.Stat(hidden); err != nil {
		t.Fatalf("hidden DLQ envelope was retried away: %v", err)
	}
}

func moveInvalidFixtureToDLQForRetryAll(t *testing.T, root, agent, id string) string {
	t.Helper()
	deliverInvalidDLQTransitionFixture(t, root, agent, id)
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		t.Fatalf("snapshot retry-all delivery root: %v", err)
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		t.Fatalf("open retry-all delivery root: %v", err)
	}
	defer func() { _ = deliveryRoot.Close() }()
	path, err := fsq.MoveToDLQ(deliveryRoot, agent, id+".md", id, "parse_error", "retry-all fixture")
	if err != nil {
		t.Fatalf("move retry-all fixture to DLQ: %v", err)
	}
	return path
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
