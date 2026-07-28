package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func TestMonitor_ExistingMessages(t *testing.T) {
	root := t.TempDir()
	agent := "alice"

	// Initialize mailbox
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a test message with co-op fields
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       id,
			From:     "bob",
			To:       []string{agent},
			Thread:   "p2p/alice__bob",
			Subject:  "Test review",
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: format.PriorityUrgent,
			Kind:     format.KindReviewRequest,
			Labels:   []string{"test", "urgent"},
		},
		Body: "Please review this.",
	}
	data, _ := msg.Marshal()
	filename := id + ".md"
	if _, err := deliverToInboxesForTest(t, root, []string{agent}, filename, data); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Run monitor (should drain existing and exit)
	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--timeout", "1s",
		"--include-body",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	// Read output
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	// Parse JSON
	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	// Verify result
	if result.Event != "messages" {
		t.Errorf("expected event=messages, got %s", result.Event)
	}
	if result.WatchEvent != "existing" {
		t.Errorf("expected watch_event=existing, got %s", result.WatchEvent)
	}
	if result.Count != 1 {
		t.Errorf("expected count=1, got %d", result.Count)
	}
	if len(result.Drained) != 1 {
		t.Fatalf("expected 1 drained item, got %d", len(result.Drained))
	}

	item := result.Drained[0]
	if item.Priority != format.PriorityUrgent {
		t.Errorf("expected priority=urgent, got %s", item.Priority)
	}
	if item.Kind != format.KindReviewRequest {
		t.Errorf("expected kind=review_request, got %s", item.Kind)
	}
	if len(item.Labels) != 2 {
		t.Errorf("expected 2 labels, got %d", len(item.Labels))
	}
	if item.Body != "Please review this.\n" {
		t.Errorf("unexpected body: %q", item.Body)
	}

	// Verify message moved to cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, agent), filename)
	if _, err := os.Stat(curPath); os.IsNotExist(err) {
		t.Error("message not moved to cur")
	}

	receipts, err := receipt.List(root, agent, receipt.ListFilter{
		MsgID: id,
		Stage: receipt.StageDrained,
	})
	if err != nil {
		t.Fatalf("receipt.List: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("expected 1 drained receipt, got %d", len(receipts))
	}
}

func TestMonitor_PeekDoesNotDrain(t *testing.T) {
	root := t.TempDir()
	agent := "alice"

	// Initialize mailbox
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a test message
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    "bob",
			To:      []string{agent},
			Thread:  "p2p/alice__bob",
			Subject: "Peek test",
			Created: now.UTC().Format(time.RFC3339Nano),
		},
		Body: "Peek only.",
	}
	data, _ := msg.Marshal()
	filename := id + ".md"
	if _, err := deliverToInboxesForTest(t, root, []string{agent}, filename, data); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Run monitor in peek mode (should NOT drain)
	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--include-body",
		"--peek",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	// Read output
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	// Parse JSON
	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	if result.Event != "messages" {
		t.Errorf("expected event=messages, got %s", result.Event)
	}
	if result.Mode != "peek" {
		t.Errorf("expected mode=peek, got %s", result.Mode)
	}
	if len(result.Drained) != 1 {
		t.Fatalf("expected 1 item, got %d", len(result.Drained))
	}
	item := result.Drained[0]
	if item.MovedToCur {
		t.Errorf("expected moved_to_cur=false in peek mode")
	}
	// Verify message still in new
	newPath := filepath.Join(fsq.AgentInboxNew(root, agent), filename)
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		t.Error("message not found in new after peek")
	}

	// Verify message not moved to cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, agent), filename)
	if _, err := os.Stat(curPath); err == nil {
		t.Error("message should not be moved to cur in peek mode")
	}

	receipts, err := receipt.List(root, agent, receipt.ListFilter{MsgID: id})
	if err != nil {
		t.Fatalf("receipt.List: %v", err)
	}
	if len(receipts) != 0 {
		t.Fatalf("expected no receipts in peek mode, got %d", len(receipts))
	}
}

