package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/avivsinai/agent-message-queue/integrations/pi"
)

// A pi host loads the file bridge-path prints; it must hold the extension
// built into this binary, replacing an older copy.
func TestPiBridgePathWritesTheBuiltInExtension(t *testing.T) {
	home := t.TempDir()
	stale := filepath.Join(home, "pi", "amq-bridge.ts")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("// older bridge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := writePiBridge(home)
	if err != nil {
		t.Fatal(err)
	}
	if path != stale {
		t.Fatalf("path = %q, want %q", path, stale)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(pibridge.Source) == 0 || !bytes.Equal(got, pibridge.Source) {
		t.Fatalf("file holds %d bytes, want the %d-byte built-in extension", len(got), len(pibridge.Source))
	}
}
