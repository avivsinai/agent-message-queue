package fsq

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDeliverToInbox(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	data := []byte("hello")
	filename := "test.md"
	path, err := DeliverToInbox(openDeliveryRootForTest(t, root), "codex", filename, data)
	if err != nil {
		t.Fatalf("DeliverToInbox: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected message in new: %v", err)
	}
	tmpDir := filepath.Join(root, "agents", "codex", "inbox", "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected tmp empty, got %d", len(entries))
	}
}

func TestMoveNewToCur(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	data := []byte("hello")
	filename := "move.md"
	if _, err := DeliverToInbox(openDeliveryRootForTest(t, root), "codex", filename, data); err != nil {
		t.Fatalf("DeliverToInbox: %v", err)
	}
	if err := MoveNewToCur(openDeliveryRootForTest(t, root), "codex", filename); err != nil {
		t.Fatalf("MoveNewToCur: %v", err)
	}
	newPath := filepath.Join(root, "agents", "codex", "inbox", "new", filename)
	curPath := filepath.Join(root, "agents", "codex", "inbox", "cur", filename)
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("expected new missing")
	}
	if _, err := os.Stat(curPath); err != nil {
		t.Fatalf("expected cur present: %v", err)
	}
}

func TestDeliverToExistingInbox(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	data := []byte("cross-project message")
	filename := "xproj.md"
	path, err := DeliverToExistingInbox(openDeliveryRootForTest(t, root), "codex", filename, data)
	if err != nil {
		t.Fatalf("DeliverToExistingInbox: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected message in new: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content mismatch: got %q, want %q", got, data)
	}

	// tmp should be empty after delivery
	tmpDir := AgentInboxTmp(root, "codex")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected tmp empty, got %d", len(entries))
	}
}

func TestDeliverToExistingInboxNoDir(t *testing.T) {
	root := t.TempDir()
	// Do NOT create agent dirs — inbox doesn't exist.
	_, err := DeliverToExistingInbox(openDeliveryRootForTest(t, root), "ghost", "test.md", []byte("nope"))
	if err == nil {
		t.Fatal("expected error for non-existent inbox")
	}
}

func TestDeliverToExistingInboxPostRenameSyncFailureReportsCommittedResult(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	deliveryRoot := openDeliveryRootForTest(t, root)
	newDir := filepath.Join("agents", "codex", "inbox", "new")
	deliveryRoot.syncDirForTest = func(dir string) error {
		if dir == newDir {
			return errors.New("injected post-rename sync failure")
		}
		return deliveryRoot.syncDirPlatform(dir)
	}

	filename := "indeterminate.md"
	path, err := DeliverToExistingInbox(deliveryRoot, "codex", filename, []byte("committed"))
	if err == nil {
		t.Fatal("DeliverToExistingInbox error = nil, want indeterminate durability")
	}
	wantPath := filepath.Join(AgentInboxNew(root, "codex"), filename)
	if path != wantPath {
		t.Fatalf("path = %q, want committed path %q", path, wantPath)
	}
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) {
		t.Fatalf("error = %T %v, want typed committed result", err, err)
	}
	if committed.FinalPath != wantPath || committed.Recipient != "codex" {
		t.Fatalf("committed result = (%q,%q), want (%q,codex)", committed.FinalPath, committed.Recipient, wantPath)
	}
	if _, statErr := os.Stat(wantPath); statErr != nil {
		t.Fatalf("committed message missing: %v", statErr)
	}
}

func TestDeliveryRootWriteFileAtomicPostRenameSyncFailureReportsCommittedResult(t *testing.T) {
	root := openDeliveryRootForTest(t, t.TempDir())
	const dir = "meta"
	syncCalls := 0
	root.syncDirForTest = func(got string) error {
		if got == dir {
			syncCalls++
			if syncCalls == 2 {
				return errors.New("injected post-rename sync failure")
			}
		}
		return root.syncDirPlatform(got)
	}

	path, err := root.WriteFileAtomic(dir, "state.json", []byte("committed"), 0o600)
	if err == nil {
		t.Fatal("WriteFileAtomic error = nil, want indeterminate durability")
	}
	wantPath := filepath.Join(root.Base(), dir, "state.json")
	if path != wantPath {
		t.Fatalf("path = %q, want committed path %q", path, wantPath)
	}
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) {
		t.Fatalf("error = %T %v, want typed committed result", err, err)
	}
	if committed.FinalPath != wantPath || committed.Recipient != "" {
		t.Fatalf("committed result = (%q,%q), want (%q,empty)", committed.FinalPath, committed.Recipient, wantPath)
	}
	if data, readErr := os.ReadFile(wantPath); readErr != nil || string(data) != "committed" {
		t.Fatalf("committed file = %q, err=%v", data, readErr)
	}
}

