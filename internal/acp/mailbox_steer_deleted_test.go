package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review of #965: a steer recreated a handle mailbox deleted after the parent
// prompt, reporting injection into a mailbox whose consumer was gone.
func TestMailboxSteerDoesNotRecreateDeletedMailbox(t *testing.T) {
	s, root := mailboxServer(t)
	runSteeredTurn(t, s, "s", strings.Repeat("1", 64), root, func(string) {
		path := filepath.Join(root, "agents", "agent")
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		resp := steerEvent(s, "s", "smaller design", strings.Repeat("e", 64))
		if resp.Error == nil {
			t.Fatalf("steer to deleted mailbox returned %+v; want a refusal", resp.Result)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("steer recreated the deleted mailbox: %v", err)
		}
	})
}
