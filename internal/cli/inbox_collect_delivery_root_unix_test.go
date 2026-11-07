//go:build !windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestDrainInboxItemsRefusesOpenedRootPathReplacement(t *testing.T) {
	parent := t.TempDir()
	authorizedRoot := filepath.Join(parent, "authorized")
	replacementRoot := filepath.Join(parent, "replacement")
	parkedRoot := filepath.Join(parent, "authorized-parked")
	const filename = "same-name.md"

	ensureInboxCollectFixtureRoot(t, authorizedRoot)
	ensureInboxCollectFixtureRoot(t, replacementRoot)
	deliverInboxCollectFixture(t, authorizedRoot, filename, "authorized-id", "authorized body")
	deliverInboxCollectFixture(t, replacementRoot, filename, "replacement-new-id", "replacement new body")
	writeInboxCollectFixture(
		t,
		filepath.Join(fsq.AgentInboxCur(replacementRoot, "alice"), filename),
		"replacement-cur-id",
		"replacement cur body",
	)

	authorized := openDeliveryRootForCLITest(t, authorizedRoot)
	if err := os.Rename(authorizedRoot, parkedRoot); err != nil {
		t.Fatalf("park authorized root: %v", err)
	}
	if err := os.Rename(replacementRoot, authorizedRoot); err != nil {
		t.Fatalf("replace authorized root path: %v", err)
	}

	items, err := drainInboxItems(
		authorized,
		authorizedRoot,
		"alice",
		true,
		0,
		&headerValidator{},
	)
	if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
		t.Fatalf("drain after root replacement error = %v, want delivery-root refusal", err)
	}
	if len(items) != 0 {
		t.Fatalf("refused drain returned contents from a replacement tree: %#v", items)
	}

	assertInboxCollectFixtureState(t, parkedRoot, filename, "new")
	assertInboxCollectFixtureState(t, authorizedRoot, filename, "new", "cur")
	assertNoInboxCollectOutcome(t, parkedRoot)
	assertNoInboxCollectOutcome(t, authorizedRoot)
}

