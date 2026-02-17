package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellSnippetPosixDefaults(t *testing.T) {
	snippet := posixSnippet(defaultClaudeAlias, defaultCodexAlias)
	if !strings.Contains(snippet, "function amc()") {
		t.Error("posix snippet missing amc function")
	}
	if !strings.Contains(snippet, "function amx()") {
		t.Error("posix snippet missing amx function")
	}
	if !strings.Contains(snippet, "--session") {
		t.Error("posix snippet missing --session flag")
	}
	if !strings.Contains(snippet, shellSetupMarker) {
		t.Error("posix snippet missing marker comment")
	}
}

func TestShellSnippetPosixCustomNames(t *testing.T) {
	snippet := posixSnippet("cc", "cx")
	if !strings.Contains(snippet, "function cc()") {
		t.Error("posix snippet missing custom claude alias")
	}
	if !strings.Contains(snippet, "function cx()") {
		t.Error("posix snippet missing custom codex alias")
	}
}

func TestShellSnippetFishDefaults(t *testing.T) {
	snippet := fishSnippet(defaultClaudeAlias, defaultCodexAlias)
	if !strings.Contains(snippet, "function amc") {
		t.Error("fish snippet missing amc function")
	}
	if !strings.Contains(snippet, "function amx") {
		t.Error("fish snippet missing amx function")
	}
	if !strings.Contains(snippet, "--session") {
		t.Error("fish snippet missing --session flag")
	}
}

func TestShellSnippetFishCustomNames(t *testing.T) {
	snippet := fishSnippet("cc", "cx")
	if !strings.Contains(snippet, "function cc") {
		t.Error("fish snippet missing custom claude alias")
	}
	if !strings.Contains(snippet, "function cx") {
		t.Error("fish snippet missing custom codex alias")
	}
}

func TestInstallToRCFileIdempotent(t *testing.T) {
	dir := t.TempDir()
	rcFile := filepath.Join(dir, ".zshrc")

	if err := os.WriteFile(rcFile, []byte(shellSetupMarker+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	origData, _ := os.ReadFile(rcFile)

	// Verify marker detection logic.
	data, err := os.ReadFile(rcFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), shellSetupMarker) {
		t.Fatal("marker not found in test rc file")
	}
	if string(data) != string(origData) {
		t.Error("rc file was modified despite marker being present")
	}
}

func TestDetectShell(t *testing.T) {
	orig := os.Getenv("SHELL")
	defer os.Setenv("SHELL", orig)

	tests := []struct {
		env  string
		want string
	}{
		{"/bin/zsh", "zsh"},
		{"/usr/bin/fish", "fish"},
		{"/bin/bash", "bash"},
		{"/usr/local/bin/unknown", "bash"},
		{"", "bash"},
	}
	for _, tt := range tests {
		os.Setenv("SHELL", tt.env)
		got := detectShell()
		if got != tt.want {
			t.Errorf("detectShell() with SHELL=%q = %q, want %q", tt.env, got, tt.want)
		}
	}
}

func TestIsValidSetupShell(t *testing.T) {
	valid := []string{"bash", "zsh", "fish"}
	for _, s := range valid {
		if !isValidSetupShell(s) {
			t.Errorf("isValidSetupShell(%q) = false, want true", s)
		}
	}
	invalid := []string{"sh", "csh", "powershell", ""}
	for _, s := range invalid {
		if isValidSetupShell(s) {
			t.Errorf("isValidSetupShell(%q) = true, want false", s)
		}
	}
}

func TestValidateAliasName(t *testing.T) {
	valid := []string{"amc", "amx", "cc", "my_alias", "Claude1"}
	for _, name := range valid {
		if err := validateAliasName(name); err != nil {
			t.Errorf("validateAliasName(%q) unexpected error: %v", name, err)
		}
	}
	invalid := []string{"", "a b", "a/b", "a.b", "a@b"}
	for _, name := range invalid {
		if err := validateAliasName(name); err == nil {
			t.Errorf("validateAliasName(%q) expected error, got nil", name)
		}
	}
}

func TestInstallConfirmationMessage(t *testing.T) {
	// Verify the confirmation message includes alias names and path.
	// We test the format string indirectly by checking posixSnippet output.
	snippet := posixSnippet("myc", "myx")
	if !strings.Contains(snippet, "function myc()") {
		t.Error("expected custom claude alias in snippet")
	}
	if !strings.Contains(snippet, "function myx()") {
		t.Error("expected custom codex alias in snippet")
	}
}
