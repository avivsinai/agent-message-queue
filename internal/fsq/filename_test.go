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

func TestFindMessagePrefersNewWhenBothBoxesExist(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	filename := "both.md"
	if err := os.WriteFile(filepath.Join(AgentInboxNew(root, "alice"), filename), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(AgentInboxCur(root, "alice"), filename), []byte("cur"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, box, err := FindMessage(root, "alice", filename)
	if err != nil || box != BoxNew || path != filepath.Join(AgentInboxNew(root, "alice"), filename) {
		t.Fatalf("FindMessage = %q %q %v, want inbox/new", path, box, err)
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