func TestMonitorPeekStopsBatchWhenContextChangesBetweenReads(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	for _, id := range []string{"peek-a", "peek-b", "peek-c"} {
		deliverGuardMessage(t, root, "alice", id)
	}

	checks := 0
	revalidateContext := func() error {
		checks++
		if checks == 3 {
			return ContextMismatchError("repo-local queue appeared")
		}
		return nil
	}

	items, err := monitorInboxItems(
		openDeliveryRootForCLITest(t, root),
		root,
		"alice",
		true,
		0,
		&headerValidator{},
		"peek",
		revalidateContext,
	)
	if err == nil || GetExitCode(err) != ExitContextMismatch {
		t.Fatalf("monitor peek error = %v, want context mismatch", err)
	}
	if len(items) != 0 {
		t.Fatalf("monitor peek returned foreign batch data after mismatch: %#v", items)
	}
	if checks != 3 {
		t.Fatalf("context checks = %d, want batch precheck plus one per attempted read", checks)
	}
	if remaining := inboxCount(t, root, "alice"); remaining != 3 {
		t.Fatalf("peek changed foreign inbox: %d messages remain, want 3", remaining)
	}
}

func TestMonitorPeekSkipsMessageClaimedAfterEnumeration(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	deliverGuardMessage(t, root, "alice", "a-claimed")
	deliverGuardMessage(t, root, "alice", "b-visible")

	checks := 0
	revalidateContext := func() error {
		checks++
		if checks == 2 {
			return fsq.MoveNewToCur(openDeliveryRootForCLITest(t, root), "alice", "a-claimed.md")
		}
		return nil
	}

	items, err := monitorInboxItems(
		openDeliveryRootForCLITest(t, root),
		root,
		"alice",
		true,
		0,
		&headerValidator{},
		"peek",
		revalidateContext,
	)
	if err != nil {
		t.Fatalf("peek after concurrent claim: %v", err)
	}
	if len(items) != 1 || items[0].ID != "b-visible" {
		t.Fatalf("peeked items = %#v, want only the still-new message", items)
	}
	if checks != 3 {
		t.Fatalf("context checks = %d, want pre-batch plus one per enumerated message", checks)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentInboxCur(root, "alice"), "a-claimed.md")); err != nil {
		t.Fatalf("concurrently claimed message is not in cur: %v", err)
	}
}

func TestMonitorDrainTreatsBatchAsOneAuthorizedTransaction(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	messageIDs := []string{"batch-a", "batch-b", "batch-c"}
	for _, id := range messageIDs {
		deliverGuardMessage(t, root, "alice", id)
	}

	// The second check models a repo-local queue appearing after the first
	// claim. Drain mode must not re-authorize inside a finite batch: doing so
	// would move the first message to cur and emit its receipt, then suppress
	// its payload when the second check refused the command.
	checks := 0
	revalidateContext := func() error {
		checks++
		if checks > 1 {
			return ContextMismatchError("repo-local queue appeared after the first claim")
		}
		return nil
	}

	items, err := monitorInboxItems(
		openDeliveryRootForCLITest(t, root),
		root,
		"alice",
		true,
		0,
		&headerValidator{},
		"drain",
		revalidateContext,
	)
	if err != nil {
		t.Fatalf("monitor drain batch: %v", err)
	}
	if checks != 1 {
		t.Fatalf("context checks = %d, want one authorization for the finite batch", checks)
	}
	if len(items) != len(messageIDs) {
		t.Fatalf("drained items = %d, want %d", len(items), len(messageIDs))
	}

	gotIDs := make(map[string]bool, len(items))
	for _, item := range items {
		gotIDs[item.ID] = true
		if !item.MovedToCur {
			t.Fatalf("drained item %q was not reported as moved to cur", item.ID)
		}
	}
	for _, id := range messageIDs {
		if !gotIDs[id] {
			t.Errorf("claimed payload %q was hidden from the monitor result", id)
		}
		receipts, err := receipt.List(root, "alice", receipt.ListFilter{
			MsgID: id,
			Stage: receipt.StageDrained,
		})
		if err != nil {
			t.Fatalf("list receipt for %s: %v", id, err)
		}
		if len(receipts) != 1 {
			t.Fatalf("drained receipts for %s = %d, want 1", id, len(receipts))
		}
	}
	if remaining := inboxCount(t, root, "alice"); remaining != 0 {
		t.Fatalf("new inbox still has %d message(s), want completed finite batch", remaining)
	}
	curEntries, err := os.ReadDir(fsq.AgentInboxCur(root, "alice"))
	if err != nil {
		t.Fatalf("read cur inbox: %v", err)
	}
	if len(curEntries) != len(messageIDs) {
		t.Fatalf("cur inbox has %d message(s), want %d", len(curEntries), len(messageIDs))
	}
}

