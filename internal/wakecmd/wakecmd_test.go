package wakecmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Review of Buzz slash commands: a command the wake claimed but has not
// answered still counts toward the queue's bound.
func TestEnqueueCountsClaimedCommands(t *testing.T) {
	agentDir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	enqueue := func(n int) (string, error) {
		return Enqueue(agentDir, fmt.Sprintf("k%d", n), Request{Target: "t", Text: "/compact", Event: []byte("{}")}, now.Add(time.Duration(n)))
	}
	var first string
	for n := range MaxPending {
		id, err := enqueue(n)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			first = id
		}
	}
	if err := os.Rename(filepath.Join(Dir(agentDir), first+ReqSuffix), filepath.Join(Dir(agentDir), first+TypingSuffix)); err != nil {
		t.Fatal(err)
	}
	if _, err := enqueue(MaxPending); err != ErrFull {
		t.Fatalf("enqueue beyond %d unanswered commands: err = %v, want ErrFull", MaxPending, err)
	}
}

// A request that failed to write does not hold its event key, so a
// redelivery of the event is admitted (audit 2026-11-10).
func TestEnqueueFailedWriteFreesKey(t *testing.T) {
	agentDir := t.TempDir()
	now := time.Now()
	req := Request{Target: "t", Text: "/compact", Event: []byte("{}")}
	blocker := filepath.Join(Dir(agentDir), fmt.Sprintf("%020d-k", now.UnixNano())+ReqSuffix)
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Enqueue(agentDir, "k", req, now); err == nil {
		t.Fatal("enqueue over a directory: err = nil, want a write error")
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := Enqueue(agentDir, "k", req, now); err != nil {
		t.Fatalf("enqueue after a failed write: err = %v, want nil", err)
	}
}
