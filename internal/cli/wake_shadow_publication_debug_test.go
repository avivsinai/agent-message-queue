//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestGenericNoLockShadowPublicationDebugNote(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "generic-shadow-debug-injector")
	shadow := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"shadow"})
	if err := writeWakeTarget(root, "codex", shadow); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	fixture := wakeStateUnixFixture{root: root, agent: "codex", agentDir: agentDir}
	if _, err := publishWakeStateForTest(fixture, captureWakeStateLegacyForTest(t, fixture)); err != nil {
		t.Fatal(err)
	}

	replacement := shadow
	replacement.Created = "2026-08-02T21:00:00Z"
	digest, err := wakeTargetDigest(shadow)
	if err != nil {
		t.Fatal(err)
	}
	var cleanup func()
	var acquireErr error
	stderr := captureWakeStderr(t, func() {
		cleanup, acquireErr = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target:   &replacement,
			wakeMode: wakeTargetInjectVia,
			debug:    true,
		})
	})
	if acquireErr != nil {
		t.Fatal(acquireErr)
	}
	defer cleanup()
	assertWakeShadowPublicationDebugNote(t, stderr, "generic", digest)
}

func TestAuthoritativeNoLockShadowPublicationDebugNote(t *testing.T) {
	root, shadow, owner := newOwnerAcquisitionPublicationFixture(t)
	if err := writeWakeTarget(root, "codex", shadow); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = agentDir.Close() }()
	fixture := wakeStateUnixFixture{root: root, agent: "codex", agentDir: agentDir}
	if _, err := publishWakeStateForTest(fixture, captureWakeStateLegacyForTest(t, fixture)); err != nil {
		t.Fatal(err)
	}

	replacement := shadow
	replacement.Created = "2026-08-02T21:01:00Z"
	digest, err := wakeTargetDigest(shadow)
	if err != nil {
		t.Fatal(err)
	}
	var cleanup func()
	var acquireErr error
	stderr := captureWakeStderr(t, func() {
		cleanup, acquireErr = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target:   &replacement,
			wakeMode: wakeTargetInjectVia,
			debug:    true,
		})
	})
	if acquireErr != nil {
		t.Fatal(acquireErr)
	}
	defer cleanup()
	if !sameWakeOwner(replacement.Owner, &owner) {
		t.Fatalf("replacement owner changed: %#v", replacement.Owner)
	}
	assertWakeShadowPublicationDebugNote(t, stderr, "authoritative", digest)
}

func TestNoLockShadowPublicationDebugNoteIsSilentByDefault(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "silent-shadow-debug-injector")
	shadow := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"shadow"})
	if err := writeWakeTarget(root, "codex", shadow); err != nil {
		t.Fatal(err)
	}

	replacement := shadow
	replacement.Created = "2026-08-02T21:02:00Z"
	var cleanup func()
	var acquireErr error
	stderr := captureWakeStderr(t, func() {
		cleanup, acquireErr = acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
			target:   &replacement,
			wakeMode: wakeTargetInjectVia,
		})
	})
	if acquireErr != nil {
		t.Fatal(acquireErr)
	}
	defer cleanup()
	if strings.Contains(stderr, "superseding no-lock wake shadow") {
		t.Fatalf("default publication emitted debug shadow note: %q", stderr)
	}
}

func TestGenericNoLockShadowPublicationSurfacesDebugWriteError(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := writeExecutableForTest(t, "shadow-debug-error-injector")
	shadow := mustNewWakeTargetForTest(t, root, "codex", injector, []string{"shadow"})
	if err := writeWakeTarget(root, "codex", shadow); err != nil {
		t.Fatal(err)
	}

	replacement := shadow
	replacement.Created = "2026-08-04T00:00:00Z"
	readOnlyStderr, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readOnlyStderr.Close() }()
	oldStderr := os.Stderr
	os.Stderr = readOnlyStderr
	defer func() { os.Stderr = oldStderr }()

	cleanup, acquireErr := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{
		target:   &replacement,
		wakeMode: wakeTargetInjectVia,
		debug:    true,
	})
	if cleanup != nil {
		cleanup()
		t.Fatal("debug write failure returned a live wake cleanup")
	}
	if acquireErr == nil {
		t.Fatal("debug write failure was ignored")
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !sameWakeTarget(persisted, shadow) {
		t.Fatalf("shadow target changed after diagnostic failure: target=%#v exists=%v", persisted, exists)
	}
	if inspection := inspectWakeLock(root, "codex"); inspection.Exists {
		t.Fatalf("wake lock published after diagnostic failure: %#v", inspection)
	}
}

func assertWakeShadowPublicationDebugNote(t *testing.T, stderr, kind, digest string) {
	t.Helper()
	want := fmt.Sprintf(
		"amq wake [debug]: %s publication superseding no-lock wake shadow target_digest=%s state_target_digest=%s",
		kind,
		digest,
		digest,
	)
	if count := strings.Count(stderr, want); count != 1 {
		t.Fatalf("shadow publication debug note count=%d stderr=%q want=%q", count, stderr, want)
	}
	if strings.Contains(stderr, ".wake.target") || strings.Contains(stderr, "injector") {
		t.Fatalf("shadow publication debug note leaked path/body detail: %q", stderr)
	}
}