func TestFinishMonitorCollectionOutputsCommittedClaimsBeforeReturningError(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	deliverGuardMessage(t, root, "alice", "a-complete")
	deliverGuardMessage(t, root, "alice", "b-interrupted")
	injectedErr := errors.New("injected post-claim failure")

	items, collectErr := drainInboxItemsWithClaimHook(
		openDeliveryRootForCLITest(t, root),
		root,
		"alice",
		true,
		0,
		&headerValidator{},
		func(claimed string) error {
			if claimed == "b-interrupted.md" {
				return injectedErr
			}
			return nil
		},
	)
	result := monitorResult{
		Event:      "messages",
		WatchEvent: "existing",
		Mode:       "drain",
		Me:         "alice",
		Count:      len(items),
		Drained:    items,
	}

	handled := false
	stdout, _, err := captureEnvOutput(t, func() error {
		var finishErr error
		handled, finishErr = finishMonitorCollection(true, root, result, collectErr)
		return finishErr
	})
	if !handled {
		t.Fatal("partial monitor collection was not handled")
	}
	if !errors.Is(err, injectedErr) {
		t.Fatalf("finish error = %v, want injected post-claim failure", err)
	}
	var output monitorResult
	if err := json.Unmarshal([]byte(stdout), &output); err != nil {
		t.Fatalf("unmarshal partial monitor output: %v (output: %s)", err, stdout)
	}
	if output.Count != 2 || len(output.Drained) != 2 {
		t.Fatalf("partial monitor output = %#v, want both committed claims", output)
	}
}

func TestFinishMonitorCollectionPreservesCommittedErrorThatWrapsNotExist(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	committed := &fsq.CommittedDurabilityError{
		FinalPath: filepath.Join(root, "agents", "alice", "inbox", "cur", "claimed.md"),
		Recipient: "alice",
		Err:       os.ErrNotExist,
	}

	handled, err := finishMonitorCollection(true, root, monitorResult{Me: "alice"}, committed)
	if !handled {
		t.Fatal("committed monitor error was not handled")
	}
	if err != committed {
		t.Fatalf("finish error = %T %v, want original committed error", err, err)
	}
}

func TestMonitor_Timeout(t *testing.T) {
	root := t.TempDir()
	agent := "alice"

	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Run monitor with short timeout (no messages)
	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--timeout", "100ms",
		"--poll",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	// Timeout now returns an error with ExitTimeout code
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if GetExitCode(err) != ExitTimeout {
		t.Errorf("expected ExitTimeout (%d), got %d", ExitTimeout, GetExitCode(err))
	}

	// Read output
	var buf [4096]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	// Parse JSON
	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	// Verify timeout result
	if result.Event != "timeout" {
		t.Errorf("expected event=timeout, got %s", result.Event)
	}
	if result.Count != 0 {
		t.Errorf("expected count=0, got %d", result.Count)
	}
}

func TestRunMonitorRejectsReplacementRootWhileIdle(t *testing.T) {
	for _, poll := range []bool{true, false} {
		name := "fsnotify"
		if poll {
			name = "poll"
		}
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			active := filepath.Join(parent, "active")
			replacement := filepath.Join(parent, "replacement")
			displaced := filepath.Join(parent, "displaced")
			for _, root := range []string{active, replacement} {
				if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
					t.Fatalf("EnsureAgentDirs(%s): %v", root, err)
				}
			}

			ready := make(chan struct{})
			resume := make(chan struct{})
			oldIdleHook := monitorIdleForTest
			monitorIdleForTest = func() {
				close(ready)
				<-resume
			}
			t.Cleanup(func() { monitorIdleForTest = oldIdleHook })

			oldStdout := os.Stdout
			stdoutReader, stdoutWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = stdoutWriter
			t.Cleanup(func() { os.Stdout = oldStdout })

			args := []string{
				"--root", active,
				"--me", "alice",
				"--json",
				"--timeout", "2s",
			}
			if poll {
				args = append(args, "--poll")
			}
			errCh := make(chan error, 1)
			go func() { errCh <- runMonitor(args) }()

			select {
			case <-ready:
			case monitorErr := <-errCh:
				t.Fatalf("monitor returned before idle swap: %v", monitorErr)
			case <-time.After(2 * time.Second):
				t.Fatal("monitor did not finish its initial empty scan")
			}
			if err := os.Rename(active, displaced); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, active); err != nil {
				t.Fatal(err)
			}
			close(resume)

			var monitorErr error
			select {
			case monitorErr = <-errCh:
			case <-time.After(3 * time.Second):
				t.Fatal("monitor did not detect replacement root")
			}
			_ = stdoutWriter.Close()
			os.Stdout = oldStdout
			var stdout bytes.Buffer
			_, _ = stdout.ReadFrom(stdoutReader)

			if monitorErr == nil || !strings.Contains(monitorErr.Error(), "delivery root changed after authorization") {
				t.Fatalf("monitor error = %v, want replacement-root refusal (stdout=%s)", monitorErr, stdout.String())
			}
			if strings.Contains(stdout.String(), `"event": "timeout"`) {
				t.Fatalf("monitor emitted timeout after root replacement: %s", stdout.String())
			}
		})
	}
}

