package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

func TestRouteExplainSameRootConfiguredMissingMailboxIsRoutableWithoutRepair(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	missing := filepath.Join(root, "agents", "bob")
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}

	result := runRouteExplainJSONForTest(t,
		"--from-root", root,
		"--me", "alice",
		"--to", "bob",
	)
	if !result.Routable {
		t.Fatalf("configured local route = non-routable: %s", result.Error)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("route explain repaired destination mailbox: %v", err)
	}

	runExplainedSend(t, result, "same-root repair")
	assertCompleteSendMailbox(t, root, "bob")
	if got := inboxCount(t, root, "bob"); got != 1 {
		t.Fatalf("delivered messages = %d, want 1", got)
	}
}

func TestReplyRepairsConfiguredSameRootMissingMailbox(t *testing.T) {
	root := initializedSendMailboxRoot(t, "alice", "bob")
	originalID := deliverOriginalForReply(t, root, "alice", format.Header{
		From:    "bob",
		To:      []string{"alice"},
		Thread:  "p2p/alice__bob",
		Subject: "local request",
	})
	if err := os.RemoveAll(filepath.Join(root, "agents", "bob")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runReply([]string{
			"--root", root,
			"--me", "alice",
			"--id", originalID,
			"--body", "local response",
			"--json",
		})
	}); err != nil {
		t.Fatalf("reply: %v", err)
	}

	assertCompleteSendMailbox(t, root, "bob")
	reply := soleDeliveredMessage(t, root, "bob")
	if reply.Header.Thread != "p2p/alice__bob" {
		t.Fatalf("reply thread = %q", reply.Header.Thread)
	}
	if len(reply.Header.Refs) != 1 || reply.Header.Refs[0] != originalID {
		t.Fatalf("reply refs = %v, want [%s]", reply.Header.Refs, originalID)
	}
}

func runExplainedSend(t *testing.T, result routeExplainResult, body string) {
	t.Helper()
	if len(result.Argv) < 2 || result.Argv[0] != "amq" || result.Argv[1] != "send" {
		t.Fatalf("route argv = %v, want amq send prefix", result.Argv)
	}
	args := append([]string(nil), result.Argv[2:]...)
	args = append(args, "--body", body, "--json")
	if _, _, err := captureEnvOutput(t, func() error {
		return runSend(args)
	}); err != nil {
		t.Fatalf("explained send: %v", err)
	}
}
