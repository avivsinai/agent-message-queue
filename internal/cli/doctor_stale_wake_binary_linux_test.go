//go:build linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxWakeBinaryComparisonUsesRunningExecutableDeviceAndInode(t *testing.T) {
	processExecutable, err := os.Stat("/proc/self/exe")
	if err != nil {
		t.Fatalf("stat current process executable: %v", err)
	}
	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{PID: os.Getpid()},
		resolvedWakeBinary{Info: processExecutable},
	)
	if err != nil {
		t.Fatalf("compare matching executable: %v", err)
	}
	if got.Stale {
		t.Fatalf("matching executable reported stale: %#v", got)
	}
	if !got.Evidence.Available {
		t.Fatalf("matching executable comparison omitted identity evidence: %#v", got)
	}

	otherPath := filepath.Join(t.TempDir(), "other-amq")
	if err := os.WriteFile(otherPath, []byte("different inode"), 0o700); err != nil {
		t.Fatal(err)
	}
	otherExecutable, err := os.Stat(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err = inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{PID: os.Getpid()},
		resolvedWakeBinary{Info: otherExecutable},
	)
	if err != nil {
		t.Fatalf("compare different executable: %v", err)
	}
	if !got.Stale || got.Method != wakeBinaryComparisonExactIdentity {
		t.Fatalf("different executable comparison = %#v", got)
	}
	if !got.Evidence.Available || got.Evidence.Running == got.Evidence.Current {
		t.Fatalf("different executable comparison omitted distinct identity evidence: %#v", got)
	}
}

func TestLinuxWakeBinaryComparisonReturnsUnknownWhenProcessExecutableCannotBeRead(t *testing.T) {
	current, err := os.Stat("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{PID: 999999999},
		resolvedWakeBinary{Info: current},
	)
	if err == nil {
		t.Fatal("missing process executable returned nil error")
	}
	if got.Stale {
		t.Fatalf("unknown executable reported stale: %#v", got)
	}
}
