package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// Regression: #976, the mailbox prompt path proved an earlier delivery with
// os.Lstat, so a directory at inbox/new/<id>.md read as a delivered message.
// It reports no delivery the inbox does not hold.
func TestANonMessageInboxEntryIsNoDelivery(t *testing.T) {
	root := canonicalTempDir(t)
	for _, handle := range []string{"agent", mailboxSender} {
		if err := fsq.EnsureAgentDirs(root, handle); err != nil {
			t.Fatal(err)
		}
	}
	const id = "2026-10-07T00-00-00.000Z_pid1_deadbeef"
	if err := os.Mkdir(filepath.Join(fsq.AgentInboxNew(root, "agent"), id+".md"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent"}
	if err := publishOnce(b, "cockpit/session/s", id, strings.Repeat("e", 64), nil, time.Now(), "hello"); err == nil {
		t.Fatal("prompt: a directory at the message name was reported delivered")
	}
}
