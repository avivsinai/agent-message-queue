package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellQuotePosix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{".agent-mail", ".agent-mail"},
		{"path/to/dir", "path/to/dir"},
		{"has space", "'has space'"},
		{"has'quote", "'has'\\''quote'"},
		{"$VAR", "'$VAR'"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shellQuotePosix(tt.input)
			if got != tt.expected {
				t.Errorf("shellQuotePosix(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestShellQuoteFish(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{".agent-mail", ".agent-mail"},
		{"path/to/dir", "path/to/dir"},
		{"has space", "'has space'"},
		{"has'quote", "'has\\'quote'"},
		{"$VAR", "'$VAR'"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shellQuoteFish(tt.input)
			if got != tt.expected {
				t.Errorf("shellQuoteFish(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestFindAndLoadAmqrc(t *testing.T) {
	// Create a temp directory structure
	root := t.TempDir()
	subdir := filepath.Join(root, "sub", "deep")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write .amqrc in root
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	// Change to subdir and try to find .amqrc
	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	if err := os.Chdir(subdir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result, err := findAndLoadAmqrc()
	if err != nil {
		t.Fatalf("findAndLoadAmqrc: %v", err)
	}

	if result.Config.Root != ".agent-mail" {
		t.Errorf("expected root=.agent-mail, got %q", result.Config.Root)
	}
	// 'me' is not in .amqrc

	// Verify Dir is set correctly (should be the root where .amqrc was found)
	resolvedDir, _ := filepath.EvalSymlinks(result.Dir)
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	if resolvedDir != resolvedRoot {
		t.Errorf("expected Dir=%q, got %q", resolvedRoot, resolvedDir)
	}
}

func TestFindAndLoadAmqrcNotFound(t *testing.T) {
	// Create an empty temp directory
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	_, err := findAndLoadAmqrc()
	if !errors.Is(err, errAmqrcNotFound) {
		t.Errorf("expected errAmqrcNotFound, got %v", err)
	}
}

func TestFindAndLoadAmqrcInvalidJSON(t *testing.T) {
	root := t.TempDir()

	// Write invalid .amqrc
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	_, err := findAndLoadAmqrc()
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
	if errors.Is(err, errAmqrcNotFound) {
		t.Error("should not be errAmqrcNotFound for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid .amqrc") {
		t.Errorf("expected 'invalid .amqrc' in error, got: %v", err)
	}
}

func TestDetectAgentMailDir(t *testing.T) {
	// Create temp directory with .agent-mail
	root := t.TempDir()
	agentMailDir := filepath.Join(root, ".agent-mail")
	if err := os.MkdirAll(agentMailDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	subdir := filepath.Join(root, "sub", "deep")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Run("finds in current directory", func(t *testing.T) {
		if err := os.Chdir(root); err != nil {
			t.Fatalf("chdir: %v", err)
		}

		detected := detectAgentMailDir()
		if detected != ".agent-mail" {
			t.Errorf("expected .agent-mail, got %q", detected)
		}
	})

	t.Run("finds in parent directory", func(t *testing.T) {
		if err := os.Chdir(subdir); err != nil {
			t.Fatalf("chdir: %v", err)
		}

		detected := detectAgentMailDir()
		// Compare resolved paths (handles macOS /var -> /private/var symlink)
		detectedResolved, _ := filepath.EvalSymlinks(detected)
		expectedResolved, _ := filepath.EvalSymlinks(agentMailDir)
		if detectedResolved != expectedResolved {
			t.Errorf("expected %q, got %q", expectedResolved, detectedResolved)
		}
	})
}

func TestDetectAgentMailDirNotFound(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	detected := detectAgentMailDir()
	if detected != "" {
		t.Errorf("expected empty string, got %q", detected)
	}
}

func TestResolveEnvConfigFromAmqrc(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	// Clear env vars
	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	rootVal, meVal, err := resolveEnvConfig("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfig: %v", err)
	}

	// Root should be resolved to base/default_session ("team")
	expectedRoot := filepath.Join(root, ".agent-mail", "team")
	resolvedRoot, _ := filepath.EvalSymlinks(rootVal)
	expectedResolved, _ := filepath.EvalSymlinks(expectedRoot)
	if resolvedRoot != expectedResolved {
		t.Errorf("expected root=%q, got %q", expectedResolved, resolvedRoot)
	}
	// me is NOT read from .amqrc (use env var or flag instead)
	if meVal != "" {
		t.Errorf("expected me=empty (not from .amqrc), got %q", meVal)
	}
}

func TestResolveEnvConfigRelativeRootFromSubdir(t *testing.T) {
	// This tests the fix for: relative root should be resolved against .amqrc location,
	// not CWD
	root := t.TempDir()
	subdir := filepath.Join(root, "sub", "deep")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write .amqrc in root with relative path
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	// Change to subdir (different from where .amqrc is)
	if err := os.Chdir(subdir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	rootVal, _, err := resolveEnvConfig("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfig: %v", err)
	}

	// Root should be resolved relative to .amqrc location + default session
	expectedRoot := filepath.Join(root, ".agent-mail", "team")
	resolvedRoot, _ := filepath.EvalSymlinks(rootVal)
	expectedResolved, _ := filepath.EvalSymlinks(expectedRoot)
	if resolvedRoot != expectedResolved {
		t.Errorf("expected root=%q (relative to .amqrc + team), got %q", expectedResolved, resolvedRoot)
	}
}

func TestResolveEnvConfigCustomDefaultSession(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc with custom default_session
	rcContent := `{"root": ".agent-mail", "default_session": "main"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	rootVal, _, err := resolveEnvConfig("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfig: %v", err)
	}

	// Root should use custom default_session "main" instead of "team"
	expectedRoot := filepath.Join(root, ".agent-mail", "main")
	resolvedRoot, _ := filepath.EvalSymlinks(rootVal)
	expectedResolved, _ := filepath.EvalSymlinks(expectedRoot)
	if resolvedRoot != expectedResolved {
		t.Errorf("expected root=%q (custom session), got %q", expectedResolved, resolvedRoot)
	}
}

func TestResolveEnvConfigFlagOverride(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc with one set of values
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Flags should override .amqrc
	rootVal, meVal, err := resolveEnvConfig("/custom/root", "codex")
	if err != nil {
		t.Fatalf("resolveEnvConfig: %v", err)
	}

	if rootVal != "/custom/root" {
		t.Errorf("expected root=/custom/root, got %q", rootVal)
	}
	if meVal != "codex" {
		t.Errorf("expected me=codex, got %q", meVal)
	}
}

func TestResolveEnvConfigEnvOverride(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc with one set of values
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	// Set env vars - should override .amqrc but not flags
	_ = os.Setenv("AM_ROOT", "/env/root")
	_ = os.Setenv("AM_ME", "envagent")
	defer func() { _ = os.Unsetenv("AM_ROOT") }()
	defer func() { _ = os.Unsetenv("AM_ME") }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	rootVal, meVal, err := resolveEnvConfig("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfig: %v", err)
	}

	if rootVal != "/env/root" {
		t.Errorf("expected root=/env/root, got %q", rootVal)
	}
	if meVal != "envagent" {
		t.Errorf("expected me=envagent, got %q", meVal)
	}
}

func TestResolveEnvConfigFlagOverridesEnv(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	// Set env vars
	_ = os.Setenv("AM_ROOT", "/env/root")
	_ = os.Setenv("AM_ME", "envagent")
	defer func() { _ = os.Unsetenv("AM_ROOT") }()
	defer func() { _ = os.Unsetenv("AM_ME") }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Flags should override both env and .amqrc
	rootVal, meVal, err := resolveEnvConfig("/flag/root", "flagagent")
	if err != nil {
		t.Fatalf("resolveEnvConfig: %v", err)
	}

	if rootVal != "/flag/root" {
		t.Errorf("expected root=/flag/root (flag), got %q", rootVal)
	}
	if meVal != "flagagent" {
		t.Errorf("expected me=flagagent (flag), got %q", meVal)
	}
}

func TestResolveEnvConfigAutoDetect(t *testing.T) {
	root := t.TempDir()

	// Create .agent-mail directory (no .amqrc)
	agentMailDir := filepath.Join(root, ".agent-mail")
	if err := os.MkdirAll(agentMailDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	rootVal, meVal, err := resolveEnvConfig("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfig: %v", err)
	}

	// Auto-detect finds .agent-mail but now appends default session
	expectedRoot := filepath.Join(".agent-mail", "team")
	if rootVal != expectedRoot {
		t.Errorf("expected root=%s, got %q", expectedRoot, rootVal)
	}
	if meVal != "" {
		t.Errorf("expected me=empty (not in .amqrc), got %q", meVal)
	}
}

func TestResolveEnvConfigInvalidAmqrcError(t *testing.T) {
	root := t.TempDir()

	// Write invalid .amqrc
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Should return error when no override provided
	_, _, err := resolveEnvConfig("", "")
	if err == nil {
		t.Error("expected error for invalid .amqrc with no override")
	}
	if !strings.Contains(err.Error(), "invalid .amqrc") {
		t.Errorf("expected 'invalid .amqrc' in error, got: %v", err)
	}
}

func TestResolveEnvConfigInvalidAmqrcWithOverride(t *testing.T) {
	root := t.TempDir()

	// Write invalid .amqrc
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Should succeed when override provided (flag takes precedence over broken .amqrc)
	rootVal, meVal, err := resolveEnvConfig("/override/root", "overrideagent")
	if err != nil {
		t.Fatalf("expected success with override, got error: %v", err)
	}
	if rootVal != "/override/root" {
		t.Errorf("expected root=/override/root, got %q", rootVal)
	}
	if meVal != "overrideagent" {
		t.Errorf("expected me=overrideagent, got %q", meVal)
	}
}

func TestResolveEnvConfigInvalidAmqrcWithEnvOverride(t *testing.T) {
	root := t.TempDir()

	// Write invalid .amqrc
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	// Set env var override
	_ = os.Setenv("AM_ROOT", "/env/override")
	defer func() { _ = os.Unsetenv("AM_ROOT") }()
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Should succeed when env override provided
	rootVal, _, err := resolveEnvConfig("", "")
	if err != nil {
		t.Fatalf("expected success with env override, got error: %v", err)
	}
	if rootVal != "/env/override" {
		t.Errorf("expected root=/env/override, got %q", rootVal)
	}
}

func TestResolveEnvConfigInvalidAmqrcWithAutoDetect(t *testing.T) {
	root := t.TempDir()

	// Write invalid .amqrc
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	// Create .agent-mail directory (lower precedence than .amqrc)
	if err := os.MkdirAll(filepath.Join(root, ".agent-mail"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Auto-detect is LOWER precedence than .amqrc, so it should NOT override an invalid .amqrc
	_, _, err := resolveEnvConfig("", "")
	if err == nil {
		t.Error("expected error for invalid .amqrc even with auto-detect available")
	}
	if !strings.Contains(err.Error(), "invalid .amqrc") {
		t.Errorf("expected 'invalid .amqrc' in error, got: %v", err)
	}
}

func TestResolveEnvConfigNoConfig(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// No .amqrc, no .agent-mail, no env vars
	_, _, err := resolveEnvConfig("", "")
	if err == nil {
		t.Error("expected error when no config found")
	}
	if !strings.Contains(err.Error(), "cannot determine root") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRunEnvJSON(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runEnv([]string{"--json"})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	var result envOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v, output was: %s", err, output)
	}

	// Root is now resolved to base/default_session
	expectedRoot := filepath.Join(root, ".agent-mail", "team")
	resolvedResult, _ := filepath.EvalSymlinks(result.Root)
	resolvedExpected, _ := filepath.EvalSymlinks(expectedRoot)
	if resolvedResult != resolvedExpected {
		t.Errorf("expected root=%q, got %q", resolvedExpected, resolvedResult)
	}
	// 'me' is not in .amqrc
	if result.Me != "" {
		t.Errorf("expected me=empty, got %q", result.Me)
	}
}

func TestRunEnvPosix(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc with absolute path to avoid resolution issues
	rcContent := `{"root": "/tmp/test-root"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runEnv([]string{})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "export AM_ROOT=/tmp/test-root/team") {
		t.Errorf("expected export AM_ROOT with /team suffix, got: %s", output)
	}
	// AM_ME is not set from .amqrc (use env var or --me flag)
	if strings.Contains(output, "export AM_ME") {
		t.Errorf("unexpected AM_ME in output (not from .amqrc): %s", output)
	}
}

func TestRunEnvFish(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc with absolute path
	rcContent := `{"root": "/tmp/test-root"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runEnv([]string{"--shell", "fish"})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "set -gx AM_ROOT /tmp/test-root/team") {
		t.Errorf("expected set -gx AM_ROOT with /team suffix, got: %s", output)
	}
	// AM_ME is not set from .amqrc (use env var or --me flag)
	if strings.Contains(output, "set -gx AM_ME") {
		t.Errorf("unexpected AM_ME in output (not from .amqrc): %s", output)
	}
}

func TestRunEnvWake(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc (wake test doesn't check root value, just wake output)
	rcContent := `{"root": "/tmp/test-root"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runEnv([]string{"--wake"})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "amq wake &") {
		t.Errorf("expected 'amq wake &', got: %s", output)
	}
}

func TestIsValidShell(t *testing.T) {
	valid := []string{"sh", "bash", "zsh", "fish"}
	for _, s := range valid {
		if !isValidShell(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}

	invalid := []string{"powershell", "cmd", "tcsh", ""}
	for _, s := range invalid {
		if isValidShell(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}
