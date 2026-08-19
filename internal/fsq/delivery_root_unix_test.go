//go:build darwin || linux

package fsq

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenDeliveryRootRejectsSwapBetweenAuthorizationAndOpen(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "authorized")
	parked := filepath.Join(parent, "authorized-parked")
	outside := filepath.Join(parent, "outside")
	for _, path := range []string{base, outside} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
	}
	identity, err := SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatalf("SnapshotDeliveryRoot: %v", err)
	}
	if err := os.Rename(base, parked); err != nil {
		t.Fatalf("park authorized root: %v", err)
	}
	if err := os.Symlink(outside, base); err != nil {
		t.Fatalf("install malicious alias: %v", err)
	}

	root, err := OpenDeliveryRoot(base, identity)
	if root != nil {
		_ = root.Close()
		t.Fatal("OpenDeliveryRoot returned a capability for the swapped root")
	}
	if err == nil || !errors.Is(err, ErrDeliveryRootChanged) || !strings.Contains(err.Error(), "between authorization and capability open") {
		t.Fatalf("OpenDeliveryRoot error = %v, want authorization/open mismatch", err)
	}
}

func TestDeliveryRootConcurrentAncestorAndRootAliasSwap(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T) (base, swapPath, parkedPath, maliciousTarget, maliciousBase string)
	}{
		{
			name: "root alias",
			make: func(t *testing.T) (string, string, string, string, string) {
				parent := t.TempDir()
				base := filepath.Join(parent, "authorized")
				outside := filepath.Join(parent, "outside")
				return base, base, filepath.Join(parent, "authorized-parked"), outside, outside
			},
		},
		{
			name: "ancestor alias",
			make: func(t *testing.T) (string, string, string, string, string) {
				parent := t.TempDir()
				ancestor := filepath.Join(parent, "authorized-parent")
				outsideAncestor := filepath.Join(parent, "outside-parent")
				return filepath.Join(ancestor, "root"), ancestor, filepath.Join(parent, "authorized-parent-parked"), outsideAncestor, filepath.Join(outsideAncestor, "root")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, swapPath, parkedPath, maliciousTarget, maliciousBase := tt.make(t)
			for _, tree := range []string{base, maliciousBase} {
				if err := EnsureAgentDirs(tree, "bob"); err != nil {
					t.Fatalf("EnsureAgentDirs(%s): %v", tree, err)
				}
			}
			identity, err := SnapshotDeliveryRoot(base)
			if err != nil {
				t.Fatalf("SnapshotDeliveryRoot: %v", err)
			}
			root, err := OpenDeliveryRoot(base, identity)
			if err != nil {
				t.Fatalf("OpenDeliveryRoot: %v", err)
			}
			defer func() { _ = root.Close() }()

			for i := 0; i < 200; i++ {
				installed := make(chan error, 1)
				release := make(chan struct{})
				restored := make(chan error, 1)
				go func() {
					if err := os.Rename(swapPath, parkedPath); err != nil {
						err = fmt.Errorf("park authorized path: %w", err)
						installed <- err
						restored <- err
						return
					}
					if err := os.Symlink(maliciousTarget, swapPath); err != nil {
						_ = os.Rename(parkedPath, swapPath)
						err = fmt.Errorf("install malicious alias: %w", err)
						installed <- err
						restored <- err
						return
					}
					installed <- nil
					<-release
					if err := os.Remove(swapPath); err != nil {
						restored <- fmt.Errorf("remove malicious alias: %w", err)
						return
					}
					if err := os.Rename(parkedPath, swapPath); err != nil {
						restored <- fmt.Errorf("restore authorized path: %w", err)
						return
					}
					restored <- nil
				}()
				if err := <-installed; err != nil {
					<-restored
					t.Fatal(err)
				}
				filename := fmt.Sprintf("stress-%03d.md", i)
				_, err := DeliverToInboxes(root, []string{"bob"}, filename, []byte("contained"))
				close(release)
				if restoreErr := <-restored; restoreErr != nil {
					t.Fatal(restoreErr)
				}
				if err == nil ||
					!strings.Contains(err.Error(), "delivery root changed after authorization") ||
					!strings.Contains(err.Error(), deliveryRootChangedRemedy) {
					t.Fatalf("DeliverToInboxes error = %v, want actionable root-change refusal", err)
				}
			}
			entries, err := os.ReadDir(AgentInboxNew(maliciousBase, "bob"))
			if err != nil {
				t.Fatalf("ReadDir malicious inbox: %v", err)
			}
			if len(entries) != 0 {
				t.Fatalf("malicious inbox received %d messages", len(entries))
			}
		})
	}
}

func TestDLQEnvelopeLockRechecksRootAliasAfterWaiting(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "authorized")
	parked := filepath.Join(parent, "authorized-parked")
	outside := filepath.Join(parent, "outside")
	for _, tree := range []string{base, outside} {
		if err := EnsureAgentDirs(tree, "alice"); err != nil {
			t.Fatalf("EnsureAgentDirs(%s): %v", tree, err)
		}
	}
	identity, err := SnapshotDeliveryRoot(base)
	if err != nil {
		t.Fatalf("SnapshotDeliveryRoot: %v", err)
	}
	first, err := OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatalf("OpenDeliveryRoot(first): %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := OpenDeliveryRoot(base, identity)
	if err != nil {
		t.Fatalf("OpenDeliveryRoot(second): %v", err)
	}
	defer func() { _ = second.Close() }()

	firstLocked := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.WithDLQEnvelopeLock("alice", "blocked.md", func(*DeliveryRoot) error {
			close(firstLocked)
			<-releaseFirst
			return nil
		})
	}()
	<-firstLocked

	var secondCallbackRan atomic.Bool
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- second.WithDLQEnvelopeLock("alice", "blocked.md", func(*DeliveryRoot) error {
			secondCallbackRan.Store(true)
			return nil
		})
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second lock did not wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := os.Rename(base, parked); err != nil {
		t.Fatalf("park authorized root: %v", err)
	}
	if err := os.Symlink(outside, base); err != nil {
		t.Fatalf("install replacement alias: %v", err)
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first lock holder: %v", err)
	}
	err = <-secondDone
	if err == nil || !strings.Contains(err.Error(), "delivery root changed after authorization") {
		t.Fatalf("waiting lock error = %v, want root-change refusal", err)
	}
	if secondCallbackRan.Load() {
		t.Fatal("waiting lock callback ran after the authorized root alias changed")
	}
}
