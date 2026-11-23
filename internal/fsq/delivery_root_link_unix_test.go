//go:build darwin || linux

package fsq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A mailbox component that is a link to another directory of the same root
// made EnsureAgentDirs create the mailbox inside the link's target, for
// example inside another agent's mailbox (bead agent-message-queue-jfyx).
func TestEnsureAgentDirsRefusesALinkedMailboxComponent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		link   string
		target string
	}{
		{name: "inbox", link: filepath.Join("agents", "bob", "inbox"), target: filepath.Join("..", "other")},
		{name: "agent", link: filepath.Join("agents", "bob"), target: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			other := filepath.Join(base, "agents", "other")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(base, tc.link)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target, filepath.Join(base, tc.link)); err != nil {
				t.Fatal(err)
			}

			err := openDeliveryRootForTest(t, base).EnsureAgentDirs("bob")
			want := "mailbox path " + tc.link + " is a symbolic link"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("EnsureAgentDirs(bob) = %v, want an error containing %q", err, want)
			}
			entries, err := os.ReadDir(other)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("agents/other holds %d entries created through the link", len(entries))
			}
		})
	}
}
