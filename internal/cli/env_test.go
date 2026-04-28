package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

	resolvedGot, _ := filepath.EvalSymlinks(got)
	resolvedWant, _ := filepath.EvalSymlinks(want)
	if resolvedGot != resolvedWant {
		t.Errorf("expected path %q, got %q", resolvedWant, resolvedGot)
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

	// Root should be the literal .amqrc root
	expectedRoot := filepath.Join(root, ".agent-mail")
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

	// Root should be resolved relative to .amqrc location (literal)
	expectedRoot := filepath.Join(root, ".agent-mail")
	resolvedRoot, _ := filepath.EvalSymlinks(rootVal)
	expectedResolved, _ := filepath.EvalSymlinks(expectedRoot)
	if resolvedRoot != expectedResolved {
		t.Errorf("expected root=%q (relative to .amqrc), got %q", expectedResolved, resolvedRoot)
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

	// Auto-detect finds .agent-mail (literal, no session appended)
	if rootVal != ".agent-mail" {
		t.Errorf("expected root=.agent-mail, got %q", rootVal)
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

func TestRunEnvInvalidGlobalAmqrcBeatsAutoDetect(t *testing.T) {
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

	_, err := captureEnvStdout(t, func() error {
		return runEnv([]string{"--json"})
	})
	if err == nil {
		t.Fatal("expected invalid global ~/.amqrc to fail before auto-detect")
	}
	if !strings.Contains(err.Error(), "invalid ~/.amqrc") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunEnvJSONV1SessionFlag(t *testing.T) {
	root := t.TempDir()

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

	if result.Root != ".agent-mail" {
		t.Errorf("expected root=.agent-mail, got %q", result.Root)
	}
	if result.BaseRoot != ".agent-mail" {
		t.Errorf("expected base_root=.agent-mail, got %q", result.BaseRoot)
	}
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

	if !strings.Contains(output, "set -gx AM_ROOT /tmp/test-root\n") {
		t.Errorf("expected set -gx AM_ROOT /tmp/test-root, got: %s", output)
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
	resolvedDir, _ := filepath.EvalSymlinks(result.Dir)
	resolvedHome, _ := filepath.EvalSymlinks(fakeHome)
	if resolvedDir != resolvedHome {
		t.Errorf("expected Dir=%q, got %q", resolvedHome, resolvedDir)
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