func TestMonitor_SessionInJSON(t *testing.T) {
	// Create a session-like layout: base/.agent-mail/mysession/agents/alice/...
	base := t.TempDir()
	baseRoot := filepath.Join(base, ".agent-mail")
	sessionRoot := filepath.Join(baseRoot, "mysession")
	agent := "alice"

	// Create the session root with agent dirs
	if err := fsq.EnsureAgentDirs(sessionRoot, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create a sibling session so classifyRoot detects the session layout
	siblingSession := filepath.Join(baseRoot, "othersession")
	if err := fsq.EnsureAgentDirs(siblingSession, "bob"); err != nil {
		t.Fatalf("EnsureAgentDirs sibling: %v", err)
	}

	// Deliver a message
	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    "bob",
			To:      []string{agent},
			Subject: "Session test",
			Created: now.UTC().Format(time.RFC3339Nano),
		},
		Body: "Test message in session.",
	}
	data, _ := msg.Marshal()
	if _, err := deliverToInboxesForTest(t, sessionRoot, []string{agent}, id+".md", data); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runMonitor([]string{
		"--me", agent,
		"--root", sessionRoot,
		"--json",
		"--timeout", "1s",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	if result.Session != "mysession" {
		t.Errorf("expected session=mysession, got %q", result.Session)
	}
}

func TestMonitor_NoSessionForPlainRoot(t *testing.T) {
	// A plain root (not under .agent-mail or any session layout) should have empty session
	root := t.TempDir()
	agent := "alice"

	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	now := time.Now()
	id, _ := format.NewMessageID(now)
	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    "bob",
			To:      []string{agent},
			Subject: "No session",
			Created: now.UTC().Format(time.RFC3339Nano),
		},
		Body: "Test.",
	}
	data, _ := msg.Marshal()
	if _, err := deliverToInboxesForTest(t, root, []string{agent}, id+".md", data); err != nil {
		t.Fatalf("DeliverToInboxes: %v", err)
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--timeout", "1s",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v\noutput: %s", err, output)
	}

	if result.Session != "" {
		t.Errorf("expected empty session for plain root, got %q", result.Session)
	}
}

func TestMonitor_PriorityInOutput(t *testing.T) {
	root := t.TempDir()
	agent := "alice"

	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	// Create messages with different priorities
	priorities := []string{format.PriorityLow, format.PriorityNormal, format.PriorityUrgent}
	for i, p := range priorities {
		now := time.Now().Add(time.Duration(i) * time.Second)
		id, _ := format.NewMessageID(now)
		msg := format.Message{
			Header: format.Header{
				Schema:   format.CurrentSchema,
				ID:       id,
				From:     "bob",
				To:       []string{agent},
				Thread:   "p2p/alice__bob",
				Subject:  "Priority " + p,
				Created:  now.UTC().Format(time.RFC3339Nano),
				Priority: p,
			},
			Body: "Test",
		}
		data, _ := msg.Marshal()
		_, _ = deliverToInboxesForTest(t, root, []string{agent}, id+".md", data)
	}

	// Capture output
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runMonitor([]string{
		"--me", agent,
		"--root", root,
		"--json",
		"--timeout", "1s",
	})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runMonitor: %v", err)
	}

	var buf [8192]byte
	n, _ := r.Read(buf[:])
	output := string(buf[:n])

	var result monitorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse output: %v", err)
	}

	if result.Count != 3 {
		t.Errorf("expected 3 messages, got %d", result.Count)
	}

	// Verify all priorities present
	foundPriorities := make(map[string]bool)
	for _, item := range result.Drained {
		foundPriorities[item.Priority] = true
	}
	for _, p := range priorities {
		if !foundPriorities[p] {
			t.Errorf("priority %s not found in output", p)
		}
	}
}
