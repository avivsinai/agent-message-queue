//go:build darwin

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func signTestAMQ(t *testing.T, path string) {
	t.Helper()
	codesign, err := exec.LookPath("codesign")
	if err != nil {
		t.Fatalf("codesign is required for Darwin AMQ test binaries; install the Xcode command line tools with xcode-select --install: %v", err)
	}
	output, err := exec.Command(codesign, "--sign", "-", "--force", path).CombinedOutput()
	if err != nil {
		t.Fatalf("codesign test AMQ %s: %v\n%s", path, err, output)
	}
	if err := verifyTestAMQ(path); err != nil {
		t.Fatalf("verify re-signed test AMQ %s: %v", path, err)
	}
}

func verifyTestAMQ(path string) error {
	codesign, err := exec.LookPath("codesign")
	if err != nil {
		return fmt.Errorf("find codesign (install the Xcode command line tools with xcode-select --install): %w", err)
	}
	output, err := exec.Command(codesign, "--verify", "--strict", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func TestTestAMQSignatureRejectsTampering(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	signed := filepath.Join(t.TempDir(), "amq")
	data, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signed, data, 0o700); err != nil {
		t.Fatal(err)
	}
	signTestAMQ(t, signed)
	if err := verifyTestAMQ(signed); err != nil {
		t.Fatalf("re-signed test AMQ failed verification: %v", err)
	}

	signedData, err := os.ReadFile(signed)
	if err != nil {
		t.Fatal(err)
	}
	if len(signedData) <= 4096 {
		t.Fatalf("signed test AMQ is too small to tamper safely: %d bytes", len(signedData))
	}
	signedData[4096] ^= 1
	corrupted := filepath.Join(t.TempDir(), "amq-corrupted")
	if err := os.WriteFile(corrupted, signedData, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyTestAMQ(corrupted); err == nil {
		t.Fatal("codesign accepted a deliberately corrupted AMQ binary")
	}
}
