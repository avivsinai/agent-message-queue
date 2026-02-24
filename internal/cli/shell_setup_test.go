package cli

import (
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

func TestDetectShell(t *testing.T) {
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
		t.Setenv("SHELL", tt.env)
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
