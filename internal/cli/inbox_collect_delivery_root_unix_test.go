//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestMonitorPeekRefusesOpenedRootPathReplacement(t *testing.T) {
	parent := t.TempDir()
	authorizedRoot := filepath.Join(parent, "authorized")
	replacementRoot := filepath.Join(parent, "replacement")
	parkedRoot := filepath.Join(parent, "authorized-parked")
	const filename = "same-name.md"

	ensureInboxCollectFixtureRoot(t, authorizedRoot)
	ensureInboxCollectFixtureRoot(t, replacementRoot)
	deliverInboxCollectFixture(t, authorizedRoot, filename, "authorized-id", "authorized body")
	deliverInboxCollectFixture(t, replacementRoot, filename, "replacement-id", "replacement body")

	authorized := openDeliveryRootForCLITest(t, authorizedRoot)
	if err := os.Rename(authorizedRoot, parkedRoot); err != nil {
		t.Fatalf("park authorized root: %v", err)
	}
	if err := os.Rename(replacementRoot, authorizedRoot); err != nil {
		t.Fatalf("replace authorized root path: %v", err)
	}

	items, err := monitorInboxItems(
		authorized,
		authorizedRoot,
		"alice",
		true,
		0,
		&headerValidator{},
		"peek",
		func() error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
		t.Fatalf("monitor peek after root replacement error = %v, want delivery-root refusal", err)
	}
	if len(items) != 0 {
		t.Fatalf("refused monitor peek returned replacement contents: %#v", items)
	}

	assertInboxCollectFixtureState(t, parkedRoot, filename, "new")
	assertInboxCollectFixtureState(t, authorizedRoot, filename, "new")
	assertNoInboxCollectOutcome(t, parkedRoot)
	assertNoInboxCollectOutcome(t, authorizedRoot)
}

func TestMonitorPeekReadsAuthorizedTreeWhenRootChangesAfterEnumeration(t *testing.T) {
	parent := t.TempDir()
	authorizedRoot := filepath.Join(parent, "authorized")
	replacementRoot := filepath.Join(parent, "replacement")
	parkedRoot := filepath.Join(parent, "authorized-parked")
	const filename = "same-name.md"

	ensureInboxCollectFixtureRoot(t, authorizedRoot)
	ensureInboxCollectFixtureRoot(t, replacementRoot)
	deliverInboxCollectFixture(t, authorizedRoot, filename, "authorized-id", "authorized body")
	deliverInboxCollectFixture(t, replacementRoot, filename, "replacement-id", "replacement body")

	checks := 0
	items, err := monitorInboxItems(
		openDeliveryRootForCLITest(t, authorizedRoot),
		authorizedRoot,
		"alice",
		true,
		0,
		&headerValidator{},
		"peek",
		func() error {
			checks++
			if checks != 2 {
				return nil
			}
			if err := os.Rename(authorizedRoot, parkedRoot); err != nil {
				return err
			}
			return os.Rename(replacementRoot, authorizedRoot)
		},
	)
	if err != nil {
		t.Fatalf("monitor peek after post-enumeration root replacement: %v", err)
	}
	if checks != 2 {
		t.Fatalf("context checks = %d, want pre-batch and pre-read checks", checks)
	}
	if len(items) != 1 {
		t.Fatalf("peeked items = %d, want one authorized item", len(items))
	}
	if item := items[0]; item.ID != "authorized-id" || item.Body != "authorized body\n" {
		t.Fatalf("monitor peek returned replacement bytes: %#v", item)
	}

	assertInboxCollectFixtureState(t, parkedRoot, filename, "new")
	assertInboxCollectFixtureState(t, authorizedRoot, filename, "new")
	assertNoInboxCollectOutcome(t, parkedRoot)
	assertNoInboxCollectOutcome(t, authorizedRoot)
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
