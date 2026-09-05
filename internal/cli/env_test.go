package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func expectedPosixIdentityLines(t *testing.T, root, baseRoot string) string {
	t.Helper()
	rootID, rootErr := resolveTreeIdentityToken(root)
	baseRootID, baseErr := resolveTreeIdentityToken(baseRoot)
	var out strings.Builder
	if rootErr == nil {
		fmt.Fprintf(&out, "export AM_ROOT_ID=%s\n", shellQuotePosix(rootID))
	} else {
		out.WriteString("unset AM_ROOT_ID\n")
	}
	if baseErr == nil {
		fmt.Fprintf(&out, "export AM_BASE_ROOT_ID=%s\n", shellQuotePosix(baseRootID))
	} else {
		out.WriteString("unset AM_BASE_ROOT_ID\n")
	}
	return out.String()
}

func expectedFishIdentityLines(t *testing.T, root, baseRoot string) string {
	t.Helper()
	rootID, rootErr := resolveTreeIdentityToken(root)
	baseRootID, baseErr := resolveTreeIdentityToken(baseRoot)
	var out strings.Builder
	if rootErr == nil {
		fmt.Fprintf(&out, "set -gx AM_ROOT_ID %s\n", shellQuoteFish(rootID))
	} else {
		out.WriteString("set -e AM_ROOT_ID\n")
	}
	if baseErr == nil {
		fmt.Fprintf(&out, "set -gx AM_BASE_ROOT_ID %s\n", shellQuoteFish(baseRootID))
	} else {
		out.WriteString("set -e AM_BASE_ROOT_ID\n")
	}
	return out.String()
}

func captureEnvStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	runErr := fn()

	_ = w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	_ = r.Close()

	return buf.String(), runErr
}

func captureEnvOutput(t *testing.T, fn func() error) (stdout, stderr string, runErr error) {
	t.Helper()

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wOut
	os.Stderr = wErr
	t.Cleanup(func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	})

	runErr = fn()

	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr

	var outBuf bytes.Buffer
	_, _ = outBuf.ReadFrom(rOut)
	_ = rOut.Close()

	var errBuf bytes.Buffer
	_, _ = errBuf.ReadFrom(rErr)
	_ = rErr.Close()

	return outBuf.String(), errBuf.String(), runErr
}

