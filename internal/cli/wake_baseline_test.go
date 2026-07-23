//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func writeWakeTestMessage(t *testing.T, root, me, filename, id, subject, body string) {
	t.Helper()
	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    "claude",
			To:      []string{me},
			Thread:  "p2p/claude__" + me,
			Subject: subject,
			Created: time.Now().UTC().Format(time.RFC3339Nano),
		},
		Body: body,
	}
	data, err := msg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, me), filename), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureWakeBaselineContainsIdentifiersOnly(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	writeWakeTestMessage(t, root, "codex", "stale.md", "msg-stale", "secret subject", "secret body")

	baseline, err := captureWakeBaseline(root, "codex")
	if err != nil {
		t.Fatalf("captureWakeBaseline: %v", err)
	}
	info, err := os.Lstat(baseline.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("baseline mode = %v", info.Mode())
	}
	data, err := os.ReadFile(baseline.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret subject") || strings.Contains(string(data), "secret body") {
		t.Fatalf("manifest leaked message content: %s", data)
	}
	if !strings.Contains(string(data), "msg-stale") || !strings.Contains(string(data), "stale.md") {
		t.Fatalf("manifest omitted message identifiers: %s", data)
	}
	if baseline.Manifest.RootID == "" || baseline.Manifest.LaunchID == "" {
		t.Fatalf("manifest identity binding incomplete: %#v", baseline.Manifest)
	}
}

