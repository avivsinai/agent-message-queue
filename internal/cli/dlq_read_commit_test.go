package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDLQListAndReadExposeTerminalRetryState(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	dlqPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "terminal-state")
	filename := filepath.Base(dlqPath)
	dlqID := strings.TrimSuffix(filename, ".md")
	deliveryRoot := openDeliveryRootForCLITest(t, root)
	if err := fsq.RetryFromDLQ(deliveryRoot, "alice", filename, false); err != nil {
		t.Fatalf("RetryFromDLQ: %v", err)
	}

	listJSON, _, err := captureEnvOutput(t, func() error {
		return runDLQList([]string{"--root", root, "--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("dlq list JSON: %v", err)
	}
	var items []dlqListItem
	if err := unmarshalJSONOutput(listJSON, &items); err != nil {
		t.Fatalf("decode dlq list JSON: %v (output: %s)", err, listJSON)
	}
	if len(items) != 1 || items[0].RetryCount != 1 || items[0].RetryState != fsq.RetryStateDelivered || items[0].RetryPending || !items[0].RetryDelivered {
		t.Fatalf("dlq list retry state = %#v, want one terminal retry", items)
	}
	for _, want := range []string{`"retry_state": "delivered"`, `"retry_pending": false`, `"retry_delivered": true`} {
		if !strings.Contains(listJSON, want) {
			t.Fatalf("dlq list JSON = %q, want field %q", listJSON, want)
		}
	}

	listText, _, err := captureEnvOutput(t, func() error {
		return runDLQList([]string{"--root", root, "--me", "alice"})
	})
	if err != nil {
		t.Fatalf("dlq list text: %v", err)
	}
	for _, want := range []string{"retries: 1", "state: delivered"} {
		if !strings.Contains(listText, want) {
			t.Fatalf("dlq list text = %q, want %q", listText, want)
		}
	}
	if strings.Contains(listText, "retry_pending") || strings.Contains(listText, "retry_delivered") {
		t.Fatalf("dlq list text exposes raw state fields instead of concise state: %q", listText)
	}

	readJSON, _, err := captureEnvOutput(t, func() error {
		return runDLQRead([]string{"--root", root, "--me", "alice", "--id", dlqID, "--json"})
	})
	if err != nil {
		t.Fatalf("dlq read JSON: %v", err)
	}
	var result dlqReadResult
	if err := unmarshalJSONOutput(readJSON, &result); err != nil {
		t.Fatalf("decode dlq read JSON: %v (output: %s)", err, readJSON)
	}
	if result.RetryCount != 1 || result.RetryState != fsq.RetryStateDelivered || result.RetryPending || !result.RetryDelivered {
		t.Fatalf("dlq read retry state = %#v, want terminal retry", result)
	}
	for _, want := range []string{`"retry_state": "delivered"`, `"retry_pending": false`, `"retry_delivered": true`} {
		if !strings.Contains(readJSON, want) {
			t.Fatalf("dlq read JSON = %q, want field %q", readJSON, want)
		}
	}

	readText, _, err := captureEnvOutput(t, func() error {
		return runDLQRead([]string{"--root", root, "--me", "alice", "--id", dlqID})
	})
	if err != nil {
		t.Fatalf("dlq read text: %v", err)
	}
	for _, want := range []string{"Retry State:    delivered", "Retry Pending:  false", "Retry Delivered: true"} {
		if !strings.Contains(readText, want) {
			t.Fatalf("dlq read text = %q, want %q", readText, want)
		}
	}
}

func TestDLQPurgeRemovesDeliveredRetryTombstoneWithoutTouchingDelivery(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const originalID = "purge-terminal-state"
	dlqPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", originalID)
	filename := filepath.Base(dlqPath)
	deliveryRoot := openDeliveryRootForCLITest(t, root)
	if err := fsq.RetryFromDLQ(deliveryRoot, "alice", filename, false); err != nil {
		t.Fatalf("RetryFromDLQ: %v", err)
	}
	curPath := filepath.Join(fsq.AgentDLQCur(root, "alice"), filename)
	inboxPath := filepath.Join(fsq.AgentInboxNew(root, "alice"), originalID+".md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDLQPurge([]string{"--root", root, "--me", "alice", "--yes", "--json"})
	})
	if err != nil {
		t.Fatalf("purge delivered tombstone: %v", err)
	}
	var result struct {
		Removed int `json:"removed"`
	}
	if err := unmarshalJSONOutput(stdout, &result); err != nil {
		t.Fatalf("decode purge output: %v (output: %s)", err, stdout)
	}
	if result.Removed != 1 {
		t.Fatalf("purge removed = %d, want one delivered tombstone", result.Removed)
	}
	if _, err := os.Stat(curPath); !os.IsNotExist(err) {
		t.Fatalf("delivered tombstone remains after purge: %v", err)
	}
	if _, err := os.Stat(inboxPath); err != nil {
		t.Fatalf("purge touched retried inbox delivery: %v", err)
	}
	if _, err := fsq.MoveToDLQ(deliveryRoot, "alice", originalID+".md", originalID, "parse_error", "still malformed"); err != nil {
		t.Fatalf("consume retried delivery back to DLQ: %v", err)
	}
	if err := fsq.RetryFromDLQ(deliveryRoot, "alice", filename, true); !os.IsNotExist(err) {
		t.Fatalf("retry purged terminal ID = %v, want not found", err)
	}
	if _, err := os.Stat(inboxPath); !os.IsNotExist(err) {
		t.Fatalf("purged old retry ID recreated inbox delivery: %v", err)
	}
}
