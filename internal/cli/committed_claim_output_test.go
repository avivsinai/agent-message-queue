package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestRunDrainOutputsCommittedClaimBeforeDurabilityError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	deliverCommittedClaimFixture(t, root, "alice", "a-later", "2026-07-28T02:00:00Z")
	deliverCommittedClaimFixture(t, root, "alice", "b-earlier", "2026-07-28T01:00:00Z")
	injectedErr := installCommittedClaimAfterMove(t, "b-earlier.md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{
			"--root", root,
			"--me", "alice",
			"--include-body",
			"--json",
		})
	})
	assertCommittedClaimError(t, err, injectedErr)

	var result drainResult
	if err := unmarshalJSONOutput(stdout, &result); err != nil {
		t.Fatalf("unmarshal drain output: %v (output: %s)", err, stdout)
	}
	assertCommittedDrainItems(t, result.Drained, result.Count)
	assertCommittedClaimOutcomes(t, root)
}

func TestRunMonitorInitialOutputsCommittedClaimBeforeDurabilityError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	deliverCommittedClaimFixture(t, root, "alice", "a-later", "2026-07-28T02:00:00Z")
	deliverCommittedClaimFixture(t, root, "alice", "b-earlier", "2026-07-28T01:00:00Z")
	injectedErr := installCommittedClaimAfterMove(t, "b-earlier.md")

	stdout, _, err := captureEnvOutput(t, func() error {
		return runMonitor([]string{
			"--root", root,
			"--me", "alice",
			"--include-body",
			"--json",
			"--poll",
			"--timeout", "3s",
		})
	})
	assertCommittedClaimError(t, err, injectedErr)

	var result monitorResult
	if err := unmarshalJSONOutput(stdout, &result); err != nil {
		t.Fatalf("unmarshal initial monitor output: %v (output: %s)", err, stdout)
	}
	if result.Event != "messages" || result.WatchEvent != "existing" {
		t.Fatalf("initial monitor event = (%q, %q), want (messages, existing)", result.Event, result.WatchEvent)
	}
	assertCommittedDrainItems(t, result.Drained, result.Count)
	assertCommittedClaimOutcomes(t, root)
}

func TestRunMonitorPostWatchOutputsCommittedClaimBeforeDurabilityError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	injectedErr := installCommittedClaimAfterMove(t, "b-earlier.md")

	idle := make(chan struct{})
	release := make(chan struct{})
	oldIdleHook := monitorPollingIdleForTest
	monitorPollingIdleForTest = func() {
		close(idle)
		<-release
	}
	t.Cleanup(func() { monitorPollingIdleForTest = oldIdleHook })

	deliveryErr := make(chan error, 1)
	go func() {
		<-idle
		err := deliverCommittedClaimFixtureError(root, "alice", "a-later", "2026-07-28T02:00:00Z")
		if err == nil {
			err = deliverCommittedClaimFixtureError(root, "alice", "b-earlier", "2026-07-28T01:00:00Z")
		}
		close(release)
		deliveryErr <- err
	}()

	stdout, _, err := captureEnvOutput(t, func() error {
		return runMonitor([]string{
			"--root", root,
			"--me", "alice",
			"--include-body",
			"--json",
			"--poll",
			"--timeout", "3s",
		})
	})
	if deliveryErr := <-deliveryErr; deliveryErr != nil {
		t.Fatalf("deliver post-watch fixtures: %v", deliveryErr)
	}
	assertCommittedClaimError(t, err, injectedErr)

	var result monitorResult
	if err := unmarshalJSONOutput(stdout, &result); err != nil {
		t.Fatalf("unmarshal post-watch monitor output: %v (output: %s)", err, stdout)
	}
	if result.Event != "messages" || result.WatchEvent != "new_message" {
		t.Fatalf("post-watch monitor event = (%q, %q), want (messages, new_message)", result.Event, result.WatchEvent)
	}
	assertCommittedDrainItems(t, result.Drained, result.Count)
	assertCommittedClaimOutcomes(t, root)
}