func runEnvJSONForTest(t *testing.T, args ...string) envOutput {
	t.Helper()

	outArgs := append([]string{"--json"}, args...)
	output, err := captureEnvStdout(t, func() error {
		return runEnv(outArgs)
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	var result envOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v, output was: %s", err, output)
	}
	return result
}

func expectSamePath(t *testing.T, got, want string) {
	t.Helper()

	resolvedGot := canonicalTestPath(t, got)
	resolvedWant := canonicalTestPath(t, want)
	if resolvedGot != resolvedWant {
		t.Errorf("expected path %q, got %q", resolvedWant, resolvedGot)
	}
}

// canonicalTestPath resolves symlinks in the longest existing prefix while
// preserving any nonexistent suffix. EvalSymlinks returns an empty result on
// failure, so ignoring its error can make two different missing paths compare
// equal and let a resolution regression pass unnoticed.
func canonicalTestPath(t *testing.T, path string) string {
	t.Helper()

	current, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("absolute path for %q: %v", path, err)
	}
	missing := make([]string, 0, 2)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		if !os.IsNotExist(err) {
			t.Fatalf("resolve path %q at existing prefix %q: %v", path, current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatalf("resolve path %q: no existing path prefix", path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func TestCanonicalTestPathPreservesNonexistentSuffix(t *testing.T) {
	parent := t.TempDir()
	left := canonicalTestPath(t, filepath.Join(parent, "missing", "left"))
	right := canonicalTestPath(t, filepath.Join(parent, "missing", "right"))
	if left == right {
		t.Fatalf("different nonexistent paths collapsed to %q", left)
	}
}

func setCLIVersionForTest(t *testing.T, version string) {
	t.Helper()

	// This mutates package state; do not use from parallel tests.
	oldVersion := cliVersion
	cliVersion = version
	t.Cleanup(func() { cliVersion = oldVersion })
}

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
		{"trailing\\", "'trailing\\\\'"},
		{"two\\\\slashes", "'two\\\\\\\\slashes'"},
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
	expectSamePath(t, result.Dir, root)
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
		expectSamePath(t, detected, agentMailDir)
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

	// Root should be the literal .amqrc root
	expectedRoot := filepath.Join(root, ".agent-mail")
	expectSamePath(t, rootVal, expectedRoot)
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

	// Root should be resolved relative to .amqrc location (literal)
	expectedRoot := filepath.Join(root, ".agent-mail")
	expectSamePath(t, rootVal, expectedRoot)
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

	// Isolate from the developer's real global config so this test exercises
	// auto-detection without unrelated fallback warnings.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMQ_GLOBAL_ROOT", "")

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

	// Auto-detect returns the canonical tree (no session appended).
	if !filepath.IsAbs(rootVal) {
		t.Errorf("expected absolute auto-detected root, got %q", rootVal)
	}
	expectSamePath(t, rootVal, agentMailDir)
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
	root := setupProjectFixture(t, "codex")

	// Isolate from the developer's real global config: a ~/.amqrc or
	// AMQ_GLOBAL_ROOT on the machine would make resolution succeed.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMQ_GLOBAL_ROOT", "")

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
	for _, want := range []string{"from your terminal", "amq setup --project-root \"$PWD\""} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no-root error missing recovery instruction %q: %v", want, err)
		}
	}
	// Exercise the printed terminal command, accepting its prompted defaults.
	// A preview/-y suggestion without the required first-setup flags is a loop.
	setupIsTerminal = func() bool { return true }
	restoreInput := withStdin(t, "\n\n\nyes\n")
	defer restoreInput()
	if _, err := captureEnvStdout(t, func() error {
		return runSetup([]string{"--project-root", root})
	}); err != nil {
		t.Fatalf("suggested setup: %v", err)
	}
	if got, _, err := resolveEnvConfig("", ""); err != nil || got == "" {
		t.Fatalf("env after suggested recovery: root=%q err=%v", got, err)
	}
}

func TestRunEnvJSON(t *testing.T) {
	root := t.TempDir()
	setCLIVersionForTest(t, "test-version")

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

	result := runEnvJSONForTest(t)

	// Root is the literal .amqrc root
	expectedRoot := filepath.Join(root, ".agent-mail")
	expectSamePath(t, result.Root, expectedRoot)
	expectSamePath(t, result.BaseRoot, expectedRoot)
	if result.SchemaVersion != 1 {
		t.Errorf("expected schema_version=1, got %d", result.SchemaVersion)
	}
	if result.AMQVersion != "test-version" {
		t.Errorf("expected amq_version=%q, got %q", "test-version", result.AMQVersion)
	}
	if result.RootSource != string(rootSourceProjectRC) {
		t.Errorf("expected root_source=%q, got %q", rootSourceProjectRC, result.RootSource)
	}
	if result.InSession {
		t.Error("expected in_session=false")
	}
	if result.SessionName != "" {
		t.Errorf("expected session_name=empty, got %q", result.SessionName)
	}
	// 'me' is not in .amqrc
	if result.Me != "" {
		t.Errorf("expected me=empty, got %q", result.Me)
	}
	// Project defaults to directory basename when not set explicitly
	expectedProject := filepath.Base(root)
	if result.Project != expectedProject {
		t.Errorf("expected project=%q, got %q", expectedProject, result.Project)
	}
	// No peers configured
	if len(result.Peers) != 0 {
		t.Errorf("expected peers={}, got %v", result.Peers)
	}
}

func TestRunEnvJSONWithPeers(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc with project + peers
	rcContent := `{"root": ".agent-mail", "project": "my-app", "peers": {"infra": "/tmp/infra/.agent-mail", "api": "/tmp/api/.agent-mail", "shared": "../shared/.agent-mail"}}`
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

	result := runEnvJSONForTest(t, "--me", "claude")

	if result.Project != "my-app" {
		t.Errorf("expected project=%q, got %q", "my-app", result.Project)
	}
	if result.Me != "claude" {
		t.Errorf("expected me=%q, got %q", "claude", result.Me)
	}
	if len(result.Peers) != 3 {
		t.Fatalf("expected 3 peers, got %d", len(result.Peers))
	}
	if result.Peers["infra"] != "/tmp/infra/.agent-mail" {
		t.Errorf("expected peer infra=%q, got %q", "/tmp/infra/.agent-mail", result.Peers["infra"])
	}
	if result.Peers["api"] != "/tmp/api/.agent-mail" {
		t.Errorf("expected peer api=%q, got %q", "/tmp/api/.agent-mail", result.Peers["api"])
	}
	expectedShared, err := filepath.Abs(filepath.Join(root, "../shared/.agent-mail"))
	if err != nil {
		t.Fatalf("abs shared peer: %v", err)
	}
	expectSamePath(t, result.Peers["shared"], expectedShared)
}

func TestRunEnvJSONGlobalAmqrcNoProject(t *testing.T) {
	// Regression: global ~/.amqrc should not infer project from home dir basename.
	fakeHome := t.TempDir()

	// Write ~/.amqrc (global, no project field)
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	// Create the .agent-mail dir so root resolves
	if err := os.MkdirAll(filepath.Join(fakeHome, ".agent-mail"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Use an unrelated cwd with no project .amqrc
	cwd := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	oldHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", oldHome) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")
	_ = os.Unsetenv("AMQ_GLOBAL_ROOT")
	_ = os.Setenv("HOME", fakeHome)

	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t)

	// Project should be empty — global ~/.amqrc is a queue locator, not a project identity
	if result.Project != "" {
		t.Errorf("expected project=empty for global ~/.amqrc, got %q", result.Project)
	}
	if result.RootSource != string(rootSourceGlobalRC) {
		t.Errorf("expected root_source=%q, got %q", rootSourceGlobalRC, result.RootSource)
	}
	if len(result.Peers) != 0 {
		t.Errorf("expected peers={}, got %v", result.Peers)
	}
}

func TestRunEnvInvalidGlobalAmqrcBeatsAutoDetectOutsideGit(t *testing.T) {
	cwd := t.TempDir()
	fakeHome := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".agent-mail"), 0o755); err != nil {
		t.Fatalf("mkdir .agent-mail: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write invalid ~/.amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("HOME", fakeHome)
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	_, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--json"})
	})
	if err == nil || !strings.Contains(err.Error(), "invalid ~/.amqrc") {
		t.Fatalf("error = %v, want invalid global config before outside-Git auto-detect", err)
	}
}

func TestRunEnvJSONV1SessionFlag(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude", "agents"), 0o700); err != nil {
		t.Fatalf("mkdir unrelated agents dir: %v", err)
	}

	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")
	_ = os.Unsetenv("AMQ_GLOBAL_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t, "--session", "feature-x", "--me", "codex")

	expectedBase := filepath.Join(root, ".agent-mail")
	expectedRoot := filepath.Join(expectedBase, "feature-x")
	expectSamePath(t, result.Root, expectedRoot)
	expectSamePath(t, result.BaseRoot, expectedBase)
	if !result.InSession {
		t.Error("expected in_session=true")
	}
	if result.SessionName != "feature-x" {
		t.Errorf("expected session_name=%q, got %q", "feature-x", result.SessionName)
	}
	if result.Me != "codex" {
		t.Errorf("expected me=%q, got %q", "codex", result.Me)
	}
	if result.RootSource != string(rootSourceFlag) {
		t.Errorf("expected root_source=%q, got %q", rootSourceFlag, result.RootSource)
	}
}

func TestRunEnvSessionHonorsConsistentLegacyPin(t *testing.T) {
	baseRoot := t.TempDir()
	currentRoot := filepath.Join(baseRoot, "current")
	targetRoot := filepath.Join(baseRoot, "feature-x")
	for _, root := range []string{currentRoot, targetRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("mkdir session root %s: %v", root, err)
		}
	}
	t.Setenv(envRoot, currentRoot)
	t.Setenv(envBaseRoot, baseRoot)
	t.Setenv(envSession, "current")
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)

	result := runEnvJSONForTest(t, "--session", "feature-x", "--me", "codex")
	expectSamePath(t, result.Root, targetRoot)
	expectSamePath(t, result.BaseRoot, baseRoot)
	if result.SessionName != "feature-x" {
		t.Fatalf("session_name = %q, want feature-x", result.SessionName)
	}
}

