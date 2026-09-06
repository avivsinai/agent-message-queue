package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const unreadBacklogNoNotifierRemedy = "drain from the owning session"

func writeUnreadBacklogForTest(t *testing.T, root string, age time.Duration) {
	t.Helper()
	messagePath := filepath.Join(fsq.AgentInboxNew(root, "alice"), "message.md")
	if err := os.WriteFile(messagePath, []byte("message"), 0o600); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		old := time.Now().Add(-age)
		if err := os.Chtimes(messagePath, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDoctorOpsOutputsIncludeUnreadBacklogNoNotifierHint(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "alice")
	writeUnreadBacklogForTest(t, root, 90*time.Second)
	clearDoctorSessionPin(t)

	output, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--root", root, "--ops"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"alice has 1 unread message",
		"oldest 90s",
		"amq wake check --root " + shellQuoteArg(root) + " --me alice",
		unreadBacklogNoNotifierRemedy,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("human output missing %q:\n%s", want, output)
		}
	}

	output, err = captureEnvStdout(t, func() error {
		return runDoctor([]string{"--root", root, "--ops", "--json", "--json-schema=2"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var result doctorResultV2
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Ops == nil {
		t.Fatal("v2 ops result missing")
	}
	hint, found := findOpsHint(result.Ops.Hints, "unread_backlog_no_notifier")
	if !found || hint.UnreadBacklogNoNotifier == nil ||
		!strings.Contains(hint.UnreadBacklogNoNotifier.Command, "--root ") {
		t.Fatalf("v2 JSON hint missing or not root-qualified: %#v", result.Ops.Hints)
	}
}
