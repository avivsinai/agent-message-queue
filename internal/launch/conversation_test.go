package launch

import (
	"os"
	"testing"
)

func TestConversationRoundTripRequiresHandleLock(t *testing.T) {
	dir, root := harnessRoot(t)
	lease := mustAcquireLease(t, root)
	record := ConversationRecord{
		Version: ConversationVersion, Handle: "claude", State: CaptureReady,
		Identity:    ConversationIdentity{Provider: ClaudeProvider, ID: testConversationID},
		LaunchNonce: testLaunchNonce,
	}
	if err := WriteConversation(root, lease, record); err == nil {
		t.Fatal("WriteConversation without handle lock succeeded")
	}
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	if err := WriteConversation(root, lease, record); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConversation(root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity != record.Identity || got.LaunchNonce != record.LaunchNonce {
		t.Fatalf("conversation = %#v, want %#v", got, record)
	}
	info, err := os.Stat(ConversationPath(dir, "claude"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("conversation permissions = %04o", info.Mode().Perm())
	}
}

func TestConversationRejectsCrossHandleAndPendingIdentity(t *testing.T) {
	_, root := harnessRoot(t)
	lease := mustAcquireLease(t, root)
	if err := lease.LockHandles("claude"); err != nil {
		t.Fatal(err)
	}
	bad := ConversationRecord{
		Version: ConversationVersion, Handle: "codex", State: CapturePending,
		Identity:    ConversationIdentity{Provider: CodexProvider, ID: testConversationID},
		LaunchNonce: testLaunchNonce,
	}
	if err := WriteConversation(root, lease, bad); err == nil {
		t.Fatal("cross-handle pending identity write succeeded")
	}
}
