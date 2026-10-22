//go:build darwin || linux

package cli

import (
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

// A presence update read the file, a wake committed its doorbell state, and
// the update then wrote its stale snapshot over it, so the wake read as ready
// and unparked (PR #1161 review, reproduced under the race detector).
func TestPresenceUpdateKeepsWakeWriteCommittedMidUpdate(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureAgentDirs(root, "claude"); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()

	wakeDone := make(chan error, 1)
	if _, err := presence.SetStatus(root, "claude", "busy", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := presence.Update(root, "claude", func(p *presence.Presence) {
		// Presence writers are serialized by one lock, so a competing writer
		// cannot acquire it while this callback holds its snapshot.
		release, ok, err := lock.TryExclusive(filepath.Join(root, "agents", "claude", presence.LockName))
		if err != nil {
			t.Error(err)
		} else if ok {
			release()
			t.Error("presence lock was free inside Update; a wake write could interleave with the update")
		}
		go func() { wakeDone <- setWakeDoorbellStatusInDir(agentDir, "claude", true, 3) }()
		p.Note = "reviewing"
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-wakeDone; err != nil {
		t.Fatal(err)
	}
	got, err := presence.Read(root, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !got.DoorbellParked || got.DoorbellAttempts != 3 || got.Note != "reviewing" {
		t.Fatalf("presence = %#v, want the wake's parked doorbell and the update's note", got)
	}
}