func TestRunEnvSessionRejectsAMRootOutsideLegacyPin(t *testing.T) {
	baseRoot := t.TempDir()
	currentRoot := filepath.Join(baseRoot, "current")
	targetRoot := filepath.Join(baseRoot, "feature-x")
	foreignRoot := filepath.Join(t.TempDir(), "foreign")
	for _, root := range []string{currentRoot, targetRoot, foreignRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("mkdir session root %s: %v", root, err)
		}
	}
	t.Setenv(envRoot, foreignRoot)
	t.Setenv(envBaseRoot, baseRoot)
	t.Setenv(envSession, "current")
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "feature-x", "--me", "codex", "--json"})
	})
	if err == nil || !strings.Contains(err.Error(), "differs from pinned root") {
		t.Fatalf("runEnv error = %v, want AM_ROOT/legacy-pin mismatch refusal", err)
	}
	if stdout != "" {
		t.Fatalf("runEnv emitted a replacement context after mismatch: %q", stdout)
	}
}

func TestRunEnvIncompleteLegacyPinNamesRepinRemedy(t *testing.T) {
	repo := t.TempDir()
	nested := filepath.Join(repo, "nested")
	baseRoot := filepath.Join(repo, defaultCoopRoot)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir git marker: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(baseRoot, "current"), 0o700); err != nil {
		t.Fatalf("mkdir session root: %v", err)
	}
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("mkdir nested cwd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".amqrc"), []byte(`{"root": ".agent-mail"}`), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	t.Chdir(nested)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envRoot, "")
	t.Setenv(envBaseRoot, baseRoot)
	t.Setenv(envSession, "current")
	t.Setenv(envGlobalRoot, "")
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--me", "codex", "--json"})
	})
	if err == nil || GetExitCode(err) != ExitContextMismatch ||
		!strings.Contains(err.Error(), "amq env --session <name>") {
		t.Fatalf("runEnv error = %v, want exit-5 repin remedy", err)
	}
	if stdout != "" {
		t.Fatalf("runEnv emitted a replacement context after mismatch: %q", stdout)
	}
}

func TestRunEnvNonExportSessionEmitsFullContext(t *testing.T) {
	root := t.TempDir()
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	projectRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "feature-x", "--me", "codex"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	expectedBase := filepath.Join(projectRoot, ".agent-mail")
	expectedRoot := filepath.Join(expectedBase, "feature-x")
	want := "export AM_ROOT=" + shellQuotePosix(expectedRoot) + "\n" +
		"export AM_BASE_ROOT=" + shellQuotePosix(expectedBase) + "\n" +
		expectedPosixIdentityLines(t, expectedRoot, expectedBase) +
		"export AM_SESSION=feature-x\n" +
		"export AM_ME=codex\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

func TestRunEnvExportSessionEmitsBaseRootAndPinNote(t *testing.T) {
	root := t.TempDir()
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	projectRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "feature-x", "--me", "codex", "--export"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	expectedBase := filepath.Join(projectRoot, ".agent-mail")
	expectedRoot := filepath.Join(expectedBase, "feature-x")
	want := "export AM_ROOT=" + shellQuotePosix(expectedRoot) + "\n" +
		"export AM_BASE_ROOT=" + shellQuotePosix(expectedBase) + "\n" +
		expectedPosixIdentityLines(t, expectedRoot, expectedBase) +
		"export AM_SESSION=feature-x\n" +
		"export AM_ME=codex\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "pinned to AMQ session feature-x") {
		t.Fatalf("stderr should contain session pin note, got %q", stderr)
	}
	if !strings.Contains(stderr, "one terminal, one session") {
		t.Fatalf("stderr should mention one terminal, one session, got %q", stderr)
	}
}

func TestRunEnvExportNonSessionPinsExactBaseRoot(t *testing.T) {
	root := t.TempDir()
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	projectRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--me", "codex", "--export"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	expectedRoot := filepath.Join(projectRoot, ".agent-mail")
	want := "export AM_ROOT=" + shellQuotePosix(expectedRoot) + "\n" +
		"export AM_BASE_ROOT=" + shellQuotePosix(expectedRoot) + "\n" +
		expectedPosixIdentityLines(t, expectedRoot, expectedRoot) +
		"export AM_SESSION=\n" +
		"export AM_ME=codex\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stdout, "export AM_BASE_ROOT="+shellQuotePosix(expectedRoot)) {
		t.Fatalf("non-session export should pin exact AM_BASE_ROOT: %q", stdout)
	}
	if !strings.Contains(stderr, "pinned to AMQ root") {
		t.Fatalf("stderr should contain root pin note, got %q", stderr)
	}
}

func TestRunEnvExportFishSessionEmitsBaseRoot(t *testing.T) {
	root := t.TempDir()
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	projectRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "feature-x", "--me", "codex", "--export", "--shell", "fish"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	expectedBase := filepath.Join(projectRoot, ".agent-mail")
	expectedRoot := filepath.Join(expectedBase, "feature-x")
	want := "set -gx AM_ROOT " + shellQuoteFish(expectedRoot) + "\n" +
		"set -gx AM_BASE_ROOT " + shellQuoteFish(expectedBase) + "\n" +
		expectedFishIdentityLines(t, expectedRoot, expectedBase) +
		"set -gx AM_SESSION feature-x\n" +
		"set -gx AM_ME codex\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestRunEnvExportSessionFromGlobalRootEmitsBaseRoot(t *testing.T) {
	cwd := t.TempDir()
	globalRoot := filepath.Join(t.TempDir(), "custom-root")

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", globalRoot)

	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "feature-x", "--me", "codex", "--export"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	expectedRoot := filepath.Join(globalRoot, "feature-x")
	want := "export AM_ROOT=" + shellQuotePosix(expectedRoot) + "\n" +
		"export AM_BASE_ROOT=" + shellQuotePosix(globalRoot) + "\n" +
		expectedPosixIdentityLines(t, expectedRoot, globalRoot) +
		"export AM_SESSION=feature-x\n" +
		"export AM_ME=codex\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "pinned to AMQ session feature-x") {
		t.Fatalf("stderr should contain session pin note, got %q", stderr)
	}
}

func TestRunEnvExportSessionIgnoresStaleAmbientBaseRoot(t *testing.T) {
	root := t.TempDir()
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", filepath.Join(t.TempDir(), "stale-root"))
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	projectRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "feature-x", "--me", "codex", "--export"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	expectedBase := filepath.Join(projectRoot, ".agent-mail")
	expectedRoot := filepath.Join(expectedBase, "feature-x")
	want := "export AM_ROOT=" + shellQuotePosix(expectedRoot) + "\n" +
		"export AM_BASE_ROOT=" + shellQuotePosix(expectedBase) + "\n" +
		expectedPosixIdentityLines(t, expectedRoot, expectedBase) +
		"export AM_SESSION=feature-x\n" +
		"export AM_ME=codex\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if strings.Contains(stdout, "stale-root") {
		t.Fatalf("stdout should not contain stale AM_BASE_ROOT: %q", stdout)
	}
}

