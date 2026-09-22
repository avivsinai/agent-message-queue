//go:build !windows

package claude

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

// testRegistryFIFO probes a FIFO at the session-registry leaf: refused
// without opening (an open would block until a writer appears — the exact
// r2 P1-1 freeze under the endpoint mutex). Unix-only: mkfifo is not in
// the Windows syscall package.
func testRegistryFIFO(t *testing.T, home, regPath string) {
	t.Helper()
	if err := os.Remove(regPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(regPath, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if _, err := readSessionRegistry(home, 77); err == nil {
		t.Fatal("accepted a FIFO at the session-registry path")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("refusal does not name the file-type rule: %v", err)
	}
}
