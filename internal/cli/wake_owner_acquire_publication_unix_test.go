//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestOwnerAcquisitionUnsupportedPublicationRemovesItsInstalledTarget(t *testing.T) {
	root, target, owner := newOwnerAcquisitionPublicationFixture(t)

	originalLink := publishAuthoritativeWakeLinkAt
	publishAuthoritativeWakeLinkAt = func(int, string, int, string, int) error {
		return syscall.EOPNOTSUPP
	}
	t.Cleanup(func() { publishAuthoritativeWakeLinkAt = originalLink })

	_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Fatalf("owner acquisition error = %v, want unsupported publication", err)
	}
	if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
		t.Fatalf("unsupported publication left a wake lock: %#v", inspection)
	}
	if _, exists, readErr := readWakeTarget(root, "codex"); readErr != nil || exists {
		t.Fatalf("unsupported publication target exists=%v err=%v", exists, readErr)
	}
	if !sameWakeOwner(target.Owner, &owner) {
		t.Fatalf("unsupported publication mutated caller owner: %#v", target.Owner)
	}
}

func TestOwnerAcquisitionUnsupportedPublicationPreservesChangedTarget(t *testing.T) {
	tests := []struct {
		name    string
		replace func(t *testing.T, path string, replacement wakeTarget)
	}{
		{
			name: "new inode",
			replace: func(t *testing.T, path string, replacement wakeTarget) {
				t.Helper()
				data, err := json.MarshalIndent(replacement, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				temp := filepath.Join(filepath.Dir(path), ".replacement-target")
				if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(temp, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "same inode mutated content",
			replace: func(t *testing.T, path string, replacement wakeTarget) {
				t.Helper()
				data, err := json.MarshalIndent(replacement, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, target, owner := newOwnerAcquisitionPublicationFixture(t)
			replacement := target
			replacement.Created = "2026-07-25T12:34:56Z"

			originalLink := publishAuthoritativeWakeLinkAt
			publishAuthoritativeWakeLinkAt = func(int, string, int, string, int) error {
				tc.replace(t, wakeTargetPath(root, "codex"), replacement)
				return syscall.EOPNOTSUPP
			}
			t.Cleanup(func() { publishAuthoritativeWakeLinkAt = originalLink })

			_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
				target:   &target,
				wakeMode: wakeTargetInjectVia,
			})
			if !errors.Is(err, syscall.EOPNOTSUPP) || !strings.Contains(err.Error(), "changed before cleanup") {
				t.Fatalf("owner acquisition error = %v, want unsupported publication plus preservation", err)
			}
			if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
				t.Fatalf("unsupported publication left a wake lock: %#v", inspection)
			}
			persisted, exists, readErr := readWakeTarget(root, "codex")
			if readErr != nil || !exists || !sameWakeTarget(persisted, replacement) {
				t.Fatalf("replacement target = %#v exists=%v err=%v", persisted, exists, readErr)
			}
			if !sameWakeOwner(target.Owner, &owner) {
				t.Fatalf("unsupported publication mutated caller owner: %#v", target.Owner)
			}
		})
	}
}

func TestOwnerPublicationPreservesReplacementInstalledImmediatelyAfterRename(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)
	replacement := target
	replacement.Created = "2026-07-25T13:45:00Z"

	originalAfterRename := publishAuthoritativeWakeAfterTargetRename
	publishAuthoritativeWakeAfterTargetRename = func() {
		data, err := json.MarshalIndent(replacement, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := wakeTargetPath(root, "codex")
		temp := filepath.Join(filepath.Dir(path), ".rename-window-replacement")
		if err := os.WriteFile(temp, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temp, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { publishAuthoritativeWakeAfterTargetRename = originalAfterRename })

	_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if err == nil || !strings.Contains(err.Error(), "changed during publication") {
		t.Fatalf("owner acquisition error = %v, want rename-window replacement refusal", err)
	}
	if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
		t.Fatalf("rename-window replacement published a wake lock: %#v", inspection)
	}
	persisted, exists, readErr := readWakeTarget(root, "codex")
	if readErr != nil || !exists || !sameWakeTarget(persisted, replacement) {
		t.Fatalf("rename-window replacement = %#v exists=%v err=%v", persisted, exists, readErr)
	}
}

func TestOwnerAcquisitionCommittedPublicationFailurePreservesExactClaim(t *testing.T) {
	root, target, _ := newOwnerAcquisitionPublicationFixture(t)

	originalSync := syncWakeOwnerDirFD
	syncCalls := 0
	syncWakeOwnerDirFD = func(fd int) error {
		syncCalls++
		if syncCalls == 2 {
			return syscall.EIO
		}
		return originalSync(fd)
	}
	t.Cleanup(func() { syncWakeOwnerDirFD = originalSync })

	_, err := acquireAuthoritativeWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &target,
		wakeMode: wakeTargetInjectVia,
	})
	if !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), "exact owner claim is visible and was preserved") {
		t.Fatalf("owner acquisition error = %v, want committed preservation", err)
	}
	inspection := inspectWakeLock(root, "codex")
	if !inspection.Exists || classifyPersistedWakeClaim(inspection) != wakeClaimAuthoritative {
		t.Fatalf("committed publication claim = %#v", inspection)
	}
	persisted, exists, readErr := readWakeTarget(root, "codex")
	if readErr != nil || !exists || !sameWakeTarget(persisted, target) {
		t.Fatalf("committed target = %#v exists=%v err=%v", persisted, exists, readErr)
	}
	agentDir, openErr := openWakeAgentDir(root, "codex")
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer func() { _ = agentDir.Close() }()
	if verifyErr := withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		_, pairErr := validateAuthoritativeWakeClaimPairAt(dirfd, agentDir, inspection)
		return pairErr
	}); verifyErr != nil {
		t.Fatalf("committed claim pair is invalid: %v", verifyErr)
	}
}

func newOwnerAcquisitionPublicationFixture(t *testing.T) (string, wakeTarget, wakeOwner) {
	t.Helper()
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "owner-publication-injector")
	owner := wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.Owner = &owner

	originalObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if !sameWakeOwner(&got, &owner) {
			t.Fatalf("observed owner = %#v, want %#v", got, owner)
		}
		return liveWakeOwnerObservationForTest(), nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = originalObserve })
	return root, target, owner
}
