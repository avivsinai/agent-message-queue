//go:build darwin || linux

package cli

import (
	"testing"
)

func TestSplitDashDash(t *testing.T) {
	tests := []struct {
		name       string
		input      []string
		wantBefore []string
		wantAfter  []string
	}{
		{
			name:       "no separator",
			input:      []string{"claude"},
			wantBefore: []string{"claude"},
			wantAfter:  nil,
		},
		{
			name:       "separator with args",
			input:      []string{"--root", "/tmp/q", "codex", "--", "--some-flag", "--other"},
			wantBefore: []string{"--root", "/tmp/q", "codex"},
			wantAfter:  []string{"--some-flag", "--other"},
		},
		{
			name:       "separator at start",
			input:      []string{"--", "claude", "-v"},
			wantBefore: []string{},
			wantAfter:  []string{"claude", "-v"},
		},
		{
			name:       "separator at end",
			input:      []string{"claude", "--"},
			wantBefore: []string{"claude"},
			wantAfter:  []string{},
		},
		{
			name:       "empty input",
			input:      []string{},
			wantBefore: []string{},
			wantAfter:  nil,
		},
		{
			name:       "multiple separators",
			input:      []string{"a", "--", "b", "--", "c"},
			wantBefore: []string{"a"},
			wantAfter:  []string{"b", "--", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := splitDashDash(tt.input)
			if !sliceEq(before, tt.wantBefore) {
				t.Errorf("before = %v, want %v", before, tt.wantBefore)
			}
			if !sliceEq(after, tt.wantAfter) {
				t.Errorf("after = %v, want %v", after, tt.wantAfter)
			}
		})
	}
}

func TestSetEnvVar(t *testing.T) {
	t.Run("append new", func(t *testing.T) {
		env := []string{"PATH=/bin", "HOME=/home"}
		got := setEnvVar(env, "AM_ROOT", "/tmp/q")
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[2] != "AM_ROOT=/tmp/q" {
			t.Fatalf("got[2] = %q, want %q", got[2], "AM_ROOT=/tmp/q")
		}
	})

	t.Run("replace existing", func(t *testing.T) {
		env := []string{"PATH=/bin", "AM_ROOT=/old", "HOME=/home"}
		got := setEnvVar(env, "AM_ROOT", "/new")
		if len(got) != 3 {
			t.Fatalf("len = %d, want 3", len(got))
		}
		if got[1] != "AM_ROOT=/new" {
			t.Fatalf("got[1] = %q, want %q", got[1], "AM_ROOT=/new")
		}
	})
}

func TestCoopExecUsageError(t *testing.T) {
	err := runCoopExec([]string{})
	if err == nil {
		t.Fatal("expected error for empty args")
	}
	exitErr, ok := err.(*ExitCodeError)
	if !ok {
		t.Fatalf("expected *ExitCodeError, got %T: %v", err, err)
	}
	if exitErr.Code != ExitUsage {
		t.Fatalf("expected ExitUsage (%d), got %d", ExitUsage, exitErr.Code)
	}
	if !containsStr(err.Error(), "command required") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func sliceEq(a, b []string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && searchStr(s, sub)
}

func searchStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
