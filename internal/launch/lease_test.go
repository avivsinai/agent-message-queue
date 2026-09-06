package launch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
