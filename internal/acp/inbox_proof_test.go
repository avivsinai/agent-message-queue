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

// Bead agent-message-queue-u1e (Pro review of #976 r2, P2): the mailbox
// prompt and steer paths proved an earlier delivery with os.Lstat, so a
// directory at inbox/new/<id>.md read as a delivered message. Neither path
// reports a delivery the inbox does not hold.
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
	if err := publishOnce(b, "cockpit/session/s", id, time.Now(), "hello"); err == nil {
		t.Fatal("prompt: a directory at the message name was reported delivered")
	}
	claim := steerClaim{MessageID: id, Created: time.Now().UTC().Format(time.RFC3339Nano), Root: root, Handle: "agent", Thread: "cockpit/session/s", Prompt: "p"}
	if _, err := writeSteerOnce(claim, strings.Repeat("e", 64), "steer", func() bool { return true }); err == nil {
		t.Fatal("steer: a directory at the message name was reported delivered")
	}
}
