//go:build darwin || linux

package acp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// Review of #964: repair reopened the ambient root after delivery had pinned
// it, so a replaced root or agents ancestor redirected mailbox creation before
// the eventual delivery refused. The sender's existing sync hook supplies the
// interleaving at the real publication boundary, without a timing race.
func TestMailboxRepairDoesNotFollowReplacedAncestors(t *testing.T) {
	for _, ancestor := range []string{"root", "agents"} {
		t.Run(ancestor, func(t *testing.T) {
			parent := canonicalTempDir(t)
			root := filepath.Join(parent, "queue")
			replacement := filepath.Join(parent, "replacement")
			if err := fsq.EnsureAgentDirs(root, "agent"); err != nil {
				t.Fatal(err)
			}
			foreignMailbox := filepath.Join(replacement, "agents", "agent")
			if ancestor == "agents" {
				foreignMailbox = filepath.Join(replacement, "agent")
			}
			if err := os.MkdirAll(filepath.Join(foreignMailbox, "inbox"), 0o700); err != nil {
				t.Fatal(err)
			}
			swapped := false
			fsq.SetPackageSyncDirFaultForTest(func(string) error {
				if swapped {
					return nil
				}
				swapped = true
				if ancestor == "root" {
					if err := os.Rename(root, filepath.Join(parent, "original")); err != nil {
						return err
					}
					return os.Rename(replacement, root)
				}
				agents := filepath.Join(root, "agents")
				if err := os.Rename(agents, filepath.Join(parent, "original-agents")); err != nil {
					return err
				}
				return os.Symlink(replacement, agents)
			})
			t.Cleanup(func() { fsq.SetPackageSyncDirFaultForTest(nil) })
			now := time.Now()
			id, err := format.NewMessageID(now)
			if err != nil {
				t.Fatal(err)
			}
			b := binding.Binding{Carrier: binding.CarrierMailbox, Root: root, Handle: "agent"}
			if err := publishOnce(b, "review/root-replacement", id, now, "hello"); err == nil {
				t.Fatal("delivery succeeded after its ancestor was replaced")
			}
			if !swapped {
				t.Fatal("publication did not reach the ancestor swap")
			}
			if ancestor == "root" {
				foreignMailbox = filepath.Join(root, "agents", "agent")
			}
			entries, err := os.ReadDir(foreignMailbox)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "inbox" {
				t.Fatalf("repair changed the replacement mailbox: %v", entries)
			}
			entries, err = os.ReadDir(filepath.Join(foreignMailbox, "inbox"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("repair changed the replacement inbox: %v", entries)
			}
		})
	}
}
