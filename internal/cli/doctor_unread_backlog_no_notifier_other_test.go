//go:build !darwin && !linux

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestDoctorOpsV2UnsupportedPlatformDoesNotClaimUnverifiedNotifierAbsent(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "alice")
	messagePath := filepath.Join(fsq.AgentInboxNew(root, "alice"), "message.md")
	if err := os.WriteFile(messagePath, []byte("message"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeWakeLockForTest(t, root, "alice", wakeLock{
		PID:        4242,
		Executable: "amq",
	})

	result := runOpsChecksWithSchema(root, "test", false, wakeCheckSchemaV2)
	if len(result.WakeLocks) != 1 || result.WakeLocks[0].Status != string(wakeLockUnverified) {
		t.Fatalf("unsupported-platform lock = %#v", result.WakeLocks)
	}
	if result.WakeLocks[0].NotifierAbsent {
		t.Fatalf("unverified lock classified absent: %#v", result.WakeLocks[0])
	}
	if _, found := findOpsHint(result.Hints, "unread_backlog_no_notifier"); found {
		t.Fatalf("unverified lock emitted no-notifier hint: %#v", result.Hints)
	}
}
