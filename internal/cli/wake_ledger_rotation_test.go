package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
)

// sabotageNotificationJournalRotation fills the default-capped journal past
// its cap with one large (valid, optional) record and puts a DIRECTORY at the
// archive path, so every later append persists its record but fails rotation.
// It returns the archive path so a test can clear the sabotage before List.
func sabotageNotificationJournalRotation(t *testing.T, root string) string {
	t.Helper()
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	dir := filepath.Join(root, "agents", "codex", "receipts")
	// Many small valid records (List scans line by line with a bounded token
	// size); orphan results are optional under compaction, so only the
	// sabotaged archive path makes rotation fail.
	var filler strings.Builder
	for i := 0; filler.Len() < 300*1024; i++ {
		fmt.Fprintf(&filler,
			`{"schema":%d,"attempt_id":"fill-%d","phase":"result","message_ids":["msg-fill"],"agent":"codex","mode":"raw","recorded_at":"2026-09-01T10:00:00Z","outcome":"written","detail":"filler"}`+"\n",
			notificationattempt.SchemaVersion, i,
		)
	}
	if err := os.WriteFile(filepath.Join(dir, notificationattempt.LogFilename), []byte(filler.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	rotated := filepath.Join(dir, notificationattempt.LogFilename+notificationattempt.RotatedSuffix)
	if err := os.Mkdir(rotated, 0o700); err != nil {
		t.Fatal(err)
	}
	return rotated
}

// v1 path (no external injector): a rotation-only error from Prepare means the
// prepared record persisted. The delivery result must join it normally. An
// implementation that treats every Prepare error as "record lost" writes a
// failure result for a real prepared record, and a successfully written
// notification traces as `failed`.
func TestV1RotationOnlyPrepareErrorDoesNotFabricateFailedAttempt(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	rotated := sabotageNotificationJournalRotation(t, root)
	cfg := &wakeConfig{
		me:            "codex",
		root:          root,
		wakeOwner:     &wakeOwner{},
		injectMode:    wakeInjectModeRaw,
		retryUntil:    wakeRetryUntilInjected,
		terminalWrite: func(string) error { return nil },
	}
	current := wakeDoorbellTestFiles(t, "pending.md")
	if err := deliverWithNotificationLedger(cfg, []string{"msg-v1-rotation"}, peerWakeNotification("pending message"), false, current); err != nil {
		t.Fatalf("delivery error = %v", err)
	}
	if err := os.Remove(rotated); err != nil {
		t.Fatal(err)
	}
	attempts := listNotificationAttemptsForTest(t, root, "msg-v1-rotation")
	if len(attempts) != 1 {
		t.Fatalf("attempts = %+v, want exactly one", attempts)
	}
	if attempts[0].State != notificationattempt.OutcomeWritten {
		t.Fatalf("attempt state under rotation-only error = %q (result %+v), want written", attempts[0].State, attempts[0].Result)
	}
}

// v2 path: Begin under a rotation-only error returns a PERSISTED lifecycle.
// It must be cached like any live attempt; otherwise the next scan for the
// same cohort begins a second AttemptID and the deferred attempt's
// transitions land on a duplicate.
func TestV2RotationOnlyBeginCachesLiveLifecycle(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	sabotageNotificationJournalRotation(t, root)
	cfg := protocolWakeConfigForTest(t, root, "/bin/echo", nil)
	writer := notificationattempt.NewWriter(root, "codex")
	ids := []string{"msg-v2-rotation"}
	current := wakeDoorbellTestFiles(t, "pending.md")

	first, err := ensureNotificationAttempt(cfg, writer, ids, current)
	if err != nil || first == nil {
		t.Fatalf("first ensure = (%v, %v), want live lifecycle without error", first, err)
	}
	if cfg.notificationAttempt == nil {
		t.Fatal("rotation-only Begin did not cache the persisted lifecycle")
	}
	second, err := ensureNotificationAttempt(cfg, writer, ids, current)
	if err != nil || second == nil {
		t.Fatalf("second ensure = (%v, %v)", second, err)
	}
	if second.AttemptID != first.AttemptID {
		t.Fatalf("second scan began a duplicate attempt %q, want cached %q", second.AttemptID, first.AttemptID)
	}
}
