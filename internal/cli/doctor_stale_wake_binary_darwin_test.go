//go:build darwin

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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
		resolvedWakeBinary{Path: path, Info: info},
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
	stubDarwinProcessImage(t, darwinMappedImageForTest(t, runningPath, runningInfo), nil)

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
		resolvedWakeBinary{Path: currentPath, Info: currentInfo},
	)
	if err != nil {
		t.Fatalf("compare Homebrew replacement: %v", err)
	}
	if !got.Stale {
		t.Fatalf("Homebrew replacement comparison = %#v, want different", got)
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

func darwinMappedImageForTest(t *testing.T, path string, info os.FileInfo) darwinWakeProcessImage {
	t.Helper()
	identity, ok := captureWakeFileIdentity(info)
	if !ok {
		t.Fatal("capture mapped image identity")
	}
	return darwinWakeProcessImage{Path: path, Identity: identity, Size: info.Size()}
}

func stubDarwinProcessImage(t *testing.T, image darwinWakeProcessImage, err error) {
	t.Helper()
	old := inspectDarwinWakeProcessImage
	inspectDarwinWakeProcessImage = func(int) (darwinWakeProcessImage, error) {
		return image, err
	}
	t.Cleanup(func() { inspectDarwinWakeProcessImage = old })
}
