//go:build darwin

package cli

import (
	"fmt"
	"os/exec"
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
