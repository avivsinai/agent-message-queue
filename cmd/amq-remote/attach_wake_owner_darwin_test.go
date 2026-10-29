//go:build darwin

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// A mailbox attach in a harness that is neither Codex nor Claude (pi under
// `coop exec`) proves the owner its wake recorded as the session's holder,
// so the machine agent makes the session's channel. Darwin only: the lock
// records no pid namespace, so Linux leaves the holder unproven.
func TestMailboxAttachProvesTheWakeOwnerItRunsUnder(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self, err := launch.HolderOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	sid, err := unix.Getsid(0)
	if err != nil {
		t.Fatal(err)
	}
	lock, _ := json.Marshal(map[string]any{
		"pid": os.Getpid() + 1, "tty": "/dev/ttys001", "root": root, "agent": "pi",
		"started": time.Now().UTC().Format(time.RFC3339), "resume_schema": 2,
		"resume_owner": map[string]any{"pid": self.PID, "process_start": self.ProcessStart, "boot_id": self.BootID, "session_id": sid},
	})
	dir := filepath.Join(root, "agents", "pi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".wake.lock"), lock, 0o600); err != nil {
		t.Fatal(err)
	}
	h := currentHarness(newCodexView(), root, "pi")
	if h.Holder != self {
		t.Fatalf("harness = %+v, want holder %+v", h, self)
	}
	if state, evidence := (binding.Binding{Root: root, Holder: h.holder()}).Liveness(); state != binding.Live {
		t.Fatalf("liveness = %s (%s), want live", state, evidence)
	}
}
