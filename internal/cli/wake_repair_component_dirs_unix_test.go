//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestOpenWakeRepairInboxDirRejectsSymlinkedInboxComponent(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	agentPath := fsq.AgentBase(root, "codex")
	inboxPath := filepath.Join(agentPath, "inbox")
	detachedInboxPath := inboxPath + ".detached"
	if err := os.Rename(inboxPath, detachedInboxPath); err != nil {
		t.Fatalf("detach inbox component: %v", err)
	}
	if err := os.Symlink(filepath.Base(detachedInboxPath), inboxPath); err != nil {
		t.Fatalf("replace inbox component with symlink: %v", err)
	}

	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()

	inboxDir, err := openWakeRepairInboxDir(agentDir)
	if inboxDir != nil {
		_ = inboxDir.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "inbox parent directory") {
		t.Fatalf("symlinked intermediate inbox component error = %v", err)
	}
}