func TestDrainInboxItemsFinishesClaimedMessageAfterRootPathReplacement(t *testing.T) {
	parent := t.TempDir()
	authorizedRoot := filepath.Join(parent, "authorized")
	replacementRoot := filepath.Join(parent, "replacement")
	parkedRoot := filepath.Join(parent, "authorized-parked")
	const filename = "same-name.md"

	ensureInboxCollectFixtureRoot(t, authorizedRoot)
	ensureInboxCollectFixtureRoot(t, replacementRoot)
	deliverInboxCollectFixture(t, authorizedRoot, filename, "authorized-id", "authorized body")
	deliverInboxCollectFixture(t, replacementRoot, filename, "replacement-new-id", "replacement new body")
	writeInboxCollectFixture(
		t,
		filepath.Join(fsq.AgentInboxCur(replacementRoot, "alice"), filename),
		"replacement-cur-id",
		"replacement cur body",
	)

	swapped := false
	items, err := drainInboxItemsWithClaimHook(
		openDeliveryRootForCLITest(t, authorizedRoot),
		authorizedRoot,
		"alice",
		true,
		0,
		&headerValidator{},
		func(claimed string) error {
			if claimed != filename {
				t.Fatalf("claimed filename = %q, want %q", claimed, filename)
			}
			if err := os.Rename(authorizedRoot, parkedRoot); err != nil {
				return err
			}
			if err := os.Rename(replacementRoot, authorizedRoot); err != nil {
				return err
			}
			swapped = true
			return nil
		},
	)
	if err != nil {
		t.Fatalf("drain after post-claim root replacement: %v", err)
	}
	if !swapped {
		t.Fatal("post-claim root replacement hook did not run")
	}
	if len(items) != 1 {
		t.Fatalf("drained items = %d, want the one already-claimed message", len(items))
	}
	if item := items[0]; item.ID != "authorized-id" ||
		item.Body != "authorized body\n" ||
		!item.MovedToCur ||
		item.MovedToDLQ ||
		item.ParseError != "" {
		t.Fatalf("drain returned anything except the authorized claimed payload: %#v", item)
	}

	assertInboxCollectFixtureState(t, parkedRoot, filename, "cur")
	assertInboxCollectFixtureState(t, authorizedRoot, filename, "new", "cur")
	assertNoInboxCollectOutcome(t, authorizedRoot)
	assertNoInboxCollectDLQ(t, parkedRoot)
	receipts, err := receipt.List(parkedRoot, "alice", receipt.ListFilter{
		MsgID: "authorized-id",
		Stage: receipt.StageDrained,
	})
	if err != nil {
		t.Fatalf("list authorized receipt: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("authorized drained receipts = %d, want 1", len(receipts))
	}
}

func TestDrainInboxItemsReportsEarlierClaimWhenLaterClaimCannotBeRead(t *testing.T) {
	root := t.TempDir()
	const (
		firstFilename  = "a-first.md"
		secondFilename = "b-unreadable.md"
	)

	ensureInboxCollectFixtureRoot(t, root)
	deliverInboxCollectFixture(t, root, firstFilename, "first-id", "first body")
	deliverInboxCollectFixture(t, root, secondFilename, "second-id", "second body")
	outsidePath := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outsidePath, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside symlink target: %v", err)
	}

	items, err := drainInboxItemsWithClaimHook(
		openDeliveryRootForCLITest(t, root),
		root,
		"alice",
		true,
		0,
		&headerValidator{},
		func(claimed string) error {
			if claimed != secondFilename {
				return nil
			}
			claimedPath := filepath.Join(fsq.AgentInboxCur(root, "alice"), claimed)
			if err := os.Remove(claimedPath); err != nil {
				return err
			}
			return os.Symlink(outsidePath, claimedPath)
		},
	)
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("drain with later claimed read failure error = %v, want ordinary DLQ placement failure", err)
	}
	if len(items) != 2 {
		t.Fatalf("drained items = %d, want both claimed outcomes: %#v", len(items), items)
	}
	itemsByID := make(map[string]inboxItem, len(items))
	for _, item := range items {
		itemsByID[item.ID] = item
	}
	if item := itemsByID["first-id"]; item.ID != "first-id" ||
		item.Body != "first body\n" ||
		!item.MovedToCur ||
		item.ParseError != "" {
		t.Fatalf("first claimed payload was hidden or changed: %#v", item)
	}
	if item := itemsByID[strings.TrimSuffix(secondFilename, ".md")]; item.ID != strings.TrimSuffix(secondFilename, ".md") ||
		item.ParseError == "" ||
		item.FailureReason != "parse_error" ||
		item.MovedToCur ||
		item.MovedToDLQ {
		t.Fatalf("later unreadable claim was not reported as a retained parse failure: %#v", item)
	}

	receipts, err := receipt.List(root, "alice", receipt.ListFilter{
		MsgID: "first-id",
		Stage: receipt.StageDrained,
	})
	if err != nil {
		t.Fatalf("list first receipt: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("first drained receipts = %d, want 1", len(receipts))
	}
	assertInboxCollectFixtureState(t, root, firstFilename, "cur")
	assertInboxCollectFixtureState(t, root, secondFilename, "cur")
}

