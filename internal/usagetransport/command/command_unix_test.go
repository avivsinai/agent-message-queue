//go:build unix

package command

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/usage/edge"
)

// PR #1235 review: a child that exited normally left its descendants
// running. The defect lives at the process boundary, so this runs a real
// shell.
func TestRunKillsDescendantsOfANormalExit(t *testing.T) {
	reply := Run(context.Background(), edge.Command{
		Path:   "sh",
		Args:   []string{"-c", "sleep 30 </dev/null >/dev/null 2>&1 & echo $!"},
		MaxOut: 64,
	})
	if reply.Failure != "" {
		t.Fatalf("run failed: %s %v", reply.Failure, reply.Err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(reply.Stdout)))
	if err != nil {
		t.Fatalf("no descendant pid in %q", reply.Stdout)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
	}
	t.Fatalf("descendant %d still alive after the child exited", pid)
}
