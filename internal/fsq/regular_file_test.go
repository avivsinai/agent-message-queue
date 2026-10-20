package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRegularNoFollowBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadRegularNoFollowBounded(path, 64)
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("ReadRegularNoFollowBounded = %q, %v", data, err)
	}
}

func TestCreateNewFileKeepsTheFirstWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.json")
	if created, err := CreateNewFile(path, []byte("first"), 0o600); err != nil || !created {
		t.Fatalf("first CreateNewFile = %v, %v", created, err)
	}
	if created, err := CreateNewFile(path, []byte("second"), 0o600); err != nil || created {
		t.Fatalf("second CreateNewFile = %v, %v; want not created", created, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "first" {
		t.Fatalf("claim = %q, %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory holds %d entries, %v; want only the claim", len(entries), err)
	}
}
