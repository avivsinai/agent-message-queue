//go:build !windows

package cli

import (
	"os"
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
	if err := validateAmqrcFile(p); err == nil {
		t.Fatal("expected world-writable config rejection")
	}
}
