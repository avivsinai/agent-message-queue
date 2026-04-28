//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadPassiveExtensionManifestRejectsFIFO(t *testing.T) {
	root := t.TempDir()
	layer := "fifo-manifest"
	layerDir := filepath.Join(root, "extensions", layer)
	if err := os.MkdirAll(layerDir, 0o700); err != nil {
		t.Fatalf("mkdir layer dir: %v", err)
	}
	manifestPath := filepath.Join(layerDir, "manifest.json")
	if err := syscall.Mkfifo(manifestPath, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}

	_, diag, ok := readPassiveExtensionManifest(root, layer, manifestPath)
	if ok {
		t.Fatal("expected FIFO manifest to be rejected")
	}
	if diag == nil || !strings.Contains(diag.Message, "not a regular file") {
		t.Fatalf("diagnostic = %+v, want not a regular file", diag)
	}
}
