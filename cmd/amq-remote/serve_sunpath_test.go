//go:build darwin || linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// A socket path over the Unix sun_path limit is a refused configuration, not
// a runtime error: serve exits 6 so up stops instead of respawning a serve
// that can never start. Regression: the 2026-11-24 remote surface walk ran
// serve on a root whose endpoint.sock path was 118 bytes and saw exit 1,
// which up would respawn forever against the README's "its configuration is
// refused ... is not respawned".
func TestServeExitsActionRequiredWhenSocketPathExceedsSunPath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, strings.Repeat("r", 200))
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	code := run([]string{"serve", "--root", root}, strings.NewReader(""), &out, &errBuf)
	if code != protocol.ExitActionRequired {
		t.Fatalf("serve on an over-long root exited %d, want %d (action required); stderr: %s",
			code, protocol.ExitActionRequired, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "sun_path") {
		t.Fatalf("stderr does not name the sun_path refusal: %s", errBuf.String())
	}
}