// Review of receipt recovery: a drained receipt that failed to write was only
// a warning, and no later drain could write it, so `send --wait-for drained`
// timed out although the message was consumed. A receipt fault must not stop
// the batch either: every waiting message is delivered.
func TestSendWaitForDrainedCompletesAfterReceiptWriteFailsOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := filepath.Join(t.TempDir(), "queue")
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs: %v", err)
		}
	}
	configureSendTestRoot(t, root, "alice", "bob")
	for _, key := range []string{envRoot, envBaseRoot, envSession} {
		setOptionalEnv(t, key, "", false)
	}
	deliveryRoot := openDeliveryRootForCLITest(t, root)
	deliverInboxCollectFixture(t, root, "0-earlier.md", "earlier", "earlier mail")
	receiptsDir := fsq.AgentReceipts(root, "alice")
	t.Cleanup(func() { _ = os.Chmod(receiptsDir, 0o700) })

	consumer := make(chan []string, 1)
	_, _, err := captureEnvOutput(t, func() error {
		go func() {
			var failures []string
			defer func() { consumer <- failures }()
			newDir := fsq.AgentInboxNew(root, "alice")
			for deadline := time.Now().Add(4 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if entries, _ := os.ReadDir(newDir); len(entries) >= 2 {
					break
				}
				if time.Now().After(deadline) {
					failures = append(failures, "message never arrived")
					return
				}
			}
			if err := os.Chmod(receiptsDir, 0o500); err != nil {
				failures = append(failures, err.Error())
				return
			}
			items, err := drainInboxItems(deliveryRoot, root, "alice", true, 0, &headerValidator{})
			if err == nil || len(items) != 2 {
				failures = append(failures, fmt.Sprintf("drain with unwritable receipts = %d items, %v; want both payloads and an error", len(items), err))
			}
			if err := os.Chmod(receiptsDir, 0o700); err != nil {
				failures = append(failures, err.Error())
				return
			}
			items, err = drainInboxItems(deliveryRoot, root, "alice", true, 0, &headerValidator{})
			if err != nil || len(items) != 0 {
				failures = append(failures, fmt.Sprintf("repairing drain = %d items, %v; want no redelivery and no error", len(items), err))
			}
		}()
		return runSend([]string{
			"--root", root,
			"--me", "bob",
			"--to", "alice",
			"--body", "consume me",
			"--wait-for", "drained",
			"--wait-timeout", "4s",
		})
	})
	failures := <-consumer
	if err != nil {
		failures = append(failures, fmt.Sprintf("send --wait-for drained: %v", err))
	}
	if len(failures) > 0 {
		t.Fatal(strings.Join(failures, "\n"))
	}
}

func ensureInboxCollectFixtureRoot(t *testing.T, root string) {
	t.Helper()
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs(%s): %v", root, err)
	}
}

func deliverInboxCollectFixture(t *testing.T, root, filename, id, body string) {
	t.Helper()
	data := marshalInboxCollectFixture(t, id, body)
	if _, err := fsq.DeliverToInbox(openDeliveryRootForCLITest(t, root), "alice", filename, data); err != nil {
		t.Fatalf("DeliverToInbox(%s): %v", root, err)
	}
}

func writeInboxCollectFixture(t *testing.T, path, id, body string) {
	t.Helper()
	if err := os.WriteFile(path, marshalInboxCollectFixture(t, id, body), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func marshalInboxCollectFixture(t *testing.T, id, body string) []byte {
	t.Helper()
	data, err := (format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: id,
			Created: "2026-07-28T00:00:00Z",
		},
		Body: body,
	}).Marshal()
	if err != nil {
		t.Fatalf("marshal %s: %v", id, err)
	}
	return data
}

func assertInboxCollectFixtureState(t *testing.T, root, filename string, wantBoxes ...string) {
	t.Helper()
	want := make(map[string]bool, len(wantBoxes))
	for _, box := range wantBoxes {
		want[box] = true
	}
	for _, box := range []string{"new", "cur"} {
		path := filepath.Join(root, "agents", "alice", "inbox", box, filename)
		_, err := os.Stat(path)
		if want[box] {
			if err != nil {
				t.Fatalf("wanted fixture in %s: %v", path, err)
			}
			continue
		}
		if !os.IsNotExist(err) {
			t.Fatalf("unexpected fixture state at %s: %v", path, err)
		}
	}
}

func assertNoInboxCollectOutcome(t *testing.T, root string) {
	t.Helper()
	assertNoInboxCollectDLQ(t, root)
	for _, dir := range []string{fsq.AgentReceipts(root, "alice")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", dir, err)
		}
		if len(entries) != 0 {
			t.Fatalf("unexpected drain outcome in %s: %v", dir, entries)
		}
	}
	receipts, err := receipt.List(root, "alice", receipt.ListFilter{})
	if err != nil {
		t.Fatalf("receipt.List(%s): %v", root, err)
	}
	if len(receipts) != 0 {
		t.Fatalf("unexpected receipts at %s: %#v", root, receipts)
	}
}

func assertNoInboxCollectDLQ(t *testing.T, root string) {
	t.Helper()
	dir := fsq.AgentDLQNew(root, "alice")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("unexpected DLQ outcome in %s: %v", dir, entries)
	}
}
