package fsq

import (
	"os"
	"path/filepath"
	"testing"
)

func invalidMessageFilenames(t *testing.T) []string {
	t.Helper()
	return []string{
		"../message.md",
		"nested/message.md",
		`nested\message.md`,
		filepath.Join(os.TempDir(), "message.md"),
		"message\x00.md",
		"message.txt",
		".attack.md",
	}
}

func TestValidateMessageFilename(t *testing.T) {
	for _, filename := range invalidMessageFilenames(t) {
		t.Run(filename, func(t *testing.T) {
			if err := ValidateMessageFilename(filename); err == nil {
				t.Fatalf("ValidateMessageFilename(%q) error = nil, want error", filename)
			}
		})
	}

	if err := ValidateMessageFilename("message.md"); err != nil {
		t.Fatalf("ValidateMessageFilename(valid) error = %v, want nil", err)
	}
}

func TestMoveNewToCurRejectsInvalidFilenames(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}

	for _, filename := range invalidMessageFilenames(t) {
		t.Run(filename, func(t *testing.T) {
			if err := MoveNewToCur(openDeliveryRootForTest(t, root), "alice", filename); err == nil {
				t.Fatalf("MoveNewToCur(%q) error = nil, want error", filename)
			}
		})
	}
}
