//go:build !windows

package route

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAmqrcFileRejectsWorldWritable(t *testing.T) {
	p := t.TempDir() + "/.amqrc"
	if err := os.WriteFile(p, []byte(`{"root":".agent-mail"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAmqrcFile(p); err == nil {
		t.Fatal("expected world-writable config rejection")
	}
}

func TestFindAmqrcRejectsUntrustedProvenance(t *testing.T) {
	root := t.TempDir()

	path := filepath.Join(root, ".amqrc")
	if err := os.WriteFile(path, []byte(`{"root":".agent-mail"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := (Walker{}).FindAmqrc(root); err == nil || !strings.Contains(err.Error(), "group/world-writable") {
		t.Fatalf("expected writable .amqrc rejection, got %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "real.amqrc")
	if err := os.WriteFile(target, []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := (Walker{}).FindAmqrc(root); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}
