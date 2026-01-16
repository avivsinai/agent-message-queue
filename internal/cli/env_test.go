package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{".agent-mail", ".agent-mail"},
		{"path/to/dir", "path/to/dir"},
		{"has space", "'has space'"},
		{"has'quote", "'has'\"'\"'quote'"},
		{"$VAR", "'$VAR'"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shellQuote(tt.input)
			if got != tt.expected {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.expected)
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
	rcContent := `{"root": ".agent-mail", "me": "claude"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	// Change to subdir and try to find .amqrc
	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	if err := os.Chdir(subdir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	rc, err := findAndLoadAmqrc()
	if err != nil {
		t.Fatalf("findAndLoadAmqrc: %v", err)
	}

	if rc.Root != ".agent-mail" {
		t.Errorf("expected root=.agent-mail, got %q", rc.Root)
	}
	if rc.Me != "claude" {
		t.Errorf("expected me=claude, got %q", rc.Me)
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
	if err == nil {
		t.Error("expected error when .amqrc not found")
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
	rcContent := `{"root": ".agent-mail", "me": "claude"}`
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

	if rootVal != ".agent-mail" {
		t.Errorf("expected root=.agent-mail, got %q", rootVal)
	}
	if meVal != "claude" {
		t.Errorf("expected me=claude, got %q", meVal)
	}
}

func TestResolveEnvConfigFlagOverride(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc with one set of values
	rcContent := `{"root": ".agent-mail", "me": "claude"}`
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
	rcContent := `{"root": ".agent-mail", "me": "claude"}`
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

	if rootVal != ".agent-mail" {
		t.Errorf("expected root=.agent-mail, got %q", rootVal)
	}
	if meVal != "" {
		t.Errorf("expected me=empty (not in .amqrc), got %q", meVal)
	}
}

func TestResolveEnvConfigError(t *testing.T) {
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
	rcContent := `{"root": ".agent-mail", "me": "claude"}`
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

	if result.Root != ".agent-mail" {
		t.Errorf("expected root=.agent-mail, got %q", result.Root)
	}
	if result.Me != "claude" {
		t.Errorf("expected me=claude, got %q", result.Me)
	}
}

func TestRunEnvPosix(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc
	rcContent := `{"root": ".agent-mail", "me": "claude"}`
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

	if !strings.Contains(output, "export AM_ROOT=.agent-mail") {
		t.Errorf("expected export AM_ROOT, got: %s", output)
	}
	if !strings.Contains(output, "export AM_ME=claude") {
		t.Errorf("expected export AM_ME, got: %s", output)
	}
}

func TestRunEnvFish(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc
	rcContent := `{"root": ".agent-mail", "me": "claude"}`
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

	if !strings.Contains(output, "set -gx AM_ROOT .agent-mail") {
		t.Errorf("expected set -gx AM_ROOT, got: %s", output)
	}
	if !strings.Contains(output, "set -gx AM_ME claude") {
		t.Errorf("expected set -gx AM_ME, got: %s", output)
	}
}

func TestRunEnvWake(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc
	rcContent := `{"root": ".agent-mail", "me": "claude"}`
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