func TestDeliverToInboxesRollback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permissions are unreliable on Windows")
	}
	root := t.TempDir()
	if err := EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs codex: %v", err)
	}
	if err := EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatalf("EnsureAgentDirs claude: %v", err)
	}
	if err := EnsureAgentDirs(root, "ada"); err != nil {
		t.Fatalf("EnsureAgentDirs ada: %v", err)
	}

	cloudNew := AgentInboxNew(root, "claude")
	if err := os.Chmod(cloudNew, 0o555); err != nil {
		t.Fatalf("chmod claude new: %v", err)
	}
	defer func() { _ = os.Chmod(cloudNew, 0o700) }()

	filename := "multi.md"
	_, err := DeliverToInboxes(openDeliveryRootForTest(t, root), []string{"codex", "claude", "ada"}, filename, []byte("hello"))
	if err == nil {
		t.Fatalf("expected delivery error")
	}
	var partial *PartialDeliveryError
	if !errors.As(err, &partial) {
		t.Fatalf("expected PartialDeliveryError, got %T: %v", err, err)
	}
	if partial.Failed != "claude" {
		t.Fatalf("failed recipient = %q, want claude", partial.Failed)
	}
	if len(partial.Pending) != 1 || partial.Pending[0] != "ada" {
		t.Fatalf("pending recipients = %#v, want [ada]", partial.Pending)
	}

	codexNew := filepath.Join(AgentInboxNew(root, "codex"), filename)
	if got := partial.Delivered["codex"]; got != codexNew {
		t.Fatalf("delivered[codex] = %q, want %q", got, codexNew)
	}
	got, err := os.ReadFile(codexNew)
	if err != nil {
		t.Fatalf("expected committed delivery to remain at %s: %v", codexNew, err)
	}
	if string(got) != "hello" {
		t.Fatalf("committed delivery content = %q, want hello", got)
	}

	cloudTmp := filepath.Join(AgentInboxTmp(root, "claude"), filename)
	if _, err := os.Stat(cloudTmp); !os.IsNotExist(err) {
		t.Fatalf("expected failed tmp to be removed from %s", cloudTmp)
	}

	adaTmp := filepath.Join(AgentInboxTmp(root, "ada"), filename)
	if _, err := os.Stat(adaTmp); !os.IsNotExist(err) {
		t.Fatalf("expected pending tmp to be removed from %s", adaTmp)
	}
	adaNew := filepath.Join(AgentInboxNew(root, "ada"), filename)
	if _, err := os.Stat(adaNew); !os.IsNotExist(err) {
		t.Fatalf("expected pending recipient to have no new delivery at %s", adaNew)
	}
}

func TestDeliverToInboxesSyncFailureCountsRenamedStageOnlyAsDelivered(t *testing.T) {
	root := t.TempDir()
	for _, agent := range []string{"codex", "claude"} {
		if err := EnsureAgentDirs(root, agent); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", agent, err)
		}
	}
	deliveryRoot := openDeliveryRootForTest(t, root)
	failingDir := filepath.Join("agents", "codex", "inbox", "new")
	deliveryRoot.syncDirForTest = func(dir string) error {
		if dir == failingDir {
			return errors.New("injected post-rename sync failure")
		}
		return deliveryRoot.syncDirPlatform(dir)
	}

	filename := "sync-failure.md"
	_, err := DeliverToInboxes(deliveryRoot, []string{"codex", "claude"}, filename, []byte("hello"))
	var partial *PartialDeliveryError
	if !errors.As(err, &partial) {
		t.Fatalf("DeliverToInboxes error = %T %v, want PartialDeliveryError", err, err)
	}
	if partial.Failed != "" {
		t.Fatalf("Failed = %q, want empty after successful rename", partial.Failed)
	}
	if got := partial.Delivered["codex"]; got != filepath.Join(AgentInboxNew(root, "codex"), filename) {
		t.Fatalf("Delivered[codex] = %q", got)
	}
	if len(partial.Pending) != 1 || partial.Pending[0] != "claude" {
		t.Fatalf("Pending = %#v, want [claude]", partial.Pending)
	}
	if _, err := os.Stat(filepath.Join(AgentInboxNew(root, "codex"), filename)); err != nil {
		t.Fatalf("committed delivery missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(AgentInboxTmp(root, "codex"), filename)); !os.IsNotExist(err) {
		t.Fatalf("renamed tmp still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(AgentInboxTmp(root, "claude"), filename)); !os.IsNotExist(err) {
		t.Fatalf("pending tmp still exists: %v", err)
	}
}

func openDeliveryRootForTest(t testing.TB, base string) *DeliveryRoot {
	t.Helper()
	identity, err := SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatalf("SnapshotDeliveryRoot(%s): %v", base, err)
	}
	root, err := OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatalf("OpenDeliveryRoot(%s): %v", base, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}