func installCommittedClaimAfterMove(t *testing.T, targetFilename string) error {
	t.Helper()
	injectedErr := errors.New("injected post-rename claim sync failure")
	oldClaim := claimInboxNewToCur
	claimInboxNewToCur = func(root *fsq.DeliveryRoot, agent, filename string) error {
		if err := fsq.MoveNewToCur(root, agent, filename); err != nil {
			return err
		}
		if filename != targetFilename {
			return nil
		}
		return &fsq.CommittedDurabilityError{
			FinalPath: root.DisplayPath(filepath.Join("agents", agent, "inbox", "cur", filename)),
			Recipient: agent,
			Err:       injectedErr,
		}
	}
	t.Cleanup(func() { claimInboxNewToCur = oldClaim })
	return injectedErr
}

func deliverCommittedClaimFixture(t *testing.T, root, agent, id, created string) {
	t.Helper()
	if err := deliverCommittedClaimFixtureError(root, agent, id, created); err != nil {
		t.Fatalf("deliver %s: %v", id, err)
	}
}

func deliverCommittedClaimFixtureError(root, agent, id, created string) error {
	data, err := (format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    "bob",
			To:      []string{agent},
			Thread:  "p2p/alice__bob",
			Subject: id,
			Created: created,
		},
		Body: "body for " + id,
	}).Marshal()
	if err != nil {
		return err
	}
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()
	_, err = fsq.DeliverToInbox(deliveryRoot, agent, id+".md", data)
	return err
}

func assertCommittedClaimError(t *testing.T, err, injectedErr error) {
	t.Helper()
	var committed *fsq.CommittedDurabilityError
	if !errors.As(err, &committed) {
		t.Fatalf("command error = %T %v, want CommittedDurabilityError", err, err)
	}
	if !errors.Is(err, injectedErr) {
		t.Fatalf("command error = %v, want injected durability error", err)
	}
}

func assertCommittedDrainItems(t *testing.T, items []inboxItem, count int) {
	t.Helper()
	if count != 2 || len(items) != 2 {
		t.Fatalf("partial output count = %d items = %#v, want 2", count, items)
	}
	wantIDs := []string{"b-earlier", "a-later"}
	for i, wantID := range wantIDs {
		item := items[i]
		if item.ID != wantID {
			t.Fatalf("partial output order[%d] = %q, want %q", i, item.ID, wantID)
		}
		if !item.MovedToCur || item.MovedToDLQ || item.ParseError != "" {
			t.Fatalf("claimed item %q outcome = %#v, want valid drained payload", wantID, item)
		}
		if item.Body != "body for "+wantID+"\n" {
			t.Fatalf("claimed item %q body = %q", wantID, item.Body)
		}
	}
}

func assertCommittedClaimOutcomes(t *testing.T, root string) {
	t.Helper()
	for _, id := range []string{"a-later", "b-earlier"} {
		if _, err := os.Stat(filepath.Join(fsq.AgentInboxCur(root, "alice"), id+".md")); err != nil {
			t.Fatalf("claimed payload %q is not in cur: %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "alice"), id+".md")); !os.IsNotExist(err) {
			t.Fatalf("claimed payload %q remains in new: %v", id, err)
		}
		receipts, err := receipt.List(root, "alice", receipt.ListFilter{
			MsgID: id,
			Stage: receipt.StageDrained,
		})
		if err != nil {
			t.Fatalf("list drained receipt for %q: %v", id, err)
		}
		if len(receipts) != 1 {
			t.Fatalf("drained receipts for %q = %d, want 1", id, len(receipts))
		}
	}
}

func unmarshalJSONOutput(output string, target any) error {
	if output == "" {
		return fmt.Errorf("empty JSON output")
	}
	return json.Unmarshal([]byte(output), target)
}
