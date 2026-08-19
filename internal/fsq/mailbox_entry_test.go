package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	traversalHandle   = "../meta"
	traversalFilename = "../../.wake.lock"
	validHandle       = "codex"
	validFilename     = "ok.md"
)

func TestMailboxEntrypointsRejectTraversalBeforeMutation(t *testing.T) {
	t.Run("DeliverToInboxes", func(t *testing.T) {
		root := newQueueRoot(t)
		delivery := openDeliveryRootForTest(t, root)

		_, err := DeliverToInboxes(delivery, []string{traversalHandle}, validFilename, []byte("payload"))
		if err == nil {
			t.Fatal("DeliverToInboxes(../meta) error = nil, want rejection")
		}
		assertNoMetaInbox(t, root)

		_, err = DeliverToInboxes(delivery, []string{validHandle}, traversalFilename, []byte("payload"))
		if err == nil {
			t.Fatal("DeliverToInboxes(../../.wake.lock) error = nil, want rejection")
		}
		assertNoWakeLockEscape(t, root, validHandle)

		path, err := DeliverToInboxes(delivery, []string{validHandle}, validFilename, []byte("payload"))
		if err != nil {
			t.Fatalf("DeliverToInboxes(valid) error = %v", err)
		}
		got, readErr := os.ReadFile(path[validHandle])
		if readErr != nil || string(got) != "payload" {
			t.Fatalf("delivered content = %q err=%v, want payload", got, readErr)
		}
	})

	t.Run("DeliverToExistingInbox", func(t *testing.T) {
		root := newQueueRoot(t)
		delivery := openDeliveryRootForTest(t, root)

		_, err := DeliverToExistingInbox(delivery, traversalHandle, validFilename, []byte("payload"))
		if err == nil {
			t.Fatal("DeliverToExistingInbox(../meta) error = nil, want rejection")
		}
		assertNoMetaInbox(t, root)

		_, err = DeliverToExistingInbox(delivery, validHandle, traversalFilename, []byte("payload"))
		if err == nil {
			t.Fatal("DeliverToExistingInbox(../../.wake.lock) error = nil, want rejection")
		}
		assertNoWakeLockEscape(t, root, validHandle)

		path, err := DeliverToExistingInbox(delivery, validHandle, validFilename, []byte("payload"))
		if err != nil {
			t.Fatalf("DeliverToExistingInbox(valid) error = %v", err)
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil || string(got) != "payload" {
			t.Fatalf("delivered content = %q err=%v, want payload", got, readErr)
		}
	})

	t.Run("MoveNewToCur", func(t *testing.T) {
		root := newQueueRoot(t)
		if err := os.WriteFile(filepath.Join(AgentInboxNew(root, validHandle), validFilename), []byte("payload"), 0o600); err != nil {
			t.Fatalf("write inbox message: %v", err)
		}
		delivery := openDeliveryRootForTest(t, root)

		if err := MoveNewToCur(delivery, traversalHandle, validFilename); err == nil {
			t.Fatal("MoveNewToCur(../meta) error = nil, want rejection")
		}
		assertNoMetaInbox(t, root)

		if err := MoveNewToCur(delivery, validHandle, traversalFilename); err == nil {
			t.Fatal("MoveNewToCur(../../.wake.lock) error = nil, want rejection")
		}
		assertNoWakeLockEscape(t, root, validHandle)

		if err := MoveNewToCur(delivery, validHandle, validFilename); err != nil {
			t.Fatalf("MoveNewToCur(valid) error = %v", err)
		}
		got, readErr := os.ReadFile(filepath.Join(AgentInboxCur(root, validHandle), validFilename))
		if readErr != nil || string(got) != "payload" {
			t.Fatalf("claimed content = %q err=%v, want payload", got, readErr)
		}
	})

	t.Run("FindMessage", func(t *testing.T) {
		root := newQueueRoot(t)
		decoy := filepath.Join(root, "meta", "inbox", "new", validFilename)
		if err := os.MkdirAll(filepath.Dir(decoy), 0o700); err != nil {
			t.Fatalf("mkdir decoy: %v", err)
		}
		if err := os.WriteFile(decoy, []byte("escaped"), 0o600); err != nil {
			t.Fatalf("write decoy: %v", err)
		}
		inboxPath := filepath.Join(AgentInboxNew(root, validHandle), validFilename)
		if err := os.WriteFile(inboxPath, []byte("payload"), 0o600); err != nil {
			t.Fatalf("write inbox message: %v", err)
		}

		path, box, err := FindMessage(root, traversalHandle, validFilename)
		if err == nil {
			t.Fatalf("FindMessage(../meta) = (%q, %q), want rejection of decoy at %s", path, box, decoy)
		}
		if path != "" || box != "" {
			t.Fatalf("FindMessage(../meta) leaked path (%q, %q)", path, box)
		}

		path, box, err = FindMessage(root, validHandle, traversalFilename)
		if err == nil {
			t.Fatalf("FindMessage(../../.wake.lock) = (%q, %q), want rejection", path, box)
		}

		path, box, err = FindMessage(root, validHandle, validFilename)
		if err != nil {
			t.Fatalf("FindMessage(valid) error = %v", err)
		}
		if path != inboxPath || box != BoxNew {
			t.Fatalf("FindMessage(valid) = (%q, %q), want (%q, %q)", path, box, inboxPath, BoxNew)
		}
	})
}

func newQueueRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, validHandle); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	return root
}

func assertNoMetaInbox(t *testing.T, root string) {
	t.Helper()
	metaInbox := filepath.Join(root, "meta", "inbox")
	if _, err := os.Lstat(metaInbox); !os.IsNotExist(err) {
		t.Fatalf("traversal created %s: %v", metaInbox, err)
	}
}

func assertNoWakeLockEscape(t *testing.T, root, agent string) {
	t.Helper()
	for _, path := range []string{
		filepath.Join(root, ".wake.lock"),
		filepath.Join(root, "agents", ".wake.lock"),
		filepath.Join(root, "agents", agent, ".wake.lock"),
		filepath.Join(AgentInboxTmp(root, agent), traversalFilename),
		filepath.Join(AgentInboxNew(root, agent), traversalFilename),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("escaped path created: %s (%v)", path, err)
		}
	}
}