func TestReadWakeBaselineRejectsSecurityAndIdentityMismatches(t *testing.T) {
	root := secureTempDirForTest(t)
	otherRoot := secureTempDirForTest(t)
	for _, candidate := range []string{root, otherRoot} {
		if err := fsq.EnsureRootDirs(candidate); err != nil {
			t.Fatal(err)
		}
		if err := fsq.EnsureAgentDirs(candidate, "codex"); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := captureWakeBaseline(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readWakeBaseline(baseline.Path, root, "claude"); err == nil {
		t.Fatalf("wrong-agent error = %v", err)
	}
	if _, err := readWakeBaseline(baseline.Path, otherRoot, "codex"); err == nil || !strings.Contains(err.Error(), "exact agent root") {
		t.Fatalf("wrong-root error = %v", err)
	}
	if err := os.Chmod(baseline.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWakeBaseline(baseline.Path, root, "codex"); err == nil || !strings.Contains(err.Error(), "want 0600") {
		t.Fatalf("mode error = %v", err)
	}
	if err := os.Chmod(baseline.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(fsq.AgentBase(root, "codex"), wakeBaselineFilePrefix+strings.Repeat("a", 32)+wakeBaselineFileSuffix)
	if err := os.Symlink(baseline.Path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readWakeBaseline(link, root, "codex"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestWakeBaselineManifestRejectsRootIdentityTampering(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	baseline, err := captureWakeBaseline(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	manifest := baseline.Manifest
	manifest.RootID = "v1:darwin:0:0"
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(baseline.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWakeBaseline(baseline.Path, root, "codex"); err == nil || !strings.Contains(err.Error(), "root identity mismatch") {
		t.Fatalf("identity error = %v", err)
	}
}

func TestWakeBaselineExplicitCleanupPreservesTargetAndSkipsSymlink(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	protected, err := captureWakeBaseline(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "injector")
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.BaselineFile = protected.Path
	target.BaselineDigest = protected.Digest
	if err := writeWakeTarget(root, "codex", target); err != nil {
		t.Fatal(err)
	}
	orphan, err := captureWakeBaseline(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(fsq.AgentBase(root, "codex"), wakeBaselineFilePrefix+strings.Repeat("b", 32)+wakeBaselineFileSuffix)
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}

	removeWakeBaselineIfUnreferenced(root, "codex", orphan.Path)
	removeWakeBaselineIfUnreferenced(root, "codex", protected.Path)
	removeWakeBaselineIfUnreferenced(root, "codex", symlink)
	if _, err := os.Stat(orphan.Path); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
	if _, err := os.Stat(protected.Path); err != nil {
		t.Fatalf("target baseline removed: %v", err)
	}
	if _, err := os.Lstat(symlink); err != nil {
		t.Fatalf("symlink was removed: %v", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("outside target changed: %q %v", data, err)
	}
}

func TestNotifyNewMessagesSkipsCorruptPrelaunchBaselineByFilename(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatal(err)
	}
	corruptPath := filepath.Join(fsq.AgentInboxNew(root, "alice"), "corrupt.md")
	if err := os.WriteFile(corruptPath, []byte("not a message"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, err := captureWakeBaseline(root, "alice")
	if err != nil {
		t.Fatal(err)
	}
	cfg, outputPath := injectViaCaptureConfig(t)
	cfg.root = root
	cfg.me = "alice"
	cfg.baseline = &baseline
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notifyNewMessages: %v", err)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("corrupt prelaunch message triggered injection: %v", err)
	}
}

func TestWakeLoopClosesPrelaunchGapBeforePublishingReadiness(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	writeWakeTestMessage(t, root, "codex", "stale.md", "msg-stale", "stale floor", "body")
	baseline, err := captureWakeBaseline(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	// This is the launch gap: the floor already exists, but wake has not yet
	// installed its watcher.
	writeWakeTestMessage(t, root, "codex", "gap.md", "msg-gap", "launch gap", "body")

	toolDir := secureTempDirForTest(t)
	outputPath := filepath.Join(toolDir, "injections.log")
	injector := filepath.Join(toolDir, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nprintf '%s\\n' \"$2\" >> \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{outputPath})
	target.BaselineFile = baseline.Path
	target.BaselineDigest = baseline.Digest
	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	readyPath := filepath.Join(secureTempDirForTest(t), "wake.ready")
	stop := make(chan struct{})
	cfg := wakeConfig{
		me:              "codex",
		root:            root,
		session:         "gap-test",
		injectVia:       injector,
		injectArgs:      []string{outputPath},
		injectTimeout:   time.Second,
		previewLen:      200,
		baseline:        &baseline,
		seen:            make(map[string]struct{}),
		readyFile:       readyPath,
		readyInspection: inspectWakeLock(root, "codex"),
		controlStop:     stop,
	}
	done := make(chan error, 1)
	go func() { done <- runWakeLoop(cfg) }()
	stopped := false
	defer func() {
		if !stopped {
			close(stop)
			<-done
		}
	}()
	waitForTestCondition(t, 3*time.Second, func() bool {
		_, statErr := os.Stat(readyPath)
		return statErr == nil
	}, "wake readiness after launch-gap catch-up")
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "launch gap") != 1 || strings.Contains(string(data), "stale floor") {
		t.Fatalf("initial catch-up output = %q", data)
	}

	writeWakeTestMessage(t, root, "codex", "post.md", "msg-post", "post ready", "body")
	waitForTestCondition(t, 3*time.Second, func() bool {
		data, readErr := os.ReadFile(outputPath)
		return readErr == nil && strings.Contains(string(data), "post ready")
	}, "post-ready watcher notification")
	close(stop)
	stopped = true
	if err := <-done; err != nil {
		t.Fatalf("runWakeLoop: %v", err)
	}
	data, err = os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "launch gap") != 1 || strings.Count(string(data), "post ready") != 1 {
		t.Fatalf("messages were not notified exactly once: %q", data)
	}
	for _, name := range []string{"stale.md", "gap.md", "post.md"} {
		if _, err := os.Stat(filepath.Join(fsq.AgentInboxNew(root, "codex"), name)); err != nil {
			t.Fatalf("wake consumed %s: %v", name, err)
		}
	}
	receipts, err := os.ReadDir(fsq.AgentReceipts(root, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 0 {
		t.Fatalf("wake emitted receipts: %v", receipts)
	}
}

func TestWakeLoopDoesNotPublishReadyWhenCatchupInjectionFails(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	baseline, err := captureWakeBaseline(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	writeWakeTestMessage(t, root, "codex", "gap.md", "msg-gap", "must fail", "body")
	injector := filepath.Join(secureTempDirForTest(t), "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, nil)
	target.BaselineFile = baseline.Path
	target.BaselineDigest = baseline.Digest
	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	readyPath := filepath.Join(secureTempDirForTest(t), "wake.ready")
	cfg := wakeConfig{
		me: "codex", root: root, injectVia: injector, injectTimeout: time.Second,
		baseline: &baseline, seen: make(map[string]struct{}), readyFile: readyPath,
		readyInspection: inspectWakeLock(root, "codex"),
	}
	err = runWakeLoop(cfg)
	if err == nil || !strings.Contains(err.Error(), "initial wake catch-up") {
		t.Fatalf("runWakeLoop error = %v", err)
	}
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatalf("ready file exists after failed catch-up: %v", statErr)
	}
	if _, statErr := os.Stat(wakeCatchupReadyPath(root, "codex")); !os.IsNotExist(statErr) {
		t.Fatalf("persistent catch-up readiness exists after failed catch-up: %v", statErr)
	}
	if len(cfg.seen) != 0 {
		t.Fatalf("failed catch-up acknowledged messages: %#v", cfg.seen)
	}
}

func TestWakeLoopMixedUrgentAndNormalCatchupCompletesBeforeReady(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	baseline, err := captureWakeBaseline(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	writeWakeTestMessage(t, root, "codex", "normal.md", "msg-normal", "normal gap", "body")
	urgent := format.Message{
		Header: format.Header{
			Schema: format.CurrentSchema, ID: "msg-urgent", From: "claude", To: []string{"codex"},
			Thread: "p2p/claude__codex", Subject: "urgent gap", Created: time.Now().UTC().Format(time.RFC3339Nano),
			Priority: "urgent", Labels: []string{"interrupt"},
		},
		Body: "body",
	}
	data, err := urgent.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fsq.AgentInboxNew(root, "codex"), "urgent.md"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	toolDir := secureTempDirForTest(t)
	outputPath := filepath.Join(toolDir, "injections.log")
	injector := filepath.Join(toolDir, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nprintf '%s\\n' \"$2\" >> \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := mustNewWakeTargetForTest(t, root, "codex", injector, []string{outputPath})
	target.BaselineFile = baseline.Path
	target.BaselineDigest = baseline.Digest
	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{target: &target, wakeMode: wakeTargetInjectVia})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	readyPath := filepath.Join(secureTempDirForTest(t), "wake.ready")
	stop := make(chan struct{})
	cfg := wakeConfig{
		me: "codex", root: root, session: "mixed", injectVia: injector, injectArgs: []string{outputPath},
		injectTimeout: time.Second, previewLen: 200, interrupt: true, interruptLabel: "interrupt",
		interruptPriority: "urgent", baseline: &baseline, seen: make(map[string]struct{}), readyFile: readyPath,
		readyInspection: inspectWakeLock(root, "codex"), controlStop: stop,
	}
	done := make(chan error, 1)
	go func() { done <- runWakeLoop(cfg) }()
	stopped := false
	defer func() {
		if !stopped {
			close(stop)
			<-done
		}
	}()
	waitForTestCondition(t, 3*time.Second, func() bool {
		_, statErr := os.Stat(readyPath)
		return statErr == nil
	}, "mixed catch-up readiness")
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "urgent gap") || !strings.Contains(string(output), "normal gap") {
		t.Fatalf("readiness preceded complete mixed catch-up: %q", output)
	}
	if ready, err := wakeCatchupReadyMatches(root, "codex", cfg.readyInspection); err != nil || !ready {
		t.Fatalf("persistent catch-up readiness = %v, err=%v", ready, err)
	}
	close(stop)
	stopped = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNotifyNewMessagesIgnoresValidDrainedDuplicate(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatal(err)
	}
	writeWakeTestMessage(t, root, "alice", "duplicate.md", "msg-drained", "already drained", "body")
	if err := receipt.Emit(root, receipt.New("msg-drained", "", "claude", "alice", receipt.StageDrained, "")); err != nil {
		t.Fatal(err)
	}
	cfg, outputPath := injectViaCaptureConfig(t)
	cfg.root = root
	cfg.me = "alice"
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("drained duplicate triggered injection: %v", err)
	}
}

func TestAcceptExistingWakeRequiresExactBaselineFloor(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatal(err)
	}
	first, err := captureWakeBaseline(root, "orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	second, err := captureWakeBaseline(root, "orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "injector")
	owner := setExactWakeOwnerEnvForTest(t)
	target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
	target.Owner = &owner
	target.BaselineFile = first.Path
	target.BaselineDigest = first.Digest
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case wakePID:
			return wakeProcessInfo{
				PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-via", injector, "--baseline-file", first.Path},
			}
		case owner.PID:
			return wakeProcessInfoForOwnerTest(owner)
		default:
			return wakeProcessInfo{PID: pid}
		}
	})
	writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
		PID: wakePID, TTY: "unknown", ProcessStart: "start-1", BootID: "boot-1",
		Executable: "/opt/homebrew/bin/amq", Generation: "generation-1",
	}, target))
	if err := writeWakeTarget(root, "orchestrator", target); err != nil {
		t.Fatal(err)
	}
	inspection := inspectWakeLock(root, "orchestrator")
	if err := waitForWakeCatchupReady(root, "orchestrator", inspection, 40*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not attest") {
		t.Fatalf("missing catch-up attestation error = %v", err)
	}
	if err := os.WriteFile(wakeCatchupReadyPath(root, "orchestrator"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForWakeCatchupReady(root, "orchestrator", inspection, 40*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not attest") {
		t.Fatalf("corrupt catch-up attestation error = %v", err)
	}
	if err := writeWakeCatchupReadyFile(root, "orchestrator", inspection); err != nil {
		t.Fatal(err)
	}

	wrongReady := filepath.Join(secureTempDirForTest(t), "wrong.ready")
	err = runWakeWithLoop([]string{
		"--root", root, "--me", "orchestrator", "--inject-via", injector,
		"--inject-arg", "exec", "--baseline-file", second.Path,
		"--ready-file", wrongReady, "--accept-existing-wake",
	}, func(wakeConfig) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "baseline floor") {
		t.Fatalf("different floor error = %v", err)
	}
	if _, statErr := os.Stat(wrongReady); !os.IsNotExist(statErr) {
		t.Fatalf("wrong floor published readiness: %v", statErr)
	}
	persisted, exists, err := readWakeTarget(root, "orchestrator")
	if err != nil || !exists {
		t.Fatalf("read persisted target: exists=%v err=%v", exists, err)
	}
	if persisted.BaselineFile != first.Path || persisted.BaselineDigest != first.Digest {
		t.Fatalf("wrong floor replaced target: %#v", persisted)
	}

	exactReady := filepath.Join(secureTempDirForTest(t), "exact.ready")
	err = runWakeWithLoop([]string{
		"--root", root, "--me", "orchestrator", "--inject-via", injector,
		"--inject-arg", "exec", "--baseline-file", first.Path,
		"--ready-file", exactReady, "--accept-existing-wake",
	}, func(wakeConfig) error { return nil })
	if err != nil {
		t.Fatalf("exact floor reuse: %v", err)
	}
	if _, statErr := os.Stat(exactReady); statErr != nil {
		t.Fatalf("exact floor did not publish readiness: %v", statErr)
	}
}

func TestWakeBaselineRotationAndConcurrentExistingSemantics(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "orchestrator"); err != nil {
		t.Fatal(err)
	}
	first, err := captureWakeBaseline(root, "orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	second, err := captureWakeBaseline(root, "orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	injector := writeExecutableForTest(t, "injector")
	owner := setExactWakeOwnerEnvForTest(t)
	withBaseline := func(baseline wakeBaseline, args ...string) wakeTarget {
		target := mustNewWakeTargetForTest(t, root, "orchestrator", injector, args)
		target.Owner = &owner
		target.BaselineFile = baseline.Path
		target.BaselineDigest = baseline.Digest
		return target
	}
	firstTarget := withBaseline(first, "exec")
	secondTarget := withBaseline(second, "exec")
	// Both starters completed their precheck/capture before either staged a
	// target. The acquire transition below reproduces the winner appearing in
	// that gap and proves the loser reuses it instead of rotating it.
	for _, provisional := range []wakeTarget{firstTarget, secondTarget} {
		if _, found, err := persistedExactWakeBaseline(root, "orchestrator", provisional); err != nil || found {
			t.Fatalf("provisional starter precheck found=%v err=%v, want clean miss", found, err)
		}
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		switch pid {
		case wakePID:
			return wakeProcessInfo{
				PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1",
				Executable: "/opt/homebrew/bin/amq",
				Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-via", injector},
			}
		case owner.PID:
			return wakeProcessInfoForOwnerTest(owner)
		default:
			return wakeProcessInfo{PID: pid}
		}
	})
	installExisting := func(target wakeTarget) {
		_ = os.Remove(filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock"))
		if err := writeWakeTarget(root, "orchestrator", target); err != nil {
			t.Fatal(err)
		}
		writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
			PID: wakePID, TTY: "unknown", ProcessStart: "start-1", BootID: "boot-1",
			Executable: "/opt/homebrew/bin/amq", Generation: "generation-old",
		}, target))
	}

	oldReplace := replaceExistingWakeLock
	replacementCalls := 0
	replaceExistingWakeLock = func(inspection wakeLockInspection) (bool, error) {
		replacementCalls++
		if err := os.Remove(inspection.LockPath); err != nil {
			return false, err
		}
		return true, nil
	}
	t.Cleanup(func() { replaceExistingWakeLock = oldReplace })

	t.Run("concurrent baseline-existing reuses winner floor", func(t *testing.T) {
		installExisting(firstTarget)
		replacementCalls = 0
		_, err := acquireWakeLockWithOptions(root, "orchestrator", wakeLockAcquireOptions{
			acceptExistingValid: true, materializeLegacyBaseline: true,
			target: &secondTarget, wakeMode: wakeTargetInjectVia,
		})
		var alreadyRunning *wakeAlreadyRunningError
		if !errors.As(err, &alreadyRunning) {
			t.Fatalf("acquire error = %v, want existing winner", err)
		}
		if replacementCalls != 0 {
			t.Fatalf("provisional floor rotated a concurrent winner %d times", replacementCalls)
		}
		persisted, exists, readErr := readWakeTarget(root, "orchestrator")
		if readErr != nil || !exists || !sameWakeInjectorIdentity(persisted, firstTarget) {
			t.Fatalf("persisted winner changed: exists=%v target=%#v err=%v", exists, persisted, readErr)
		}
	})

	t.Run("legacy target without floor migrates once", func(t *testing.T) {
		legacy := mustNewWakeTargetForTest(t, root, "orchestrator", injector, []string{"exec"})
		legacy.Owner = &owner
		installExisting(legacy)
		replacementCalls = 0
		cleanup, err := acquireWakeLockWithOptions(root, "orchestrator", wakeLockAcquireOptions{
			acceptExistingValid: true, materializeLegacyBaseline: true,
			target: &secondTarget, wakeMode: wakeTargetInjectVia,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if replacementCalls != 1 {
			t.Fatalf("legacy migration replacements = %d, want 1", replacementCalls)
		}
		persisted, exists, readErr := readWakeTarget(root, "orchestrator")
		if readErr != nil || !exists || !sameWakeInjectorIdentity(persisted, secondTarget) {
			t.Fatalf("legacy target did not migrate exactly: exists=%v target=%#v err=%v", exists, persisted, readErr)
		}
	})

	t.Run("explicit floor replacement rotates same transport", func(t *testing.T) {
		installExisting(firstTarget)
		replacementCalls = 0
		cleanup, err := acquireWakeLockWithOptions(root, "orchestrator", wakeLockAcquireOptions{
			acceptExistingValid: true, replaceExistingBaseline: true,
			target: &secondTarget, wakeMode: wakeTargetInjectVia,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if replacementCalls != 1 {
			t.Fatalf("exact floor replacements = %d, want 1", replacementCalls)
		}
	})

	t.Run("explicit replacement refuses different arguments", func(t *testing.T) {
		installExisting(firstTarget)
		replacementCalls = 0
		different := withBaseline(second, "different")
		_, err := acquireWakeLockWithOptions(root, "orchestrator", wakeLockAcquireOptions{
			acceptExistingValid: true, replaceExistingBaseline: true,
			target: &different, wakeMode: wakeTargetInjectVia,
		})
		if err == nil {
			t.Fatal("different transport unexpectedly replaced the existing wake")
		}
		if replacementCalls != 0 {
			t.Fatalf("different transport replacement calls = %d", replacementCalls)
		}
	})

	t.Run("explicit replacement refuses different injector path", func(t *testing.T) {
		installExisting(firstTarget)
		replacementCalls = 0
		otherInjector := writeExecutableForTest(t, "other-injector")
		different := mustNewWakeTargetForTest(t, root, "orchestrator", otherInjector, []string{"exec"})
		different.Owner = &owner
		different.BaselineFile = second.Path
		different.BaselineDigest = second.Digest
		_, err := acquireWakeLockWithOptions(root, "orchestrator", wakeLockAcquireOptions{
			acceptExistingValid: true, replaceExistingBaseline: true,
			target: &different, wakeMode: wakeTargetInjectVia,
		})
		if err == nil || replacementCalls != 0 {
			t.Fatalf("different injector error=%v replacement calls=%d", err, replacementCalls)
		}
	})

	t.Run("explicit replacement refuses different owner", func(t *testing.T) {
		const existingOwnerPID = 5001
		const requestedOwnerPID = 5002
		stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
			switch pid {
			case wakePID:
				return wakeProcessInfo{
					PID: pid, Running: true, StartToken: "start-1", BootID: "boot-1",
					Executable: "/opt/homebrew/bin/amq",
					Args:       []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator", "--inject-via", injector},
				}
			case existingOwnerPID:
				return wakeProcessInfo{PID: pid, Running: true, StartToken: "owner-a", BootID: "boot-1"}
			case requestedOwnerPID:
				return wakeProcessInfo{PID: pid, Running: true, StartToken: "owner-b", BootID: "boot-1"}
			default:
				return wakeProcessInfo{PID: pid}
			}
		})
		owner := wakeOwner{PID: existingOwnerPID, ProcessStart: "owner-a", BootID: "boot-1"}
		existing := firstTarget
		existing.Owner = &owner
		installExisting(existing)
		replacementCalls = 0
		requestedOwner := wakeOwner{PID: requestedOwnerPID, ProcessStart: "owner-b", BootID: "boot-1"}
		requested := firstTarget
		requested.Owner = &requestedOwner
		_, err := acquireWakeLockWithOptions(root, "orchestrator", wakeLockAcquireOptions{
			acceptExistingValid: true, replaceExistingBaseline: true,
			target: &requested, wakeMode: wakeTargetInjectVia,
		})
		if err == nil || replacementCalls != 0 {
			t.Fatalf("different owner error=%v replacement calls=%d", err, replacementCalls)
		}
	})

	t.Run("explicit replacement refuses unverified identity", func(t *testing.T) {
		stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
			switch pid {
			case wakePID:
				return wakeProcessInfo{
					PID: pid, Running: true, Executable: "/opt/homebrew/bin/amq",
					Args: []string{"/opt/homebrew/bin/amq", "wake", "--me", "orchestrator"},
				}
			case owner.PID:
				return wakeProcessInfoForOwnerTest(owner)
			default:
				return wakeProcessInfo{PID: pid}
			}
		})
		_ = os.Remove(filepath.Join(fsq.AgentBase(root, "orchestrator"), ".wake.lock"))
		if err := writeWakeTarget(root, "orchestrator", firstTarget); err != nil {
			t.Fatal(err)
		}
		writeWakeLockForTest(t, root, "orchestrator", bindWakeLockToTarget(wakeLock{
			PID: wakePID, TTY: "unknown", ProcessStart: "start-1", BootID: "boot-1",
			Executable: "/opt/homebrew/bin/amq", Generation: "generation-unverified",
		}, firstTarget))
		replacementCalls = 0
		_, err := acquireWakeLockWithOptions(root, "orchestrator", wakeLockAcquireOptions{
			acceptExistingValid: true, replaceExistingBaseline: true,
			target: &secondTarget, wakeMode: wakeTargetInjectVia,
		})
		if err == nil || replacementCalls != 0 {
			t.Fatalf("unverified identity error=%v replacement calls=%d", err, replacementCalls)
		}
	})
}

func TestBaselineExistingInjectViaPersistsExactRepairFloor(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	writeWakeTestMessage(t, root, "codex", "existing.md", "msg-existing", "existing", "body")
	injector := writeExecutableForTest(t, "injector")
	called := false
	err := runWakeWithLoop([]string{
		"--root", root, "--me", "codex", "--inject-via", injector,
		"--inject-arg", "exec", "--baseline-existing",
	}, func(cfg wakeConfig) error {
		called = true
		target, exists, err := readWakeTarget(root, "codex")
		if err != nil || !exists {
			return fmt.Errorf("read persisted target: exists=%v err=%w", exists, err)
		}
		if target.BaselineFile == "" || target.BaselineDigest == "" || cfg.baseline == nil || cfg.baseline.Path != target.BaselineFile {
			return fmt.Errorf("baseline-existing target is not exact: %#v", target)
		}
		args := strings.Join(buildRepairWakeArgs(root, "codex", target, "/tmp/ready"), "|")
		if !strings.Contains(args, "|--baseline-file|"+target.BaselineFile+"|") || strings.Contains(args, "|--baseline-existing|") {
			return fmt.Errorf("repair args lost exact floor: %s", args)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("wake loop callback was not called")
	}
}

func TestBaselineExistingRestartReusesPersistedFloorAcrossDowntime(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	setExactWakeOwnerEnvForTest(t)
	writeWakeTestMessage(t, root, "codex", "before.md", "msg-before", "before first start", "body")
	toolDir := secureTempDirForTest(t)
	outputPath := filepath.Join(toolDir, "injections.log")
	injector := filepath.Join(toolDir, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\nprintf '%s\\n' \"$2\" >> \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	baseArgs := []string{
		"--root", root, "--me", "codex", "--inject-via", injector,
		"--inject-arg", outputPath, "--baseline-existing",
	}
	var originalFloor string
	if err := runWakeWithLoop(baseArgs, func(cfg wakeConfig) error {
		if cfg.baseline == nil || cfg.baseline.Path == "" {
			return fmt.Errorf("first start did not materialize an exact floor")
		}
		originalFloor = cfg.baseline.Path
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentBase(root, "codex"), ".wake.lock")); !os.IsNotExist(err) {
		t.Fatalf("first wake lock still exists after simulated stop: %v", err)
	}
	writeWakeTestMessage(t, root, "codex", "downtime.md", "msg-downtime", "arrived during downtime", "body")

	readyPath := filepath.Join(secureTempDirForTest(t), "wake.ready")
	restartArgs := append(append([]string{}, baseArgs...), "--accept-existing-wake", "--ready-file", readyPath)
	if err := runWakeWithLoop(restartArgs, func(cfg wakeConfig) error {
		gotFloor := ""
		if cfg.baseline != nil {
			gotFloor = cfg.baseline.Path
		}
		if gotFloor != originalFloor {
			return fmt.Errorf("restart floor = %q, want persisted %q", gotFloor, originalFloor)
		}
		return notifyNewMessages(&cfg)
	}); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "arrived during downtime") || strings.Contains(string(output), "before first start") {
		t.Fatalf("restart notification output = %q", output)
	}
	persisted, exists, err := readWakeTarget(root, "codex")
	if err != nil || !exists || persisted.BaselineFile != originalFloor {
		t.Fatalf("restart persisted floor changed: exists=%v target=%#v err=%v", exists, persisted, err)
	}
	if err := os.Remove(originalFloor); err != nil {
		t.Fatal(err)
	}
	err = runWakeWithLoop(restartArgs, func(wakeConfig) error {
		t.Fatal("wake loop ran after persisted floor disappeared")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "persisted exact wake floor is unusable") {
		t.Fatalf("missing persisted floor error = %v", err)
	}
}

func TestRepairArgsReusePersistedBaselineFile(t *testing.T) {
	target := wakeTarget{
		InjectVia:      "/abs/injector",
		InjectArgs:     []string{"exec", "target"},
		BaselineFile:   "/private/baseline.json",
		BaselineDigest: "sha256:abc",
	}
	args := buildRepairWakeArgs("/tmp/root", "codex", target, "/tmp/ready")
	got := strings.Join(args, "|")
	want := "--no-update-check|wake|--me|codex|--root|/tmp/root|--inject-via|/abs/injector|--baseline-file|/private/baseline.json|--inject-arg|exec|--inject-arg|target|--ready-file|/tmp/ready"
	if got != want {
		t.Fatalf("repair args = %q, want %q", got, want)
	}
}

func waitForTestCondition(t *testing.T, timeout time.Duration, condition func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}
