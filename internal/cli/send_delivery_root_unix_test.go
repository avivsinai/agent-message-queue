//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestSendRejectsSymlinkSwapAfterGuard(t *testing.T) {
	parent := secureTempDirForTest(t)
	authorizedAlias := filepath.Join(parent, "authorized")
	authorizedMoved := filepath.Join(parent, "authorized-moved")
	outside := filepath.Join(parent, "outside")
	for _, root := range []string{authorizedAlias, outside} {
		for _, agent := range []string{"alice", "bob"} {
			if err := fsq.EnsureAgentDirs(root, agent); err != nil {
				t.Fatalf("EnsureAgentDirs(%s,%s): %v", root, agent, err)
			}
		}
	}
	clearDeliveryRootTestEnv(t)

	bodyFIFO := filepath.Join(parent, "body.fifo")
	if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	type openResult struct {
		file *os.File
		err  error
	}
	writerCh := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
		writerCh <- openResult{file: file, err: err}
	}()
	t.Cleanup(func() {
		// Release the blocked writer if runSend returns before opening the FIFO.
		reader, err := os.OpenFile(bodyFIFO, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = reader.Close()
		}
	})
	errCh := make(chan error, 1)
	go func() {
		errCh <- runSend([]string{
			"--root", authorizedAlias,
			"--me", "alice",
			"--to", "bob",
			"--body", "@" + bodyFIFO,
		})
	}()

	var writer *os.File
	select {
	case result := <-writerCh:
		if result.err != nil {
			t.Fatalf("open FIFO writer: %v", result.err)
		}
		writer = result.file
	case err := <-errCh:
		t.Fatalf("send returned before reading body: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("send did not reach post-authorization body read")
	}
	if err := os.Rename(authorizedAlias, authorizedMoved); err != nil {
		t.Fatalf("move authorized root: %v", err)
	}
	if err := os.Symlink(outside, authorizedAlias); err != nil {
		t.Fatalf("replace authorized alias: %v", err)
	}
	if _, err := writer.Write([]byte("after authorization")); err != nil {
		t.Fatalf("write FIFO body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close FIFO writer: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
			t.Fatalf("send error = %v, want post-authorization root-change refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send did not return after body was released")
	}
	if entries, err := os.ReadDir(fsq.AgentInboxNew(outside, "bob")); err != nil {
		t.Fatalf("ReadDir outside inbox: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("escaped delivery wrote %d message(s) outside authorized root", len(entries))
	}
	if entries, err := os.ReadDir(fsq.AgentInboxNew(authorizedMoved, "bob")); err != nil {
		t.Fatalf("ReadDir original inbox: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("refused send wrote %d message(s) into moved authorized root", len(entries))
	}
}

func TestSendRejectsEscapingMailboxSymlink(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "authorized")
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	outsideInbox := filepath.Join(parent, "outside-inbox")
	for _, box := range []string{"tmp", "new"} {
		if err := os.MkdirAll(filepath.Join(outsideInbox, box), 0o700); err != nil {
			t.Fatalf("mkdir outside inbox: %v", err)
		}
	}
	bobInbox := filepath.Join(root, "agents", "bob", "inbox")
	if err := os.RemoveAll(bobInbox); err != nil {
		t.Fatalf("remove bob inbox: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "..", "outside-inbox"), bobInbox); err != nil {
		t.Fatalf("symlink escaping inbox: %v", err)
	}
	clearDeliveryRootTestEnv(t)

	err := runSend([]string{
		"--root", root,
		"--me", "alice",
		"--to", "bob",
		"--body", "must stay in root",
	})
	if err == nil {
		t.Fatal("send through an escaping mailbox symlink succeeded")
	}
	if entries, readErr := os.ReadDir(filepath.Join(outsideInbox, "new")); readErr != nil {
		t.Fatalf("ReadDir outside inbox: %v", readErr)
	} else if len(entries) != 0 {
		t.Fatalf("escaping mailbox symlink received %d message(s)", len(entries))
	}
}

func TestReplyRejectsSymlinkSwapAfterGuard(t *testing.T) {
	parent := secureTempDirForTest(t)
	authorizedAlias := filepath.Join(parent, "authorized")
	authorizedMoved := filepath.Join(parent, "authorized-moved")
	outside := filepath.Join(parent, "outside")
	for _, root := range []string{authorizedAlias, outside} {
		for _, agent := range []string{"alice", "bob"} {
			if err := fsq.EnsureAgentDirs(root, agent); err != nil {
				t.Fatalf("EnsureAgentDirs(%s,%s): %v", root, agent, err)
			}
		}
	}
	now := time.Now()
	originalID, err := format.NewMessageID(now)
	if err != nil {
		t.Fatalf("NewMessageID: %v", err)
	}
	original := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      originalID,
			From:    "bob",
			To:      []string{"alice"},
			Thread:  "p2p/alice__bob",
			Subject: "root swap",
			Created: now.UTC().Format(time.RFC3339Nano),
		},
		Body: "authorize before replying",
	}
	data, err := original.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := deliverToInboxForTest(t, authorizedAlias, "alice", originalID+".md", data); err != nil {
		t.Fatalf("deliver original: %v", err)
	}
	clearDeliveryRootTestEnv(t)

	bodyFIFO := filepath.Join(parent, "reply-body.fifo")
	if err := syscall.Mkfifo(bodyFIFO, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	type openResult struct {
		file *os.File
		err  error
	}
	writerCh := make(chan openResult, 1)
	go func() {
		file, err := os.OpenFile(bodyFIFO, os.O_WRONLY, 0)
		writerCh <- openResult{file: file, err: err}
	}()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runReply([]string{
			"--root", authorizedAlias,
			"--me", "alice",
			"--id", originalID,
			"--body", "@" + bodyFIFO,
		})
	}()

	var writer *os.File
	select {
	case result := <-writerCh:
		if result.err != nil {
			t.Fatalf("open FIFO writer: %v", result.err)
		}
		writer = result.file
	case err := <-errCh:
		t.Fatalf("reply returned before reading body: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("reply did not reach post-authorization body read")
	}
	if err := os.Rename(authorizedAlias, authorizedMoved); err != nil {
		t.Fatalf("move authorized root: %v", err)
	}
	if err := os.Symlink(outside, authorizedAlias); err != nil {
		t.Fatalf("replace authorized alias: %v", err)
	}
	if _, err := writer.Write([]byte("after authorization")); err != nil {
		t.Fatalf("write FIFO body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close FIFO writer: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
			t.Fatalf("reply error = %v, want post-authorization root-change refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reply did not return after body was released")
	}
	if entries, err := os.ReadDir(fsq.AgentInboxNew(outside, "bob")); err != nil {
		t.Fatalf("ReadDir outside inbox: %v", err)
	} else if len(entries) != 0 {
		t.Fatalf("escaped reply wrote %d message(s) outside authorized root", len(entries))
	}
}

func TestSendAllowsInRootRelativeMailboxSymlink(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "authorized")
	for _, agent := range []string{"alice", "bob"} {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	inRootInbox := filepath.Join(root, "mailboxes", "bob")
	for _, box := range []string{"tmp", "new"} {
		if err := os.MkdirAll(filepath.Join(inRootInbox, box), 0o700); err != nil {
			t.Fatalf("mkdir in-root inbox: %v", err)
		}
	}
	bobInbox := filepath.Join(root, "agents", "bob", "inbox")
	if err := os.RemoveAll(bobInbox); err != nil {
		t.Fatalf("remove bob inbox: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "mailboxes", "bob"), bobInbox); err != nil {
		t.Fatalf("symlink in-root inbox: %v", err)
	}
	clearDeliveryRootTestEnv(t)

	if err := runSend([]string{
		"--root", root,
		"--me", "alice",
		"--to", "bob",
		"--body", "contained symlink",
	}); err != nil {
		t.Fatalf("send through contained mailbox symlink: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(inRootInbox, "new"))
	if err != nil {
		t.Fatalf("ReadDir in-root inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("in-root mailbox received %d messages, want 1", len(entries))
	}
}

func clearDeliveryRootTestEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{envRoot, envBaseRoot, envSession, "AMQ_GLOBAL_ROOT"} {
		setOptionalEnv(t, key, "", false)
	}
}
