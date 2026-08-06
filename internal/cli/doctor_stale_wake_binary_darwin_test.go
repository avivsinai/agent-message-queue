//go:build darwin

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testDarwinCorroboratedImageMethod wakeBinaryComparisonMethod = "darwin_process_image"

const (
	testDarwinImageCaptureHelperEnv = "AMQ_TEST_DARWIN_IMAGE_CAPTURE_HELPER"
	testDarwinImageReadyEnv         = "AMQ_TEST_DARWIN_IMAGE_READY"
	testDarwinImageCaptureGateEnv   = "AMQ_TEST_DARWIN_IMAGE_CAPTURE_GATE"
	testDarwinImageCaptureResultEnv = "AMQ_TEST_DARWIN_IMAGE_CAPTURE_RESULT"
	testDarwinImageReleaseEnv       = "AMQ_TEST_DARWIN_IMAGE_RELEASE"
)

type testDarwinImageCaptureResult struct {
	Started  string              `json:"started"`
	Evidence wakeImageEvidenceV1 `json:"evidence"`
	Error    string              `json:"error,omitempty"`
}

func TestDarwinSameVersionPathReplacementCannotReportCurrent(t *testing.T) {
	if os.Getenv(testDarwinImageCaptureHelperEnv) == "1" {
		runDarwinImageCaptureHelper(t)
		return
	}

	dir := t.TempDir()
	testImage, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	executionPath := filepath.Join(dir, "amq-test")
	replacementPath := filepath.Join(dir, "amq-replacement")
	for _, path := range []string{executionPath, replacementPath} {
		if err := os.WriteFile(path, testImage, 0o700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	loadedInfo, err := os.Stat(executionPath)
	if err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(dir, "ready")
	captureGatePath := filepath.Join(dir, "capture")
	resultPath := filepath.Join(dir, "result.json")
	releasePath := filepath.Join(dir, "release")
	var childOutput bytes.Buffer
	cmd := exec.Command(executionPath, "-test.run=^TestDarwinSameVersionPathReplacementCannotReportCurrent$")
	cmd.Env = append(os.Environ(),
		testDarwinImageCaptureHelperEnv+"=1",
		testDarwinImageReadyEnv+"="+readyPath,
		testDarwinImageCaptureGateEnv+"="+captureGatePath,
		testDarwinImageCaptureResultEnv+"="+resultPath,
		testDarwinImageReleaseEnv+"="+releasePath,
	)
	cmd.Stdout = &childOutput
	cmd.Stderr = &childOutput
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if finished {
			return
		}
		_ = os.WriteFile(releasePath, []byte("release\n"), 0o600)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if err := waitForDarwinImageTestPath(readyPath); err != nil {
		t.Fatalf("wait for running image A: %v\n%s", err, childOutput.String())
	}

	if err := os.Rename(replacementPath, executionPath); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Stat(executionPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(loadedInfo, replacementInfo) {
		t.Fatal("replacement did not change the executable pathname vnode")
	}
	if err := os.WriteFile(captureGatePath, []byte("capture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForDarwinImageTestPath(resultPath); err != nil {
		t.Fatalf("wait for replacement evidence: %v\n%s", err, childOutput.String())
	}
	payload, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var captured testDarwinImageCaptureResult
	if err := json.Unmarshal(payload, &captured); err != nil {
		t.Fatalf("decode child result: %v\n%s", err, payload)
	}
	if captured.Error != "" {
		t.Fatalf("child capture failed: %s", captured.Error)
	}
	if captured.Evidence.Inode == 0 || captured.Evidence.Inode == wakeFileInodeForTest(t, loadedInfo) {
		t.Fatalf("capture did not bind replacement vnode: %#v", captured.Evidence)
	}
	currentInfo, err := os.Stat(captured.Evidence.ExecutionPath)
	if err != nil {
		t.Fatal(err)
	}
	inspection := wakeLockInspection{
		PID: cmd.Process.Pid,
		Lock: wakeLock{
			Started:              captured.Started,
			ImagePath:            captured.Evidence.ExecutionPath,
			ImageVersion:         captured.Evidence.EmbeddedVersion,
			RunningImageEvidence: &captured.Evidence,
		},
	}
	comparison, err := inspectWakeBinaryStalenessPlatform(
		inspection,
		resolvedWakeBinary{Info: currentInfo},
	)
	if err != nil {
		t.Fatalf("compare post-exec replacement: %v", err)
	}
	if comparison.Stale || comparison.Method != wakeBinaryComparisonDarwinProcessImage {
		t.Fatalf("pathname evidence did not reproduce false agreement: %#v", comparison)
	}
	stubWakeBinaryStaleness(t, func(wakeLockInspection) (wakeBinaryStaleness, error) {
		return comparison, nil
	})
	if got := inspectWakeCheckImageStatus(inspection, wakeCheckResult{
		RunningVersion: captured.Evidence.EmbeddedVersion,
		CurrentVersion: captured.Evidence.EmbeddedVersion,
	}); got != wakeImageUnknown {
		t.Fatalf("post-exec pathname replacement image status = %q, want %q", got, wakeImageUnknown)
	}

	if err := os.WriteFile(releasePath, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("capture helper: %v\n%s", err, childOutput.String())
	}
	finished = true
}

func runDarwinImageCaptureHelper(t *testing.T) {
	t.Helper()
	result := testDarwinImageCaptureResult{Started: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := os.WriteFile(os.Getenv(testDarwinImageReadyEnv), []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForDarwinImageTestPath(os.Getenv(testDarwinImageCaptureGateEnv)); err != nil {
		result.Error = err.Error()
	} else {
		result.Evidence, err = captureCurrentWakeImageEvidence()
		if err != nil {
			result.Error = err.Error()
		}
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(testDarwinImageCaptureResultEnv), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForDarwinImageTestPath(os.Getenv(testDarwinImageReleaseEnv)); err != nil {
		t.Fatal(err)
	}
}

func waitForDarwinImageTestPath(path string) error {
	// Starting a copied, ad-hoc-signed Go test binary can be delayed by macOS
	// validation when race builds are running concurrently. Keep the causal
	// handshake bounded, but leave enough headroom for that startup work.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", filepath.Base(path))
}

func wakeFileInodeForTest(t *testing.T, info os.FileInfo) uint64 {
	t.Helper()
	identity, ok := captureWakeFileIdentity(info)
	if !ok {
		t.Fatal("capture test file identity")
	}
	return identity.Inode
}

func TestDarwinProcessExecutablePathMatchesCurrentProcess(t *testing.T) {
	got, err := readDarwinProcessExecutablePath(os.Getpid())
	if err != nil {
		t.Fatalf("read current process executable: %v", err)
	}
	if !filepath.IsAbs(got) || filepath.Clean(got) != got {
		t.Fatalf("process executable path = %q, want canonical absolute path", got)
	}

	want, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err = filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("process executable %q is not current executable %q", got, want)
	}
}

func TestDarwinWakeBinaryComparisonReportsCurrentFromCorroboratedLiveImage(t *testing.T) {
	path, err := readDarwinProcessExecutablePath(os.Getpid())
	if err != nil {
		t.Fatalf("read current process executable: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence := darwinWakeImageEvidenceForTest(t, path, info)
	started := time.Now().UTC().Add(time.Second).Truncate(time.Second)

	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{
			PID: os.Getpid(),
			Lock: wakeLock{
				Started:              started.Format(time.RFC3339),
				ImagePath:            path,
				ImageVersion:         evidence.EmbeddedVersion,
				RunningImageEvidence: &evidence,
			},
		},
		resolvedWakeBinary{Info: info},
	)
	if err != nil {
		t.Fatalf("compare corroborated current image: %v", err)
	}
	if got.Stale || got.Method != testDarwinCorroboratedImageMethod {
		t.Fatalf("corroborated current comparison = %#v", got)
	}
	if !got.Evidence.Available || got.Evidence.Running != got.Evidence.Current {
		t.Fatalf("corroborated evidence = %#v", got.Evidence)
	}
}

func TestDarwinWakeBinaryComparisonReportsHomebrewReplacementDifferent(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	dir := t.TempDir()
	runningPath := filepath.Join(dir, "Cellar", "amq", "0.50.1", "bin", "amq")
	currentPath := filepath.Join(dir, "Cellar", "amq", "0.51.0", "bin", "amq")
	for _, path := range []string{runningPath, currentPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(runningPath, started.Add(-time.Minute), started.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(currentPath, started.Add(time.Minute), started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	runningInfo, err := os.Stat(runningPath)
	if err != nil {
		t.Fatal(err)
	}
	currentInfo, err := os.Stat(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	evidence := darwinWakeImageEvidenceForTest(t, runningPath, runningInfo)
	stubDarwinProcessExecutablePath(t, func(int) (string, error) { return runningPath, nil })

	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{
			PID: 4242,
			Lock: wakeLock{
				Started:              started.Format(time.RFC3339),
				ImagePath:            runningPath,
				ImageVersion:         evidence.EmbeddedVersion,
				RunningImageEvidence: &evidence,
			},
		},
		resolvedWakeBinary{Info: currentInfo},
	)
	if err != nil {
		t.Fatalf("compare Homebrew replacement: %v", err)
	}
	if !got.Stale {
		t.Fatalf("Homebrew replacement comparison = %#v, want different", got)
	}
}

func TestDarwinWakeBinaryComparisonReportsDeletedLiveImageStale(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	dir := t.TempDir()
	runningPath := filepath.Join(dir, "Cellar", "amq", "0.52.0", "bin", "amq")
	currentPath := filepath.Join(dir, "Cellar", "amq", "0.52.3", "bin", "amq")
	for _, path := range []string{runningPath, currentPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runningInfo, err := os.Stat(runningPath)
	if err != nil {
		t.Fatal(err)
	}
	currentInfo, err := os.Stat(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	evidence := darwinWakeImageEvidenceForTest(t, runningPath, runningInfo)
	stubDarwinProcessExecutablePath(t, func(int) (string, error) { return runningPath, nil })
	if err := os.Remove(runningPath); err != nil {
		t.Fatal(err)
	}

	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{
			PID:               4242,
			IdentityConfirmed: true,
			Process:           wakeProcessInfo{PID: 4242, Running: true},
			Lock: wakeLock{
				Started:              started.Format(time.RFC3339),
				ImagePath:            runningPath,
				ImageVersion:         evidence.EmbeddedVersion,
				RunningImageEvidence: &evidence,
			},
		},
		resolvedWakeBinary{Info: currentInfo},
	)
	if err != nil {
		t.Fatalf("compare deleted live image: %v", err)
	}
	if !got.Stale || got.Method != wakeBinaryComparisonDarwinDeletedImage {
		t.Fatalf("deleted live image comparison = %#v, want proven stale", got)
	}
	if got.Reason != "wake is running a deleted image; restart it" {
		t.Fatalf("deleted live image reason = %q", got.Reason)
	}
}

func TestDarwinWakeBinaryComparisonReportsDeletedRecordedImageWhenProcPIDPathReturnsENOENT(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	dir := t.TempDir()
	runningPath := filepath.Join(dir, "Cellar", "amq", "0.52.0", "bin", "amq")
	currentPath := filepath.Join(dir, "Cellar", "amq", "0.52.3", "bin", "amq")
	for _, path := range []string{runningPath, currentPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(path), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runningInfo, err := os.Stat(runningPath)
	if err != nil {
		t.Fatal(err)
	}
	currentInfo, err := os.Stat(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	evidence := darwinWakeImageEvidenceForTest(t, runningPath, runningInfo)
	stubDarwinProcessExecutablePath(t, func(int) (string, error) { return "", os.ErrNotExist })
	if err := os.Remove(runningPath); err != nil {
		t.Fatal(err)
	}

	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{
			PID:               4242,
			IdentityConfirmed: true,
			Process:           wakeProcessInfo{PID: 4242, Running: true},
			Lock: wakeLock{
				Started:              started.Format(time.RFC3339),
				ImagePath:            runningPath,
				ImageVersion:         evidence.EmbeddedVersion,
				RunningImageEvidence: &evidence,
			},
		},
		resolvedWakeBinary{Info: currentInfo},
	)
	if err != nil {
		t.Fatalf("compare deleted recorded image after proc_pidpath ENOENT: %v", err)
	}
	if !got.Stale || got.Method != wakeBinaryComparisonDarwinDeletedImage || got.Reason != deletedWakeImageReason {
		t.Fatalf("deleted recorded image comparison = %#v, want proven stale", got)
	}
}

func TestDarwinWakeBinaryComparisonKeepsNonENOENTImageFailureUnknown(t *testing.T) {
	path := t.TempDir()
	currentPath := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(currentPath, []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}
	currentInfo, err := os.Stat(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	stubDarwinProcessExecutablePath(t, func(int) (string, error) { return path, nil })
	recorded := wakeImageEvidenceV1{
		Schema:          wakeImageEvidenceSchemaV1,
		Platform:        "darwin",
		Method:          wakeImageMethodPathnameExecVerified,
		ExecutionPath:   path,
		Device:          1,
		Inode:           1,
		Size:            1,
		CTimeNS:         1,
		SHA256:          "sha256:" + strings.Repeat("0", 64),
		EmbeddedVersion: "test-version",
	}

	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{
			PID:               4242,
			IdentityConfirmed: true,
			Process:           wakeProcessInfo{PID: 4242, Running: true},
			Lock: wakeLock{
				Started:              time.Now().UTC().Format(time.RFC3339Nano),
				ImagePath:            path,
				ImageVersion:         recorded.EmbeddedVersion,
				RunningImageEvidence: &recorded,
			},
		},
		resolvedWakeBinary{Info: currentInfo},
	)
	if err == nil || got.Stale {
		t.Fatalf("non-ENOENT image failure = %#v, %v; want unknown error", got, err)
	}
}

func TestDarwinWakeBinaryComparisonRejectsRecordedEvidenceDisagreement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence := darwinWakeImageEvidenceForTest(t, path, info)
	evidence.Inode++
	stubDarwinProcessExecutablePath(t, func(int) (string, error) { return path, nil })

	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{
			PID: 4242,
			Lock: wakeLock{
				Started:              time.Now().UTC().Format(time.RFC3339),
				ImagePath:            path,
				ImageVersion:         evidence.EmbeddedVersion,
				RunningImageEvidence: &evidence,
			},
		},
		resolvedWakeBinary{Info: info},
	)
	if err == nil {
		t.Fatalf("disagreeing recorded evidence returned %#v, want unknown error", got)
	}
}

func TestDarwinWakeBinaryComparisonRejectsRecordedDigestDisagreement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence := darwinWakeImageEvidenceForTest(t, path, info)
	evidence.SHA256 = "sha256:" + strings.Repeat("f", 64)
	stubDarwinProcessExecutablePath(t, func(int) (string, error) { return path, nil })

	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{
			PID: 4242,
			Lock: wakeLock{
				Started:              time.Now().UTC().Format(time.RFC3339),
				ImagePath:            path,
				ImageVersion:         evidence.EmbeddedVersion,
				RunningImageEvidence: &evidence,
			},
		},
		resolvedWakeBinary{Info: info},
	)
	if err == nil {
		t.Fatalf("disagreeing recorded digest returned %#v, want unknown error", got)
	}
}

func darwinWakeImageEvidenceForTest(t *testing.T, path string, info os.FileInfo) wakeImageEvidenceV1 {
	t.Helper()
	identity, ok := captureWakeFileIdentity(info)
	if !ok {
		t.Fatal("capture test image identity")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return wakeImageEvidenceV1{
		Schema:          wakeImageEvidenceSchemaV1,
		Platform:        "darwin",
		Method:          wakeImageMethodPathnameExecVerified,
		ExecutionPath:   filepath.Clean(path),
		Device:          identity.Device,
		Inode:           identity.Inode,
		Size:            info.Size(),
		CTimeNS:         identity.CTimeSec*1_000_000_000 + identity.CTimeNsec,
		SHA256:          "sha256:" + hex.EncodeToString(digest[:]),
		EmbeddedVersion: "test-version",
	}
}

func stubDarwinProcessExecutablePath(t *testing.T, fn func(int) (string, error)) {
	t.Helper()
	old := readDarwinProcessExecutablePath
	readDarwinProcessExecutablePath = fn
	t.Cleanup(func() { readDarwinProcessExecutablePath = old })
}

func TestDarwinWakeBinaryComparisonUsesStrictStartedMTimeHeuristic(t *testing.T) {
	started := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		mtime time.Time
		stale bool
	}{
		{name: "before started", mtime: started.Add(-time.Second)},
		{name: "equal started", mtime: started},
		{name: "inside second precision window", mtime: started.Add(999 * time.Millisecond)},
		{name: "equal uncertainty boundary", mtime: started.Add(time.Second)},
		{name: "strictly after uncertainty boundary", mtime: started.Add(time.Second + time.Nanosecond), stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chtimes(path, tc.mtime, tc.mtime); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := inspectWakeBinaryStalenessPlatform(
				wakeLockInspection{Lock: wakeLock{Started: started.Format(time.RFC3339)}},
				resolvedWakeBinary{Info: info},
			)
			if err != nil {
				t.Fatalf("compare timestamps: %v", err)
			}
			if got.Stale != tc.stale {
				t.Fatalf("stale = %v, want %v: %#v", got.Stale, tc.stale, got)
			}
			if got.Stale && got.Method != wakeBinaryComparisonStartedMTime {
				t.Fatalf("stale method = %q", got.Method)
			}
			if !got.Evidence.Available || got.Evidence.CurrentModTime != tc.mtime.UnixNano() {
				t.Fatalf("comparison evidence = %#v, want current mtime %d", got.Evidence, tc.mtime.UnixNano())
			}
		})
	}
}

func TestDarwinWakeBinaryComparisonReturnsUnknownForInvalidStarted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{Lock: wakeLock{Started: "not-a-timestamp"}},
		resolvedWakeBinary{Info: info},
	)
	if err == nil {
		t.Fatal("invalid Started returned nil error")
	}
	if got.Stale {
		t.Fatalf("invalid Started reported stale: %#v", got)
	}
}

func TestDarwinWakeBinaryComparisonStatsResolvedHomebrewTargetNotSymlink(t *testing.T) {
	started := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
	dir := t.TempDir()
	target := filepath.Join(dir, "Cellar", "amq", "0.49.6", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	targetMTime := started.Add(-time.Minute)
	if err := os.Chtimes(target, targetMTime, targetMTime); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !linkInfo.ModTime().After(started.Add(wakeStartedTimestampUncertainty)) {
		t.Fatalf("test requires symlink mtime after wake uncertainty boundary: %v", linkInfo.ModTime())
	}

	old := resolveWakeExecutablePath
	resolveWakeExecutablePath = func() (string, string, error) {
		return link, target, nil
	}
	t.Cleanup(func() { resolveWakeExecutablePath = old })

	got, err := inspectWakeBinaryStalenessDefault(wakeLockInspection{
		Lock: wakeLock{Started: started.Format(time.RFC3339)},
	})
	if err != nil {
		t.Fatalf("compare resolved Homebrew target: %v", err)
	}
	if got.Stale {
		t.Fatalf("newer Homebrew symlink mtime produced stale hint: %#v", got)
	}
}
