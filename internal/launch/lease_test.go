package launch

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func mustAcquireLease(t *testing.T, root *fsq.DeliveryRoot) *Lease {
	t.Helper()
	lease, err := AcquireLease(root, "nonce-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return lease
}

func TestAcquireLeaseExclusiveAndRelease(t *testing.T) {
	_, root := openTestRoot(t)
	first := mustAcquireLease(t, root)
	_, err := AcquireLease(root, "nonce-other")
	var held *LeaseHeldError
	if !errors.As(err, &held) {
		t.Fatalf("second acquire = %v, want LeaseHeldError", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireLease(root, "nonce-next")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseReleaseRetriesAfterRemovalFailure(t *testing.T) {
	dir, root := openTestRoot(t)
	lease, err := AcquireLease(root, "retry-release")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected lease removal failure")
	oldRemove := removeLeaseRecord
	calls := 0
	removeLeaseRecord = func(root *fsq.DeliveryRoot) error {
		calls++
		if calls == 1 {
			return injected
		}
		return oldRemove(root)
	}
	t.Cleanup(func() { removeLeaseRecord = oldRemove })

	if err := lease.Release(); !errors.Is(err, injected) {
		t.Fatalf("first Release = %v, want injected failure", err)
	}
	if lease.released || lease.secret == 0 {
		t.Fatalf("failed Release discarded capability: released=%v secret=%d", lease.released, lease.secret)
	}
	if _, err := os.Stat(LeasePath(dir)); err != nil {
		t.Fatalf("lease record after failed Release = %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("second Release = %v", err)
	}
	if _, err := os.Stat(LeasePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease record after successful retry = %v, want absent", err)
	}
}

func TestAcquireLeaseRNGFailureDoesNotPublishLease(t *testing.T) {
	dir, root := openTestRoot(t)
	injected := errors.New("injected RNG failure")
	oldRandomRead := leaseRandomRead
	leaseRandomRead = func([]byte) (int, error) { return 0, injected }
	t.Cleanup(func() { leaseRandomRead = oldRandomRead })

	if _, err := AcquireLease(root, "rng-failure"); !errors.Is(err, injected) {
		t.Fatalf("AcquireLease = %v, want injected RNG failure", err)
	}
	if _, err := os.Stat(LeasePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease record after RNG failure = %v, want absent", err)
	}
}

func TestNewSecretRejectsZeroAndCollision(t *testing.T) {
	oldRandomRead := leaseRandomRead
	t.Cleanup(func() { leaseRandomRead = oldRandomRead })

	leaseRandomRead = func(buf []byte) (int, error) {
		clear(buf)
		return len(buf), nil
	}
	if _, err := newSecret(); err == nil || !strings.Contains(err.Error(), "zero") {
		t.Fatalf("zero secret = %v, want zero-value error", err)
	}

	const secret = uint64(0x0102030405060708)
	liveLeaseSecrets.Store(secret, struct{}{})
	t.Cleanup(func() { liveLeaseSecrets.Delete(secret) })
	leaseRandomRead = func(buf []byte) (int, error) {
		binary.BigEndian.PutUint64(buf, secret)
		return len(buf), nil
	}
	if _, err := newSecret(); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("colliding secret = %v, want collision error", err)
	}
}

func TestAcquireLeaseGeneratedNonceIsAdapterCompatibleUUID(t *testing.T) {
	_, root := harnessRoot(t)
	lease, err := AcquireLease(root, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	if !validUUID(lease.LaunchNonce()) {
		t.Fatalf("generated launch nonce %q is not a UUID", lease.LaunchNonce())
	}
}

func TestConcurrentAcquireExactlyOneWins(t *testing.T) {
	const contenders = 8
	dir := t.TempDir()
	type result struct {
		lease *Lease
		err   error
	}
	results := make(chan result, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			identity, err := fsq.SnapshotDeliveryRoot(dir)
			if err != nil {
				results <- result{err: err}
				return
			}
			root, err := fsq.OpenDeliveryRoot(dir, identity)
			if err != nil {
				results <- result{err: err}
				return
			}
			lease, err := AcquireLease(root, "shared-nonce")
			if err != nil {
				_ = root.Close()
				results <- result{err: err}
				return
			}
			t.Cleanup(func() {
				_ = lease.Release()
				_ = root.Close()
			})
			results <- result{lease: lease}
		}()
	}
	wg.Wait()
	close(results)
	var wins, losses int
	for got := range results {
		if got.err == nil {
			wins++
			continue
		}
		var held *LeaseHeldError
		if !errors.As(got.err, &held) {
			t.Fatalf("loser error = %v, want LeaseHeldError", got.err)
		}
		losses++
	}
	if wins != 1 || losses != contenders-1 {
		t.Fatalf("concurrent acquire wins=%d losses=%d, want 1 and %d", wins, losses, contenders-1)
	}
}

func TestStaleLeaseIsReplaceableAfterCrash(t *testing.T) {
	dir, root := openTestRoot(t)
	if err := os.MkdirAll(filepath.Dir(LeasePath(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostileLease(t, dir, leaseRecord{
		Version:     LeaseVersion,
		Holder:      HolderIdentity{PID: 1 << 30, ProcessStart: "1.000000000", BootID: "boot"},
		LaunchNonce: "dead-nonce",
	}, 0o600)
	lease, err := AcquireLease(root, "recovered")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	if lease.LaunchNonce() != "recovered" {
		t.Fatalf("nonce = %q", lease.LaunchNonce())
	}
}

func TestUnverifiedHolderFailsClosed(t *testing.T) {
	dir, root := openTestRoot(t)
	if err := os.MkdirAll(filepath.Dir(LeasePath(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostileLease(t, dir, leaseRecord{
		Version:     LeaseVersion,
		Holder:      HolderIdentity{PID: 1, ProcessStart: "1.000000000", BootID: "boot"},
		LaunchNonce: "unverified-nonce",
	}, 0o600)
	old := inspectProcess
	inspectProcess = func(pid int) processInfo {
		if pid == os.Getpid() {
			return old(pid)
		}
		return processInfo{PID: pid, Running: true, InspectError: errors.New("start token unavailable")}
	}
	t.Cleanup(func() { inspectProcess = old })
	_, err := AcquireLease(root, "should-not-win")
	var unverified *LeaseUnverifiedError
	if !errors.As(err, &unverified) {
		t.Fatalf("acquire = %v, want LeaseUnverifiedError", err)
	}
	got, err := os.ReadFile(LeasePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "unverified-nonce") {
		t.Fatalf("unverified acquire mutated lease: %s", got)
	}
}

func TestPidReuseIdentityMismatchIsStale(t *testing.T) {
	dir, root := openTestRoot(t)
	if err := os.MkdirAll(filepath.Dir(LeasePath(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	self := inspectProcess(os.Getpid())
	writeHostileLease(t, dir, leaseRecord{
		Version: LeaseVersion,
		Holder: HolderIdentity{
			PID:          os.Getpid(),
			ProcessStart: "0.000000001",
			BootID:       self.BootID,
		},
		LaunchNonce: "reused-pid",
	}, 0o600)
	lease, err := AcquireLease(root, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	if lease.LaunchNonce() != "replacement" {
		t.Fatalf("pid-reuse did not replace stale lease: %q", lease.LaunchNonce())
	}
}

func TestLeaseSymlinkAndPermissiveModeRefused(t *testing.T) {
	dir, root := openTestRoot(t)
	path := LeasePath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	self := inspectProcess(os.Getpid())
	writeHostileLease(t, dir, leaseRecord{
		Version:     LeaseVersion,
		Holder:      HolderIdentity{PID: os.Getpid(), ProcessStart: self.StartToken, BootID: self.BootID},
		LaunchNonce: "permissive",
	}, 0o644)
	if _, err := AcquireLease(root, "nope"); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("0644 acquire = %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "lease.json")
	if err := os.WriteFile(outside, []byte(`{"version":1,"holder":{"pid":1},"launch_nonce":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLease(root, "nope"); err == nil {
		t.Fatal("symlink lease was acquired")
	}
}

func TestLockHandlesSortedAndReleasedBeforeLease(t *testing.T) {
	_, root := openTestRoot(t)
	lease := mustAcquireLease(t, root)
	if err := lease.LockHandles("codex", "claude"); err != nil {
		t.Fatal(err)
	}
	if got := lease.LockedHandles(); len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Fatalf("handle order = %#v, want claude then codex", got)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if len(lease.LockedHandles()) != 0 {
		t.Fatal("handle locks survived lease release")
	}
}

func TestWriteBindingRequiresLiveLease(t *testing.T) {
	_, root := openTestRoot(t)
	record := validBinding()
	if err := WriteBinding(root, nil, record); err == nil {
		t.Fatal("nil lease wrote a binding")
	}
	if err := WriteBinding(root, &Lease{}, record); err == nil {
		t.Fatal("forged empty lease wrote a binding")
	}
	lease := mustAcquireLease(t, root)
	if err := WriteBinding(root, &Lease{root: root, nonce: lease.LaunchNonce(), holder: lease.holder, secret: 99}, record); err == nil {
		t.Fatal("forged secret wrote a binding")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := WriteBinding(root, lease, record); err == nil {
		t.Fatal("released lease wrote a binding")
	}
}

func writeHostileLease(t *testing.T, dir string, record leaseRecord, mode os.FileMode) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LeasePath(dir), data, mode); err != nil {
		t.Fatal(err)
	}
}
