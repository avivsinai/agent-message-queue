package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunDLQReadOutputsCommittedCurEnvelopeBeforeDurabilityError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	dlqPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "read-committed-cur")
	filename := filepath.Base(dlqPath)
	dlqID := strings.TrimSuffix(filename, ".md")

	injectedErr := errors.New("injected committed DLQ inspection sync failure")
	oldInspect := inspectDLQMessage
	inspectDLQMessage = func(deliveryRoot *fsq.DeliveryRoot, agent, gotFilename string) (*fsq.DLQEnvelope, []byte, string, error) {
		envelope, body, box, err := oldInspect(deliveryRoot, agent, gotFilename)
		if err != nil {
			return envelope, body, box, err
		}
		return envelope, body, box, &fsq.CommittedDurabilityError{
			FinalPath: deliveryRoot.DisplayPath(filepath.Join("agents", agent, "dlq", "cur", gotFilename)),
			Recipient: agent,
			Err:       injectedErr,
		}
	}
	t.Cleanup(func() { inspectDLQMessage = oldInspect })

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runDLQRead([]string{
			"--root", root,
			"--me", "alice",
			"--id", dlqID,
			"--json",
		})
	})
	var committed *fsq.CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, injectedErr) || GetExitCode(err) != ExitError {
		t.Fatalf("dlq read error = %T %v (exit %d), want committed durability error", err, err, GetExitCode(err))
	}
	wantCur := filepath.Join(fsq.AgentDLQCur(root, "alice"), filename)
	if committed.FinalPath != wantCur || committed.Recipient != "alice" {
		t.Fatalf("committed DLQ read = %#v, want path %q recipient alice", committed, wantCur)
	}
	if strings.Contains(stderr, "warning: failed to move DLQ message to cur") {
		t.Fatalf("committed DLQ read was reduced to a warning: %s", stderr)
	}

	var result dlqReadResult
	if decodeErr := unmarshalJSONOutput(stdout, &result); decodeErr != nil {
		t.Fatalf("decode committed DLQ read output: %v (output: %s)", decodeErr, stdout)
	}
	if result.ID != dlqID || result.Box != fsq.BoxCur || result.OriginalContent == "" {
		t.Fatalf("committed DLQ read output = %#v, want visible cur envelope", result)
	}
	if _, statErr := os.Stat(dlqPath); !os.IsNotExist(statErr) {
		t.Fatalf("committed envelope remains in dlq/new: %v", statErr)
	}
	if _, statErr := os.Stat(wantCur); statErr != nil {
		t.Fatalf("committed envelope missing from dlq/cur: %v", statErr)
	}
}

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

