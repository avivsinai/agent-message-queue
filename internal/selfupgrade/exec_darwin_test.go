//go:build darwin

package selfupgrade

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecImagePlatformRejectsModifiedDarwinCodeSignature(t *testing.T) {
	candidatePath := signedDarwinCandidate(t)
	corruptDarwinCandidate(t, candidatePath)
	candidate, err := CaptureImageEvidence(candidatePath, "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	previousPath := selfUpgradeCodesignPath
	previousExec := selfUpgradeDarwinExec
	t.Cleanup(func() {
		selfUpgradeCodesignPath = previousPath
		selfUpgradeDarwinExec = previousExec
	})
	execCalls := 0
	selfUpgradeDarwinExec = func(string, []string, []string) error {
		execCalls++
		return errors.New("unexpected exec")
	}

	err = execImagePlatform(candidate, []string{candidatePath}, os.Environ())
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "modified") {
		t.Fatalf("execImagePlatform() error = %v, want signature refusal", err)
	}
	if execCalls != 0 {
		t.Fatalf("exec calls = %d, want zero after signature refusal", execCalls)
	}
}

func TestExecImagePlatformAcceptsIntactDarwinCodeSignature(t *testing.T) {
	candidatePath := signedDarwinCandidate(t)
	candidate, err := CaptureImageEvidence(candidatePath, "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	previousPath := selfUpgradeCodesignPath
	previousExec := selfUpgradeDarwinExec
	t.Cleanup(func() {
		selfUpgradeCodesignPath = previousPath
		selfUpgradeDarwinExec = previousExec
	})
	wantExecErr := errors.New("stop before replacement")
	execCalls := 0
	selfUpgradeDarwinExec = func(string, []string, []string) error {
		execCalls++
		return wantExecErr
	}

	err = execImagePlatform(candidate, []string{candidatePath}, os.Environ())
	if !errors.Is(err, wantExecErr) {
		t.Fatalf("execImagePlatform() error = %v, want injected exec error", err)
	}
	if execCalls != 1 {
		t.Fatalf("exec calls = %d, want one after signature verification", execCalls)
	}
}

func TestExecImagePlatformReportsMissingDarwinCodeSignatureTool(t *testing.T) {
	dir := t.TempDir()
	candidatePath := filepath.Join(dir, "amq-keepalive")
	if err := os.WriteFile(candidatePath, []byte("candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	candidate, err := CaptureImageEvidence(candidatePath, "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	previousPath := selfUpgradeCodesignPath
	previousExec := selfUpgradeDarwinExec
	t.Cleanup(func() {
		selfUpgradeCodesignPath = previousPath
		selfUpgradeDarwinExec = previousExec
	})
	selfUpgradeCodesignPath = filepath.Join(dir, "missing-codesign")
	execCalls := 0
	selfUpgradeDarwinExec = func(string, []string, []string) error {
		execCalls++
		return nil
	}

	err = execImagePlatform(candidate, []string{candidatePath}, os.Environ())
	if err == nil || !strings.Contains(err.Error(), "/usr/bin/codesign must exist and pass validation") {
		t.Fatalf("execImagePlatform() error = %v, want installation remedy", err)
	}
	if execCalls != 0 {
		t.Fatalf("exec calls = %d, want zero when codesign is missing", execCalls)
	}
}

func signedDarwinCandidate(t *testing.T) string {
	t.Helper()
	sourcePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	candidatePath := filepath.Join(t.TempDir(), "amq-keepalive")
	if err := os.WriteFile(candidatePath, data, 0o700); err != nil {
		t.Fatal(err)
	}
	codesignPath := "/usr/bin/codesign"
	output, err := exec.Command(
		codesignPath,
		"--force",
		"--sign",
		"-",
		"--timestamp=none",
		candidatePath,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("codesign signed fixture: %v\n%s", err, output)
	}
	return candidatePath
}

func corruptDarwinCandidate(t *testing.T, candidatePath string) {
	t.Helper()
	file, err := os.OpenFile(candidatePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 2 {
		t.Fatalf("signed fixture size = %d, want at least two bytes", info.Size())
	}
	offset := info.Size() / 2
	var byteAtOffset [1]byte
	if _, err := file.ReadAt(byteAtOffset[:], offset); err != nil {
		t.Fatal(err)
	}
	byteAtOffset[0] ^= 0xff
	if _, err := file.WriteAt(byteAtOffset[:], offset); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
}
