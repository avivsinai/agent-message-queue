//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func assertExactWakeQuarantineForTest(
	t *testing.T,
	agentDir string,
	prefix string,
	wantRaw []byte,
	wantInfo os.FileInfo,
) {
	t.Helper()
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	var path string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			if path != "" {
				t.Fatalf("multiple quarantine artifacts with prefix %q", prefix)
			}
			path = filepath.Join(agentDir, entry.Name())
		}
	}
	if path == "" {
		t.Fatalf("missing quarantine artifact with prefix %q", prefix)
	}
	gotRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gotInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotRaw, wantRaw) || !os.SameFile(gotInfo, wantInfo) {
		t.Fatal("quarantine artifact did not preserve exact inode/raw")
	}
}

func TestAcquireQuarantinesAgedSyntaxInvalidGenericWakeLock(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(`{"pid":`), []byte(`not-json`)} {
		t.Run(string(raw), func(t *testing.T) {
			root := secureTempDirForTest(t)
			agentDir := filepath.Join(root, "agents", "codex")
			if err := os.MkdirAll(agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(agentDir, ".wake.lock")
			if err := os.WriteFile(lockPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			past := time.Now().Add(-3 * time.Second)
			if err := os.Chtimes(lockPath, past, past); err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}

			cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
			if err != nil {
				t.Fatalf("acquire after malformed lock quarantine: %v", err)
			}
			t.Cleanup(cleanup)
			assertExactWakeQuarantineForTest(
				t,
				agentDir,
				".wake.lock.quarantined.",
				raw,
				beforeInfo,
			)
			var replacement wakeLock
			replacementRaw, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(replacementRaw, &replacement); err != nil || replacement.PID != os.Getpid() {
				t.Fatalf("replacement wake lock = %#v err=%v", replacement, err)
			}
		})
	}
}

func TestRecoverOwnerClearsDeadOwnerBearingOrphanTarget(t *testing.T) {
	root := secureTempDirForTest(t)
	agentDir := filepath.Join(root, "agents", "codex")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := mustNewWakeTargetForTest(
		t,
		root,
		"codex",
		writeExecutableForTest(t, "dead-owner-orphan-injector"),
		nil,
	)
	target.Owner = &wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	wakeDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wakeDir.Close() })
	fixture := wakeStateUnixFixture{
		root:     root,
		agent:    "codex",
		injector: target.InjectVia,
		agentDir: wakeDir,
	}
	if _, err := publishWakeStateForTest(fixture, captureWakeStateLegacyForTest(t, fixture)); err != nil {
		t.Fatalf("publish orphan target state: %v", err)
	}

	originalObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(got wakeOwner) (wakeOwnerObservation, error) {
		if !sameWakeOwner(&got, target.Owner) {
			t.Fatalf("observed owner = %#v, want %#v", got, *target.Owner)
		}
		return wakeOwnerObservation{State: wakeOwnerDead}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = originalObserve })

	result, err := recoverOwnerWake(root, "codex")
	if err != nil || result.Status != "recovered" {
		t.Fatalf("recover dead owner-bearing orphan = %#v err=%v", result, err)
	}
	if _, statErr := os.Lstat(filepath.Join(agentDir, wakeTargetFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("recovered orphan target still exists: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(agentDir, wakeStateFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("recovered orphan target state still exists: %v", statErr)
	}
	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
	if err != nil {
		t.Fatalf("advertised targetless acquisition after recover-owner: %v", err)
	}
	t.Cleanup(cleanup)
}

func TestRecoverOwnerPreservesLiveOwnerBearingOrphanTarget(t *testing.T) {
	root := secureTempDirForTest(t)
	agentDir := filepath.Join(root, "agents", "codex")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := mustNewWakeTargetForTest(
		t,
		root,
		"codex",
		writeExecutableForTest(t, "live-owner-orphan-injector"),
		nil,
	)
	target.Owner = &wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(agentDir, wakeTargetFileName)
	before, err := os.Lstat(targetPath)
	if err != nil {
		t.Fatal(err)
	}

	originalObserve := observeAuthoritativeWakeOwner
	observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
		return wakeOwnerObservation{State: wakeOwnerSame}, nil
	}
	t.Cleanup(func() { observeAuthoritativeWakeOwner = originalObserve })

	result, err := recoverOwnerWake(root, "codex")
	if err == nil || result.Status != "refused" || !strings.Contains(result.Reason, "still live") {
		t.Fatalf("recover live owner-bearing orphan = %#v err=%v", result, err)
	}
	after, statErr := os.Lstat(targetPath)
	if statErr != nil || !os.SameFile(before, after) {
		t.Fatalf("live owner-bearing orphan target changed: info=%v err=%v", after, statErr)
	}
}