func TestDLQListAndReadExposeLegacyIndeterminateRetryState(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	const (
		dlqID    = "legacy-indeterminate"
		filename = dlqID + ".md"
	)
	raw := []byte("---\n" +
		`{"schema":"amq/dlq/v1","id":"legacy-indeterminate","original_id":"legacy-original","original_file":"legacy-original.md","failure_reason":"parse_error","failure_detail":"legacy fixture","failure_time":"2026-07-28T00:00:00Z","retry_count":1,"source_dir":"new"}` +
		"\n---\nlegacy body")
	if err := os.WriteFile(filepath.Join(fsq.AgentDLQNew(root, "alice"), filename), raw, 0o600); err != nil {
		t.Fatalf("write legacy DLQ envelope: %v", err)
	}

	listJSON, _, err := captureEnvOutput(t, func() error {
		return runDLQList([]string{"--root", root, "--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("dlq list JSON: %v", err)
	}
	var items []dlqListItem
	if err := unmarshalJSONOutput(listJSON, &items); err != nil {
		t.Fatalf("decode legacy list JSON: %v (output: %s)", err, listJSON)
	}
	if len(items) != 1 || items[0].RetryState != fsq.RetryStateIndeterminate || items[0].RetryPending || items[0].RetryDelivered {
		t.Fatalf("legacy list state = %#v, want indeterminate compatibility view", items)
	}
	if !strings.Contains(listJSON, `"retry_state": "indeterminate"`) {
		t.Fatalf("legacy list JSON = %q, want retry_state", listJSON)
	}
	listText, _, err := captureEnvOutput(t, func() error {
		return runDLQList([]string{"--root", root, "--me", "alice"})
	})
	if err != nil || !strings.Contains(listText, "state: indeterminate") {
		t.Fatalf("legacy list text = %q err=%v, want indeterminate state", listText, err)
	}

	readJSON, _, err := captureEnvOutput(t, func() error {
		return runDLQRead([]string{"--root", root, "--me", "alice", "--id", dlqID, "--json"})
	})
	if err != nil {
		t.Fatalf("dlq read JSON: %v", err)
	}
	var result dlqReadResult
	if err := unmarshalJSONOutput(readJSON, &result); err != nil {
		t.Fatalf("decode legacy read JSON: %v (output: %s)", err, readJSON)
	}
	if result.RetryState != fsq.RetryStateIndeterminate || result.RetryPending || result.RetryDelivered {
		t.Fatalf("legacy read state = %#v, want indeterminate compatibility view", result)
	}
	if !strings.Contains(readJSON, `"retry_state": "indeterminate"`) {
		t.Fatalf("legacy read JSON = %q, want retry_state", readJSON)
	}
	readText, _, err := captureEnvOutput(t, func() error {
		return runDLQRead([]string{"--root", root, "--me", "alice", "--id", dlqID})
	})
	if err != nil || !strings.Contains(readText, "Retry State:    indeterminate") {
		t.Fatalf("legacy read text = %q err=%v, want indeterminate state", readText, err)
	}
}

func TestRunDLQListPrefersAuthoritativeCurOverDivergentNewResidue(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	newPath := moveInvalidFixtureToDLQForRetryAll(t, root, "alice", "list-cur-authority")
	filename := filepath.Base(newPath)
	staleNew, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read stale new fixture: %v", err)
	}
	deliveryRoot := openDeliveryRootForCLITest(t, root)
	if err := fsq.RetryFromDLQ(deliveryRoot, "alice", filename, false); err != nil {
		t.Fatalf("retry fixture into authoritative cur audit: %v", err)
	}
	if err := os.WriteFile(newPath, staleNew, 0o600); err != nil {
		t.Fatalf("recreate stale new residue: %v", err)
	}

	defaultJSON, _, err := captureEnvOutput(t, func() error {
		return runDLQList([]string{"--root", root, "--me", "alice", "--json"})
	})
	if err != nil {
		t.Fatalf("default dlq list: %v", err)
	}
	var defaultItems []dlqListItem
	if err := unmarshalJSONOutput(defaultJSON, &defaultItems); err != nil {
		t.Fatalf("decode default dlq list: %v (output: %s)", err, defaultJSON)
	}
	if len(defaultItems) != 1 || defaultItems[0].Box != fsq.BoxCur || defaultItems[0].RetryState != fsq.RetryStateDelivered {
		t.Fatalf("default divergent list = %#v, want one authoritative cur delivered item", defaultItems)
	}

	for _, test := range []struct {
		name  string
		flag  string
		box   string
		state string
	}{
		{name: "new physical filter", flag: "--new", box: fsq.BoxNew, state: fsq.RetryStateReady},
		{name: "cur physical filter", flag: "--cur", box: fsq.BoxCur, state: fsq.RetryStateDelivered},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, _, err := captureEnvOutput(t, func() error {
				return runDLQList([]string{"--root", root, "--me", "alice", test.flag, "--json"})
			})
			if err != nil {
				t.Fatalf("physical list %s: %v", test.flag, err)
			}
			var items []dlqListItem
			if err := unmarshalJSONOutput(output, &items); err != nil {
				t.Fatalf("decode physical list %s: %v (output: %s)", test.flag, err, output)
			}
			if len(items) != 1 || items[0].Box != test.box || items[0].RetryState != test.state {
				t.Fatalf("physical list %s = %#v, want %s %s", test.flag, items, test.box, test.state)
			}
		})
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
