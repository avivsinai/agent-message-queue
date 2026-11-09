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
