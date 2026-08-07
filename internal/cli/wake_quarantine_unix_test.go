//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func TestWakeRestartQuarantineRefusesReplacedSnapshot(t *testing.T) {
	root := secureTempDirForTest(t)
	ensureCoopWakeMailboxForTest(t, root, "codex")
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	path := filepath.Join(agentDir.path, wakeRestartFileName)
	original := []byte(`{"schema":`)
	replacement := []byte(`{"schema":99`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	var expected wakeRestartRecordSnapshot
	if err := agentDir.withFD(func(dirfd int) error {
		var exists bool
		var readErr error
		expected, exists, readErr = readWakeRestartRecordSnapshotAt(dirfd, agentDir)
		if !exists || readErr == nil || expected.Object.FileInfo == nil {
			return fmt.Errorf("initial restart snapshot exists=%v err=%v", exists, readErr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldHook := beforeWakeRestartQuarantineRevalidation
	beforeWakeRestartQuarantineRevalidation = func(wakeRestartRecordSnapshot) {
		temp := path + ".replacement"
		if err := os.WriteFile(temp, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temp, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeWakeRestartQuarantineRevalidation = oldHook })

	err = withWakeLifecycleGuardInDir(agentDir, func(dirfd int) error {
		_, quarantineErr := quarantineWakeRestartRecordAt(dirfd, agentDir, expected)
		return quarantineErr
	})
	if err == nil || !strings.Contains(err.Error(), "changed before quarantine") {
		t.Fatalf("replacement quarantine error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("replacement restart raw=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(agentDir.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), wakeRestartFileName+".quarantined.") {
			t.Fatalf("replaced restart record was quarantined as %s", entry.Name())
		}
	}
}

func TestParseWakeRestartQuarantineName(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 34, 56, 789, time.UTC)
	name, err := wakeQuarantineName(wakeRestartFileName, now)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parseWakeQuarantineName(name)
	if !ok || !got.Equal(now) {
		t.Fatalf("parse restart quarantine %q = %s ok=%v", name, got, ok)
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

func TestTargetlessAcquisitionQuarantinesReadableMalformedOrphanTarget(t *testing.T) {
	for _, raw := range [][]byte{
		nil,
		[]byte(`{"schema":`),
		[]byte(`{"owner":null,"schema":`),
		[]byte(`{"ow\u006eer_note":"x","schema":`),
		[]byte(`{owner_note:`),
	} {
		t.Run(string(raw), func(t *testing.T) {
			root := secureTempDirForTest(t)
			agentDir := filepath.Join(root, "agents", "codex")
			if err := os.MkdirAll(agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(agentDir, wakeTargetFileName)
			if err := os.WriteFile(targetPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(targetPath)
			if err != nil {
				t.Fatal(err)
			}

			cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
			if err != nil {
				t.Fatalf("targetless acquisition after malformed target quarantine: %v", err)
			}
			t.Cleanup(cleanup)
			assertExactWakeQuarantineForTest(
				t,
				agentDir,
				".wake.target.quarantined.",
				raw,
				before,
			)
		})
	}
}

func TestTargetlessAcquisitionPreservesOwnerBearingOrphanTarget(t *testing.T) {
	root := secureTempDirForTest(t)
	agentDir := filepath.Join(root, "agents", "codex")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := mustNewWakeTargetForTest(
		t,
		root,
		"codex",
		writeExecutableForTest(t, "owner-orphan-injector"),
		[]string{"exec"},
	)
	target.Owner = &wakeOwner{
		PID:          4242,
		ProcessStart: "12345",
		BootID:       "11111111-1111-1111-1111-111111111111",
		SessionID:    99,
	}
	raw, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	assertTargetlessAcquisitionPreservesOwnerShapedTarget(
		t,
		root,
		agentDir,
		raw,
		"owner-bearing orphan target; run 'amq wake recover-owner --me codex'",
	)
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

func TestTargetlessAcquisitionPreservesMalformedOwnerShapedOrphanTarget(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{name: "lowercase", raw: []byte(`{"schema":1,"owner":{"pid":4242`)},
		{name: "uppercase", raw: []byte(`{"schema":1,"OWNER":{"pid":4242`)},
		{name: "escaped", raw: []byte(`{"schema":1,"ow\u006eer":{"pid":4242`)},
		{name: "truncated lowercase key", raw: []byte(`{"schema":1,"owner`)},
		{name: "truncated escaped key", raw: []byte(`{"schema":1,"ow\u006eer`)},
		{name: "terminated key without colon", raw: []byte(`{"schema":1,"owner"`)},
		{name: "bare lowercase", raw: []byte(`{owner:{`)},
		{name: "bare uppercase", raw: []byte(`{OWNER:`)},
		{name: "truncated bare", raw: []byte(`{owner`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			agentDir := filepath.Join(root, "agents", "codex")
			if err := os.MkdirAll(agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			assertTargetlessAcquisitionPreservesOwnerShapedTarget(
				t,
				root,
				agentDir,
				test.raw,
				"unverified owner-shaped orphan target",
			)
		})
	}
}

func assertTargetlessAcquisitionPreservesOwnerShapedTarget(
	t *testing.T,
	root string,
	agentDir string,
	raw []byte,
	wantError string,
) {
	t.Helper()
	targetPath := filepath.Join(agentDir, wakeTargetFileName)
	if err := os.WriteFile(targetPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(targetPath)
	if err != nil {
		t.Fatal(err)
	}

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
	if cleanup != nil {
		cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), wantError) {
		t.Fatalf("targetless owner-shaped acquisition error = %v, want %q", err, wantError)
	}
	afterRaw, readErr := os.ReadFile(targetPath)
	after, statErr := os.Lstat(targetPath)
	if readErr != nil || statErr != nil || !bytes.Equal(afterRaw, raw) || !os.SameFile(before, after) {
		t.Fatalf(
			"owner-shaped orphan target changed: raw=%q read_err=%v info=%v stat_err=%v",
			afterRaw,
			readErr,
			after,
			statErr,
		)
	}
	if _, lockErr := os.Lstat(filepath.Join(agentDir, ".wake.lock")); !os.IsNotExist(lockErr) {
		t.Fatalf("targetless owner-shaped refusal created a wake lock: %v", lockErr)
	}
	entries, readDirErr := os.ReadDir(agentDir)
	if readDirErr != nil {
		t.Fatal(readDirErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".wake.target.quarantined.") {
			t.Fatalf("owner-shaped orphan target was quarantined as %s", entry.Name())
		}
	}
}

func TestTargetlessAcquisitionPreservesIneligibleOrphanTargets(t *testing.T) {
	for _, kind := range []string{"wrong-mode", "oversized", "symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := secureTempDirForTest(t)
			agentDir := filepath.Join(root, "agents", "codex")
			if err := os.MkdirAll(agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(agentDir, wakeTargetFileName)
			switch kind {
			case "wrong-mode":
				if err := os.WriteFile(targetPath, []byte(`{"schema":`), 0o400); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(targetPath, bytes.Repeat([]byte("x"), maxWakeMetadataFileBytes+1), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte(`{"schema":`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, targetPath); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(targetPath, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(targetPath)
			if err != nil {
				t.Fatal(err)
			}
			cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
			if cleanup != nil {
				cleanup()
			}
			if err == nil {
				t.Fatalf("targetless acquisition accepted %s orphan target", kind)
			}
			after, statErr := os.Lstat(targetPath)
			if statErr != nil || !os.SameFile(before, after) {
				t.Fatalf("%s orphan target changed: info=%v err=%v", kind, after, statErr)
			}
			entries, readErr := os.ReadDir(agentDir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".wake.target.quarantined.") {
					t.Fatalf("%s orphan target was quarantined", kind)
				}
			}
		})
	}
}

func TestAcquirePreservesIneligibleMalformedWakeLocks(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		mode os.FileMode
		aged bool
	}{
		{name: "fresh", raw: []byte(`{"pid":`), mode: 0o600},
		{name: "owner mode", raw: []byte(`{"pid":`), mode: wakeOwnerLockFileMode, aged: true},
		{name: "owner shaped", raw: []byte(`{"owner_schema":1,`), mode: 0o600, aged: true},
		{name: "escaped owner shape", raw: []byte(`{"ow\u006eer_schema":1,`), mode: 0o600, aged: true},
		{name: "valid JSON wrong known type", raw: []byte(`{"pid":"wrong","tty":"","root":"","started":""}`), mode: 0o600, aged: true},
		{name: "oversized", raw: bytes.Repeat([]byte("x"), maxWakeMetadataFileBytes+1), mode: 0o600, aged: true},
		{name: "wrong mode", raw: []byte(`{"pid":`), mode: 0o000, aged: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := secureTempDirForTest(t)
			agentDir := filepath.Join(root, "agents", "codex")
			if err := os.MkdirAll(agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(agentDir, ".wake.lock")
			if err := os.WriteFile(lockPath, test.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(lockPath, test.mode); err != nil {
				t.Fatal(err)
			}
			if test.aged {
				past := time.Now().Add(-3 * time.Second)
				if err := os.Chtimes(lockPath, past, past); err != nil {
					t.Fatal(err)
				}
			}
			beforeInfo, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}

			cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
			if cleanup != nil {
				cleanup()
			}
			if err == nil {
				t.Fatal("acquisition accepted ineligible malformed wake lock")
			}
			afterInfo, statErr := os.Lstat(lockPath)
			if statErr != nil {
				t.Fatalf("ineligible malformed lock was moved: %v", statErr)
			}
			if !os.SameFile(beforeInfo, afterInfo) {
				t.Fatal("ineligible malformed lock inode changed")
			}
			entries, readErr := os.ReadDir(agentDir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".wake.lock.quarantined.") {
					t.Fatalf("ineligible malformed lock was quarantined as %s", entry.Name())
				}
			}
		})
	}
}

func TestMalformedLockQuarantineNoReplaceCollisionPreservesSource(t *testing.T) {
	root := secureTempDirForTest(t)
	agentDir := filepath.Join(root, "agents", "codex")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(agentDir, ".wake.lock")
	raw := []byte(`{"pid":`)
	if err := os.WriteFile(lockPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-3 * time.Second)
	if err := os.Chtimes(lockPath, past, past); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 8, 4, 12, 0, 0, 123456789, time.UTC)
	originalNow := wakeQuarantineNow
	wakeQuarantineNow = func() time.Time { return fixed }
	t.Cleanup(func() { wakeQuarantineNow = originalNow })
	name, err := wakeQuarantineName(".wake.lock", fixed)
	if err != nil {
		t.Fatal(err)
	}
	collisionPath := filepath.Join(agentDir, name)
	if err := os.WriteFile(collisionPath, []byte("existing quarantine"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("quarantine collision unexpectedly allowed acquisition")
	}
	afterRaw, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	after, statErr := os.Lstat(lockPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if !bytes.Equal(afterRaw, raw) || !os.SameFile(before, after) {
		t.Fatal("quarantine collision changed source lock")
	}
	if got, readErr := os.ReadFile(collisionPath); readErr != nil || string(got) != "existing quarantine" {
		t.Fatalf("quarantine collision target changed: %q err=%v", got, readErr)
	}
}

func TestAcquirePreservesSpecialWakeLocks(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := secureTempDirForTest(t)
			agentDir := filepath.Join(root, "agents", "codex")
			if err := os.MkdirAll(agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(agentDir, ".wake.lock")
			switch kind {
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte(`{"pid":`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, lockPath); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(lockPath, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
			if cleanup != nil {
				cleanup()
			}
			if err == nil {
				t.Fatalf("acquisition accepted %s wake lock", kind)
			}
			if _, err := os.Lstat(lockPath); err != nil {
				t.Fatalf("%s wake lock was removed: %v", kind, err)
			}
			entries, err := os.ReadDir(agentDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".wake.lock.quarantined.") {
					t.Fatalf("%s wake lock was quarantined", kind)
				}
			}
		})
	}
}
