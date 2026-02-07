package fsq

import (
	"strings"
	"testing"
)

func TestValidateHandle(t *testing.T) {
	tests := []struct {
		handle string
		ok     bool
		errStr string
	}{
		{"claude", true, ""},
		{"codex", true, ""},
		{"agent-1", true, ""},
		{"my_agent", true, ""},
		{"a123", true, ""},
		{"", false, "empty"},
		{"  ", false, "empty"},
		{"..", false, "path traversal"},
		{"../etc", false, "path traversal"},
		{"foo/bar", false, "path traversal"},
		{"foo/../bar", false, "path traversal"},
		{"CLAUDE", false, "must match"},
		{"Agent", false, "must match"},
		{"has space", false, "must match"},
		{"has.dot", false, "must match"},
	}
	for _, tc := range tests {
		err := ValidateHandle(tc.handle)
		if tc.ok && err != nil {
			t.Errorf("ValidateHandle(%q) = %v, want nil", tc.handle, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("ValidateHandle(%q) = nil, want error containing %q", tc.handle, tc.errStr)
			} else if !strings.Contains(err.Error(), tc.errStr) {
				t.Errorf("ValidateHandle(%q) = %v, want error containing %q", tc.handle, err, tc.errStr)
			}
		}
	}
}

func TestEnsureAgentDirs_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	if err := EnsureAgentDirs(root, "../escape"); err == nil {
		t.Error("expected error for path traversal agent handle")
	}
}