func TestRunEnvExportExplicitBaseRootPinsExactBaseRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".agent-mail")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--root", root, "--me", "codex", "--export"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	want := "export AM_ROOT=" + shellQuotePosix(root) + "\n" +
		"export AM_BASE_ROOT=" + shellQuotePosix(root) + "\n" +
		expectedPosixIdentityLines(t, root, root) +
		"export AM_SESSION=\n" +
		"export AM_ME=codex\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stdout, "export AM_BASE_ROOT="+shellQuotePosix(root)) {
		t.Fatalf("explicit base-root export should pin exact AM_BASE_ROOT: %q", stdout)
	}
}

func TestRunEnvExportMutuallyExclusiveOutputModes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "json",
			args: []string{"--export", "--json"},
			want: "--export and --json are mutually exclusive",
		},
		{
			name: "session-name",
			args: []string{"--export", "--session-name"},
			want: "--export and --session-name are mutually exclusive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := captureEnvOutput(t, func() error {
				return runEnv(tt.args)
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRunEnvJSONV1FieldsAlwaysPresent(t *testing.T) {
	cwd := t.TempDir()
	fakeHome := t.TempDir()
	root := filepath.Join(cwd, "explicit-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("HOME", fakeHome)
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	output, err := captureEnvStdout(t, func() error {
		return runEnv([]string{"--json", "--root", root})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &raw); err != nil {
		t.Fatalf("unmarshal raw output: %v, output was: %s", err, output)
	}

	required := []string{
		"schema_version",
		"amq_version",
		"root",
		"base_root",
		"session_name",
		"in_session",
		"me",
		"project",
		"root_source",
		"peers",
	}
	for _, key := range required {
		if _, ok := raw[key]; !ok {
			t.Errorf("expected v1 field %q to be present in JSON output", key)
		}
	}
	if got := string(raw["peers"]); got != "{}" {
		t.Errorf("expected peers to serialize as {}, got %s", got)
	}
}

func TestRunEnvJSONV1ExplicitRoot(t *testing.T) {
	cwd := t.TempDir()
	fakeHome := t.TempDir()
	root := filepath.Join(cwd, "explicit-root")

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("HOME", fakeHome)
	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")
	_ = os.Unsetenv("AMQ_GLOBAL_ROOT")

	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t, "--root", root)

	if result.Root != root {
		t.Errorf("expected root=%q, got %q", root, result.Root)
	}
	if result.BaseRoot != root {
		t.Errorf("expected base_root=%q, got %q", root, result.BaseRoot)
	}
	if result.RootSource != string(rootSourceFlag) {
		t.Errorf("expected root_source=%q, got %q", rootSourceFlag, result.RootSource)
	}
	if result.InSession {
		t.Error("expected in_session=false")
	}
	if result.Project != "" {
		t.Errorf("expected project=empty, got %q", result.Project)
	}
	if len(result.Peers) != 0 {
		t.Errorf("expected peers={}, got %v", result.Peers)
	}
}

func TestRunEnvJSONV1CustomRootFromProjectAmqrc(t *testing.T) {
	root := t.TempDir()

	rcContent := `{"root": "custom-root"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")
	_ = os.Unsetenv("AMQ_GLOBAL_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t)

	expectedRoot := filepath.Join(root, "custom-root")
	expectSamePath(t, result.Root, expectedRoot)
	expectSamePath(t, result.BaseRoot, expectedRoot)
	if result.RootSource != string(rootSourceProjectRC) {
		t.Errorf("expected root_source=%q, got %q", rootSourceProjectRC, result.RootSource)
	}
	if result.InSession {
		t.Error("expected in_session=false")
	}
}

func TestRunEnvJSONV1GlobalRoot(t *testing.T) {
	cwd := t.TempDir()
	fakeHome := t.TempDir()
	globalRoot := filepath.Join(t.TempDir(), "global-root")

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("HOME", fakeHome)
	t.Setenv("AMQ_GLOBAL_ROOT", globalRoot)
	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t)

	if result.Root != globalRoot {
		t.Errorf("expected root=%q, got %q", globalRoot, result.Root)
	}
	if result.BaseRoot != globalRoot {
		t.Errorf("expected base_root=%q, got %q", globalRoot, result.BaseRoot)
	}
	if result.RootSource != string(rootSourceGlobalEnv) {
		t.Errorf("expected root_source=%q, got %q", rootSourceGlobalEnv, result.RootSource)
	}
	if result.Project != "" {
		t.Errorf("expected project=empty, got %q", result.Project)
	}
	if len(result.Peers) != 0 {
		t.Errorf("expected peers={}, got %v", result.Peers)
	}
}

func TestRunEnvJSONV1AutoDetect(t *testing.T) {
	cwd := t.TempDir()
	fakeHome := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".agent-mail"), 0o755); err != nil {
		t.Fatalf("mkdir .agent-mail: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("HOME", fakeHome)
	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")
	_ = os.Unsetenv("AMQ_GLOBAL_ROOT")

	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t)

	expectedRoot := filepath.Join(cwd, ".agent-mail")
	if !filepath.IsAbs(result.Root) || !filepath.IsAbs(result.BaseRoot) {
		t.Errorf("expected absolute root/base_root, got root=%q base=%q", result.Root, result.BaseRoot)
	}
	expectSamePath(t, result.Root, expectedRoot)
	expectSamePath(t, result.BaseRoot, expectedRoot)
	if result.RootSource != string(rootSourceAutoDetect) {
		t.Errorf("expected root_source=%q, got %q", rootSourceAutoDetect, result.RootSource)
	}
	if result.InSession {
		t.Error("expected in_session=false")
	}
	if result.SessionName != "" {
		t.Errorf("expected session_name=empty, got %q", result.SessionName)
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

	if !strings.Contains(output, "export AM_ROOT=/tmp/test-root\n") {
		t.Errorf("expected export AM_ROOT=/tmp/test-root, got: %s", output)
	}
	// A missing identity clears any stale terminal identity.
	if !strings.Contains(output, "unset AM_ME\n") {
		t.Errorf("expected stale AM_ME to be cleared, got: %s", output)
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

	if !strings.Contains(output, "set -gx AM_ROOT /tmp/test-root\n") {
		t.Errorf("expected set -gx AM_ROOT /tmp/test-root, got: %s", output)
	}
	// A missing identity clears any stale terminal identity.
	if !strings.Contains(output, "set -e AM_ME\n") {
		t.Errorf("expected stale AM_ME to be cleared, got: %s", output)
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

// --- Global root resolution tests ---

func TestLoadGlobalAmqrc(t *testing.T) {
	// Create a fake HOME with ~/.amqrc
	fakeHome := t.TempDir()
	rcContent := `{"root": "/global/agent-mail"}`
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write ~/.amqrc: %v", err)
	}

	// Override HOME so loadGlobalAmqrc finds it
	t.Setenv("HOME", fakeHome)

	result, err := loadGlobalAmqrc()
	if err != nil {
		t.Fatalf("loadGlobalAmqrc: %v", err)
	}
	if result.Config.Root != "/global/agent-mail" {
		t.Errorf("expected root=/global/agent-mail, got %q", result.Config.Root)
	}
	expectSamePath(t, result.Dir, fakeHome)
}

func TestFindAndLoadAmqrcRejectsUntrustedProvenance(t *testing.T) {
	root := t.TempDir()
	old, _ := os.Getwd()
	defer func() { _ = os.Chdir(old) }()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, ".amqrc")
	if err := os.WriteFile(path, []byte(`{"root":".agent-mail"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := findAndLoadAmqrc(); err == nil || !strings.Contains(err.Error(), "group/world-writable") {
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
	if _, err := findAndLoadAmqrc(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestLoadGlobalAmqrcNotFound(t *testing.T) {
	// Create a fake HOME without ~/.amqrc
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	_, err := loadGlobalAmqrc()
	if !errors.Is(err, errAmqrcNotFound) {
		t.Errorf("expected errAmqrcNotFound, got %v", err)
	}
}

func TestLoadGlobalAmqrcReadErrorIsNotAbsence(t *testing.T) {
	fakeHome := t.TempDir()
	path := filepath.Join(fakeHome, ".amqrc")
	if err := os.WriteFile(path, []byte(`{"root":"/global/agent-mail"}`), 0o600); err != nil {
		t.Fatalf("write ~/.amqrc: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	originalReadFile := globalAmqrcReadFile
	globalAmqrcReadFile = func(string) ([]byte, error) {
		return nil, errors.New("read denied")
	}
	t.Cleanup(func() {
		globalAmqrcReadFile = originalReadFile
	})

	_, err := loadGlobalAmqrc()
	if err == nil || !strings.Contains(err.Error(), "cannot read ~/.amqrc") {
		t.Fatalf("loadGlobalAmqrc error = %v, want read failure", err)
	}
	if errors.Is(err, errAmqrcNotFound) {
		t.Fatalf("read failure was classified as absence: %v", err)
	}
}

func TestLoadGlobalAmqrcInvalidJSON(t *testing.T) {
	fakeHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write ~/.amqrc: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	_, err := loadGlobalAmqrc()
	if err == nil {
		t.Error("expected error for invalid JSON in ~/.amqrc")
	}
	if errors.Is(err, errAmqrcNotFound) {
		t.Error("should not be errAmqrcNotFound for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid ~/.amqrc") {
		t.Errorf("expected 'invalid ~/.amqrc' in error, got: %v", err)
	}
}

func TestFindAmqrcForRootUsesGlobalConfigOnlyForConfiguredTree(t *testing.T) {
	fakeHome := t.TempDir()
	globalRoot := filepath.Join(fakeHome, "global-mail")
	globalSession := filepath.Join(globalRoot, "session1")
	peerRoot := filepath.Join(fakeHome, "peer-mail")
	unrelatedRoot := filepath.Join(fakeHome, "workspace", "unrelated", ".agent-mail", "session1")
	for _, dir := range []string{globalSession, peerRoot, unrelatedRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	rcData, err := json.Marshal(amqrc{
		Root:    "global-mail",
		Project: "global-fabric",
		Peers:   map[string]string{"peer": "peer-mail"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatalf("write global .amqrc: %v", err)
	}
	t.Setenv("HOME", fakeHome)

	for _, root := range []string{globalRoot, globalSession} {
		result, err := findAmqrcForRoot(root)
		if err != nil {
			t.Fatalf("findAmqrcForRoot(%s): %v", root, err)
		}
		if result.Path != filepath.Join(fakeHome, ".amqrc") {
			t.Fatalf("config path for %s = %q, want global ~/.amqrc", root, result.Path)
		}
		project, peers := envProjectAndPeers(root)
		if project != "global-fabric" {
			t.Fatalf("project for %s = %q, want global-fabric", root, project)
		}
		expectSamePath(t, peers["peer"], peerRoot)
		resolvedPeer, err := resolvePeer(root, "peer")
		if err != nil {
			t.Fatalf("resolvePeer(%s): %v", root, err)
		}
		expectSamePath(t, resolvedPeer, peerRoot)
	}

	if _, err := findAmqrcForRoot(unrelatedRoot); !errors.Is(err, errAmqrcNotFound) {
		t.Fatalf("unrelated root reached global ~/.amqrc: %v", err)
	}
}

func TestGlobalAmqrcFallbackWhenProjectAbsent(t *testing.T) {
	// No project .amqrc, but global ~/.amqrc exists -> global wins
	projectDir := t.TempDir()
	fakeHome := t.TempDir()

	rcContent := `{"root": "/global/root"}`
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write ~/.amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", "") // Outside-repository fallback must not require git.
	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")
	_ = os.Unsetenv("AMQ_GLOBAL_ROOT")

	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfigWithSource: %v", err)
	}
	if root != "/global/root" {
		t.Errorf("expected root=/global/root, got %q", root)
	}
	if source != rootSourceGlobalRC {
		t.Errorf("expected source=global_amqrc, got %q", source)
	}
}

func TestGlobalAmqrcFallbackRefusedInUnconfiguredLinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for linked worktree routing")
	}

	fakeHome := t.TempDir()
	primary := filepath.Join(fakeHome, "workspace", "primary")
	linked := filepath.Join(fakeHome, "workspace", "linked")
	if err := os.MkdirAll(primary, 0o700); err != nil {
		t.Fatalf("mkdir primary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(primary, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGitForTest(t, primary, "init")
	runGitForTest(t, primary, "add", "README.md")
	runGitForTest(t, primary, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "-m", "fixture")
	runGitForTest(t, primary, "worktree", "add", "-b", "linked", linked)

	primaryRoot := filepath.Join(primary, ".agent-mail")
	globalRoot := filepath.Join(fakeHome, "global-agent-mail")
	for _, root := range []string{primaryRoot, globalRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("mkdir root %s: %v", root, err)
		}
	}
	if err := os.WriteFile(filepath.Join(primary, ".amqrc"), []byte(`{"root":".agent-mail","project":"primary"}`), 0o600); err != nil {
		t.Fatalf("write primary .amqrc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(`{"root":"`+globalRoot+`","project":"unrelated"}`), 0o600); err != nil {
		t.Fatalf("write global .amqrc: %v", err)
	}

	t.Setenv("HOME", fakeHome)
	setOptionalEnv(t, envRoot, "", false)
	setOptionalEnv(t, envBaseRoot, "", false)
	setOptionalEnv(t, envSession, "", false)
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)
	setOptionalEnv(t, envMe, "", false)
	setOptionalEnv(t, envGlobalRoot, "", false)
	t.Chdir(linked)
	t.Cleanup(resetAmqrcCache)
	resetAmqrcCache()

	if _, err := os.Stat(filepath.Join(linked, ".amqrc")); !os.IsNotExist(err) {
		t.Fatalf("linked .amqrc unexpectedly exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(linked, ".agent-mail")); !os.IsNotExist(err) {
		t.Fatalf("linked .agent-mail unexpectedly exists: %v", err)
	}
	if _, err := findAndLoadAmqrc(); !errors.Is(err, errAmqrcNotFound) {
		t.Fatalf("linked worktree project config = %v, want errAmqrcNotFound", err)
	}

	_, _, _, err := resolveEnvConfigWithSource("", "")
	if err == nil {
		t.Fatal("expected implicit ~/.amqrc fallback to be refused in an unconfigured linked worktree")
	}
	for _, want := range []string{"Git worktree", linked, "~/.amqrc", "--session", "AMQ_GLOBAL_ROOT"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "session1", "--me", "codex-lead"})
	})
	if err == nil || GetExitCode(err) != ExitContextMismatch || !strings.Contains(err.Error(), "Git worktree") {
		t.Fatalf("amq env --session error = %v, want context-mismatch refusal", err)
	}
	if stdout != "" {
		t.Fatalf("amq env --session emitted a wrong-root context: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(globalRoot, "session1")); !os.IsNotExist(err) {
		t.Fatalf("wrong-root session was mutated: %v", err)
	}
	if _, found, err := resolveDiscoveredBaseRoot(); err == nil || found {
		t.Fatalf("coop base discovery = found %v, err %v; want refusal", found, err)
	}

	t.Setenv(envGlobalRoot, primaryRoot)
	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		t.Fatalf("explicit AMQ_GLOBAL_ROOT: %v", err)
	}
	expectSamePath(t, root, primaryRoot)
	if source != rootSourceGlobalEnv {
		t.Fatalf("source = %q, want %q", source, rootSourceGlobalEnv)
	}
	result := runEnvJSONForTest(t, "--session", "session1", "--me", "codex-lead")
	expectSamePath(t, result.Root, filepath.Join(primaryRoot, "session1"))
	if result.RootSource != string(rootSourceFlag) {
		t.Fatalf("session root source = %q, want %q", result.RootSource, rootSourceFlag)
	}
}

func TestGlobalAmqrcFallbackRefusedForSubmoduleStyleGitFile(t *testing.T) {
	fakeHome := t.TempDir()
	submodule := filepath.Join(fakeHome, "workspace", "submodule")
	submoduleGitDir := filepath.Join(fakeHome, "workspace", "primary", ".git", "modules", "submodule")
	globalRoot := filepath.Join(fakeHome, "global-agent-mail")
	for _, dir := range []string{submodule, submoduleGitDir, globalRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(submodule, ".git"), []byte("gitdir: "+submoduleGitDir+"\n"), 0o600); err != nil {
		t.Fatalf("write submodule .git file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(`{"root":"`+globalRoot+`"}`), 0o600); err != nil {
		t.Fatalf("write global .amqrc: %v", err)
	}

	t.Setenv("HOME", fakeHome)
	setOptionalEnv(t, envRoot, "", false)
	setOptionalEnv(t, envBaseRoot, "", false)
	setOptionalEnv(t, envSession, "", false)
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)
	setOptionalEnv(t, envMe, "", false)
	setOptionalEnv(t, envGlobalRoot, "", false)
	t.Chdir(submodule)
	t.Cleanup(resetAmqrcCache)
	resetAmqrcCache()

	_, _, _, err := resolveEnvConfigWithSource("", "")
	if err == nil || GetExitCode(err) != ExitContextMismatch {
		t.Fatalf("submodule-style .git error = %v, want context mismatch", err)
	}
}

func TestGlobalAmqrcFallbackRefusedInPrimaryGitWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for primary worktree routing")
	}
	fakeHome := t.TempDir()
	repo := filepath.Join(fakeHome, "workspace", "primary")
	globalRoot := filepath.Join(fakeHome, "global-agent-mail")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, repo, "init")
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(`{"root":"`+globalRoot+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe, envGlobalRoot} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	t.Chdir(repo)
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	_, _, _, err := resolveEnvConfigWithSource("", "")
	if err == nil || GetExitCode(err) != ExitContextMismatch || !strings.Contains(err.Error(), repo) {
		t.Fatalf("primary worktree error = %v, want context mismatch naming %s", err, repo)
	}
}

func TestGitWorktreeDetectionUsesFilesystemAcrossSymlinkAndHostileGitEnv(t *testing.T) {
	realRepo := filepath.Join(t.TempDir(), "repo")
	nested := filepath.Join(realRepo, "nested")
	if err := os.MkdirAll(filepath.Join(realRepo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(realRepo, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "hostile"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	t.Setenv("GIT_COMMON_DIR", t.TempDir())
	t.Chdir(filepath.Join(alias, "nested"))

	top, ok := gitWorktreeRootFromCWD()
	if !ok {
		t.Fatal("filesystem-backed Git worktree was not detected")
	}
	expectSamePath(t, top, realRepo)
}

func TestGitWorktreeDetectionTreatsAnyMarkerOrInspectionFailureAsSafetySignal(t *testing.T) {
	t.Run("symlink marker", func(t *testing.T) {
		repo := t.TempDir()
		target := filepath.Join(t.TempDir(), "actual-git-dir")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(repo, ".git")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Chdir(repo)
		top, ok := gitWorktreeRootFromCWD()
		if !ok {
			t.Fatal(".git symlink was not treated as a routing safety signal")
		}
		expectSamePath(t, top, repo)
	})

	t.Run("inspection failure", func(t *testing.T) {
		repo := t.TempDir()
		t.Chdir(repo)
		physicalRepo, err := filepath.EvalSymlinks(repo)
		if err != nil {
			t.Fatal(err)
		}
		targetMarker := filepath.Join(physicalRepo, ".git")
		original := gitMarkerLstat
		gitMarkerLstat = func(path string) (os.FileInfo, error) {
			if filepath.Clean(path) == filepath.Clean(targetMarker) {
				return nil, os.ErrPermission
			}
			return os.Lstat(path)
		}
		t.Cleanup(func() { gitMarkerLstat = original })
		top, ok := gitWorktreeRootFromCWD()
		if !ok {
			t.Fatal("non-ENOENT .git inspection failure failed open")
		}
		expectSamePath(t, top, repo)
	})
}

func TestGitWorktreeDiscoveryStopsAtWorktreeRoot(t *testing.T) {
	for _, source := range []string{".amqrc", ".agent-mail"} {
		t.Run(source, func(t *testing.T) {
			fakeHome := t.TempDir()
			workspace := filepath.Join(t.TempDir(), "workspace")
			repo := filepath.Join(workspace, "repo")
			nested := filepath.Join(repo, "nested")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			ancestorRoot := filepath.Join(workspace, ".agent-mail")
			if source == ".amqrc" {
				if err := os.WriteFile(filepath.Join(workspace, ".amqrc"), []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(ancestorRoot, 0o700); err != nil {
				t.Fatal(err)
			}

			t.Setenv("HOME", fakeHome)
			for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe, envGlobalRoot} {
				setOptionalEnv(t, key, "", false)
			}
			t.Chdir(nested)
			resetAmqrcCache()
			t.Cleanup(resetAmqrcCache)

			if _, err := findAndLoadAmqrc(); !errors.Is(err, errAmqrcNotFound) {
				t.Fatalf("ancestor config escaped Git ceiling: %v", err)
			}
			if got := detectAgentMailDir(); got != "" {
				t.Fatalf("ancestor queue escaped Git ceiling: %q", got)
			}
			if _, _, _, err := resolveEnvConfigWithSource("", ""); err == nil || GetExitCode(err) != ExitContextMismatch {
				t.Fatalf("environment resolver error = %v, want context mismatch", err)
			}
			if _, found, err := resolveDiscoveredBaseRoot(); err == nil || found || GetExitCode(err) != ExitContextMismatch {
				t.Fatalf("base resolver found=%v err=%v, want context mismatch", found, err)
			}
			stdout, _, err := captureEnvOutput(t, func() error {
				return runEnv([]string{"--session", "session1", "--me", "codex-lead"})
			})
			if err == nil || GetExitCode(err) != ExitContextMismatch || stdout != "" {
				t.Fatalf("runEnv stdout=%q err=%v, want empty stdout and exit 5", stdout, err)
			}
			if _, err := os.Stat(filepath.Join(ancestorRoot, "session1")); !os.IsNotExist(err) {
				t.Fatalf("ancestor queue was mutated: %v", err)
			}
		})
	}
}

func TestDiscoveryAtGitTopAndAbovePlainDirectory(t *testing.T) {
	for _, insideGit := range []bool{false, true} {
		for _, source := range []string{".amqrc", ".agent-mail"} {
			name := fmt.Sprintf("inside_git=%v/%s", insideGit, source)
			t.Run(name, func(t *testing.T) {
				fakeHome := t.TempDir()
				top := filepath.Join(t.TempDir(), "top")
				nested := filepath.Join(top, "nested")
				if err := os.MkdirAll(nested, 0o700); err != nil {
					t.Fatal(err)
				}
				if insideGit {
					if err := os.MkdirAll(filepath.Join(top, ".git"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				root := filepath.Join(top, ".agent-mail")
				if source == ".amqrc" {
					if err := os.WriteFile(filepath.Join(top, ".amqrc"), []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HOME", fakeHome)
				setOptionalEnv(t, envRoot, "", false)
				setOptionalEnv(t, envGlobalRoot, "", false)
				t.Chdir(nested)
				resetAmqrcCache()
				t.Cleanup(resetAmqrcCache)

				resolved, sourceKind, _, err := resolveEnvConfigWithSource("", "")
				if err != nil {
					t.Fatal(err)
				}
				expectSamePath(t, resolveRoot(resolved), root)
				wantSource := rootSourceAutoDetect
				if source == ".amqrc" {
					wantSource = rootSourceProjectRC
				}
				if sourceKind != wantSource {
					t.Fatalf("source = %q, want %q", sourceKind, wantSource)
				}
			})
		}
	}
}

func TestGitTopAtHomeIsIncludedInLocalDiscovery(t *testing.T) {
	for _, source := range []string{".amqrc", ".agent-mail"} {
		t.Run(source, func(t *testing.T) {
			home := t.TempDir()
			nested := filepath.Join(home, "nested", "deeper")
			if err := os.MkdirAll(filepath.Join(home, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(home, ".agent-mail")
			if source == ".amqrc" {
				if err := os.WriteFile(filepath.Join(home, ".amqrc"), []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			for _, key := range []string{envRoot, envBaseRoot, envSession, envRootID, envBaseRootID, envMe, envGlobalRoot} {
				setOptionalEnv(t, key, "", false)
			}
			t.Chdir(nested)
			resetAmqrcCache()

			resolved, sourceKind, _, err := resolveEnvConfigWithSource("", "")
			if err != nil {
				t.Fatalf("environment resolver: %v", err)
			}
			expectSamePath(t, resolveRoot(resolved), root)
			wantSource := rootSourceAutoDetect
			if source == ".amqrc" {
				wantSource = rootSourceProjectRC
			}
			if sourceKind != wantSource {
				t.Fatalf("source = %q, want %q", sourceKind, wantSource)
			}
			discovered, found, err := resolveDiscoveredBaseRoot()
			if err != nil || !found {
				t.Fatalf("base resolver root=%q found=%v err=%v", discovered, found, err)
			}
			expectSamePath(t, discovered, root)
		})
	}
}

func TestGitWorktreeLocalRootIgnoresMalformedHomeConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for repository routing")
	}
	fakeHome := t.TempDir()
	repo := filepath.Join(fakeHome, "workspace", "repo")
	localRoot := filepath.Join(repo, ".agent-mail")
	if err := os.MkdirAll(localRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitForTest(t, repo, "init")
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	setOptionalEnv(t, envRoot, "", false)
	setOptionalEnv(t, envGlobalRoot, "", false)
	t.Chdir(repo)
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		t.Fatalf("local root with ineligible malformed home config: %v", err)
	}
	expectSamePath(t, resolveRoot(root), localRoot)
	if source != rootSourceAutoDetect {
		t.Fatalf("source = %q, want %q", source, rootSourceAutoDetect)
	}
}

func TestAMQGlobalRootWinsOverRepoLocalAutoDetect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for repository routing")
	}
	repo := t.TempDir()
	localRoot := filepath.Join(repo, ".agent-mail")
	explicitRoot := filepath.Join(t.TempDir(), "shared")
	for _, root := range []string{localRoot, explicitRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runGitForTest(t, repo, "init")
	t.Setenv(envGlobalRoot, explicitRoot)
	setOptionalEnv(t, envRoot, "", false)
	t.Chdir(repo)
	resetAmqrcCache()
	t.Cleanup(resetAmqrcCache)

	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		t.Fatal(err)
	}
	expectSamePath(t, root, explicitRoot)
	if source != rootSourceGlobalEnv {
		t.Fatalf("source = %q, want %q", source, rootSourceGlobalEnv)
	}
	discovered, found, err := resolveDiscoveredBaseRoot()
	if err != nil || !found {
		t.Fatalf("discovered root = %q, found=%v, err=%v", discovered, found, err)
	}
	expectSamePath(t, discovered, explicitRoot)
}

func TestStatusPreservingEnvShellFormStopsBeforeSend(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "sent")
	script := `
amq() {
  if [ "$1" = env ]; then
    return 5
  fi
  : > "$AMQ_TEST_SEND_MARKER"
}
amq_context="$(amq env --session session1 --me codex-lead)" &&
eval "$amq_context" &&
amq send --to peer --body wrong-root
`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "AMQ_TEST_SEND_MARKER="+marker)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != ExitContextMismatch {
		t.Fatalf("status-preserving shell exit = %v, want %d", err, ExitContextMismatch)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("send continued after env failure: %v", err)
	}
}

func TestHomeConfigAndAgentMailAreGlobalNotProjectLocal(t *testing.T) {
	fakeHome := t.TempDir()
	projectDir := filepath.Join(fakeHome, "workspace", "plain-repo")
	homeAgentMail := filepath.Join(fakeHome, defaultCoopRoot)
	globalRCRoot := filepath.Join(fakeHome, "configured-global-root")
	for _, dir := range []string{projectDir, homeAgentMail, globalRCRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	rcData, err := json.Marshal(map[string]string{"root": globalRCRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), rcData, 0o600); err != nil {
		t.Fatalf("write global .amqrc: %v", err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	t.Setenv("HOME", fakeHome)
	t.Setenv(envRoot, "")
	t.Setenv(envGlobalRoot, "")
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	if _, err := findAndLoadAmqrc(); !errors.Is(err, errAmqrcNotFound) {
		t.Fatalf("project config discovery reached HOME/.amqrc: %v", err)
	}
	if got := detectAgentMailDir(); got != "" {
		t.Fatalf("repo-local auto-detection reached HOME/.agent-mail: %q", got)
	}
	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfigWithSource: %v", err)
	}
	expectSamePath(t, root, globalRCRoot)
	if source != rootSourceGlobalRC {
		t.Fatalf("root source = %q, want %q", source, rootSourceGlobalRC)
	}
}

func TestRepoLocalAgentMailWinsOverGlobalAmqrcFallback(t *testing.T) {
	fakeHome := t.TempDir()
	projectDir := filepath.Join(fakeHome, "workspace", "snagline")
	localRoot := filepath.Join(projectDir, ".agent-mail")
	gitDir := filepath.Join(projectDir, ".git")
	globalRoot := filepath.Join(fakeHome, "global-agent-mail")
	for _, dir := range []string{localRoot, gitDir, globalRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(fakeHome, ".amqrc"),
		[]byte(fmt.Sprintf(`{"root":%q}`, globalRoot)),
		0o600,
	); err != nil {
		t.Fatalf("write global .amqrc: %v", err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWd)
		resetAmqrcCache()
	})

	t.Setenv("HOME", fakeHome)
	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", "")
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfigWithSource: %v", err)
	}
	expectSamePath(t, resolveRoot(root), localRoot)
	if source != rootSourceAutoDetect {
		t.Fatalf("root source = %q, want %q", source, rootSourceAutoDetect)
	}
}

func TestProjectAmqrcWinsOverGlobalAmqrc(t *testing.T) {
	// Both project .amqrc and global ~/.amqrc exist -> project wins
	projectDir := t.TempDir()
	fakeHome := t.TempDir()

	// Write project .amqrc
	projRC := `{"root": "/project/root"}`
	if err := os.WriteFile(filepath.Join(projectDir, ".amqrc"), []byte(projRC), 0o644); err != nil {
		t.Fatalf("write project .amqrc: %v", err)
	}

	// Write global ~/.amqrc
	globalRC := `{"root": "/global/root"}`
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(globalRC), 0o644); err != nil {
		t.Fatalf("write ~/.amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("HOME", fakeHome)
	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")
	_ = os.Unsetenv("AMQ_GLOBAL_ROOT")

	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfigWithSource: %v", err)
	}
	if root != "/project/root" {
		t.Errorf("expected root=/project/root, got %q", root)
	}
	if source != rootSourceProjectRC {
		t.Errorf("expected source=project_amqrc, got %q", source)
	}
}

func TestAMQGlobalRootEnvWinsOverGlobalAmqrc(t *testing.T) {
	// AMQ_GLOBAL_ROOT env var takes precedence over ~/.amqrc
	projectDir := t.TempDir()
	fakeHome := t.TempDir()

	// Write global ~/.amqrc (lower precedence)
	globalRC := `{"root": "/global-rc/root"}`
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(globalRC), 0o644); err != nil {
		t.Fatalf("write ~/.amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("HOME", fakeHome)
	t.Setenv("AMQ_GLOBAL_ROOT", "/global-env/root")
	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfigWithSource: %v", err)
	}
	if root != "/global-env/root" {
		t.Errorf("expected root=/global-env/root, got %q", root)
	}
	if source != rootSourceGlobalEnv {
		t.Errorf("expected source=global_env, got %q", source)
	}
}
