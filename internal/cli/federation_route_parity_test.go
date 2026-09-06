package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestReplyRejectsSymlinkedPeerSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	clearSendMailboxTestEnv(t)
	sourceProjectDir := filepath.Join(t.TempDir(), "source")
	sourceRoot := filepath.Join(sourceProjectDir, ".agent-mail", "collab")
	peerBase := filepath.Join(t.TempDir(), "peer-mail")
	outsideTarget := filepath.Join(t.TempDir(), "peer-qa")
	ensureRouteAgents(t, sourceRoot, "alice")
	ensureRouteAgents(t, outsideTarget, "bob")
	if err := os.MkdirAll(peerBase, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideTarget, filepath.Join(peerBase, "qa")); err != nil {
		t.Fatal(err)
	}
	writeRouteAmqrc(t, sourceProjectDir, map[string]any{
		"root":    ".agent-mail",
		"project": "source",
		"peers": map[string]string{
			"peer": peerBase,
		},
	})
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	originalID := deliverOriginalForReply(t, sourceRoot, "alice", format.Header{
		From:         "bob",
		To:           []string{"alice"},
		Thread:       "federated-thread",
		ReplyTo:      "bob@qa",
		ReplyProject: "peer",
		FromProject:  "peer",
	})
	_, _, err := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", sourceRoot,
			"--me", "alice",
			"--id", originalID,
			"--body", "must not escape peer base",
		})
	})
	if err == nil {
		t.Fatal("reply through symlinked peer session succeeded")
	}
	if !strings.Contains(err.Error(), "direct directory under base") {
		t.Fatalf("error = %v, want direct child refusal", err)
	}
	if got := inboxCount(t, outsideTarget, "bob"); got != 0 {
		t.Fatalf("outside peer inbox count = %d, want 0", got)
	}
}

func deliverOriginalForReply(t *testing.T, root, recipient string, header format.Header) string {
	t.Helper()
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		t.Fatal(err)
	}
	header.Schema = format.CurrentSchema
	header.ID = id
	header.Created = now.UTC().Format(time.RFC3339Nano)
	message := format.Message{Header: header, Body: "request"}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxesForTest(t, root, []string{recipient}, id+".md", data); err != nil {
		t.Fatal(err)
	}
	return id
}

func soleDeliveredMessage(t *testing.T, root, agent string) format.Message {
	t.Helper()
	entries, err := os.ReadDir(fsq.AgentInboxNew(root, agent))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox entries = %d, want 1", len(entries))
	}
	message, err := format.ReadMessageFile(filepath.Join(fsq.AgentInboxNew(root, agent), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	return message
}
