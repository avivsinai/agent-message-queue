//go:build darwin

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testDarwinCorroboratedImageMethod wakeBinaryComparisonMethod = "darwin_process_image"

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
