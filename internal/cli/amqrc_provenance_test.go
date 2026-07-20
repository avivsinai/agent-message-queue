package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFindAmqrcForRootLstatFailureRefused(t *testing.T) {
	root := t.TempDir()
	rc := filepath.Join(root, ".amqrc")
	if err := os.WriteFile(rc, []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := amqrcLstat
	amqrcLstat = func(string) (os.FileInfo, error) { return nil, errors.New("metadata denied") }
	defer func() { amqrcLstat = orig }()
	if _, err := findAmqrcForRoot(root); err == nil {
		t.Fatal("expected Lstat failure to refuse config")
	}
}
