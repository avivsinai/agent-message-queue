package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeHandle(t *testing.T) {
	if got, err := normalizeHandle("codex"); err != nil || got != "codex" {
		t.Fatalf("normalizeHandle valid: %v, %v", got, err)
	}
	if _, err := normalizeHandle("Codex"); err == nil {
		t.Fatalf("expected error for uppercase handle")
	}
	if _, err := normalizeHandle("co/dex"); err == nil {
		t.Fatalf("expected error for invalid characters")
	}
	if got, err := normalizeHandle("codex_1"); err != nil || got != "codex_1" {
		t.Fatalf("normalizeHandle underscore: %v, %v", got, err)
	}
}

func TestValidateKnownHandle(t *testing.T) {
	root := t.TempDir()
	metaDir := filepath.Join(root, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create config with known agents
	cfg := map[string]any{
		"version": 1,
		"agents":  []string{"alice", "bob"},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(metaDir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Known handle should pass
	if err := validateKnownHandles(root, false, "alice"); err != nil {
		t.Errorf("known handle should pass: %v", err)
	}

	// Unknown handle with strict=false should warn but not error
	if err := validateKnownHandles(root, false, "unknown"); err != nil {
		t.Errorf("unknown handle with strict=false should warn, not error: %v", err)
	}

	// Unknown handle with strict=true should error
	if err := validateKnownHandles(root, true, "unknown"); err == nil {
		t.Errorf("unknown handle with strict=true should error")
	}
}

func TestValidateKnownHandleNoConfig(t *testing.T) {
	root := t.TempDir()

	// No config file - should pass any handle
	if err := validateKnownHandles(root, true, "anyhandle"); err != nil {
		t.Errorf("no config should pass any handle: %v", err)
	}
}

func TestValidateKnownHandleCorruptConfig(t *testing.T) {
	root := t.TempDir()
	metaDir := filepath.Join(root, "meta")
	if err := os.MkdirAll(metaDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write invalid JSON
	if err := os.WriteFile(filepath.Join(metaDir, "config.json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write corrupt config: %v", err)
	}

	// Corrupt config with strict=false should warn but not error
	if err := validateKnownHandles(root, false, "alice"); err != nil {
		t.Errorf("corrupt config with strict=false should warn, not error: %v", err)
	}

	// Corrupt config with strict=true should error
	if err := validateKnownHandles(root, true, "alice"); err == nil {
		t.Errorf("corrupt config with strict=true should error")
	}
}

func TestDefaultRootFromAmqrc(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "custom-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write .amqrc
	amqrcData, _ := json.Marshal(map[string]string{"root": "custom-root"})
	if err := os.WriteFile(filepath.Join(base, ".amqrc"), amqrcData, 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
		resetAmqrcCache()
		_ = os.Unsetenv("AM_ROOT")
	})
	_ = os.Unsetenv("AM_ROOT")
	resetAmqrcCache()

	if err := os.Chdir(base); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := defaultRoot()
	want := filepath.Join(base, "custom-root")
	gotEval, _ := filepath.EvalSymlinks(got)
	wantEval, _ := filepath.EvalSymlinks(want)
	if gotEval != wantEval {
		t.Fatalf("defaultRoot() = %q, want %q", got, want)
	}
}

func TestDefaultRootEnvOverridesAmqrc(t *testing.T) {
	base := t.TempDir()

	// Write .amqrc with one root
	amqrcData, _ := json.Marshal(map[string]string{"root": "amqrc-root"})
	if err := os.WriteFile(filepath.Join(base, ".amqrc"), amqrcData, 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
		resetAmqrcCache()
		_ = os.Unsetenv("AM_ROOT")
	})
	resetAmqrcCache()

	if err := os.Chdir(base); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Set AM_ROOT to a different value
	t.Setenv("AM_ROOT", "/env/root")

	got := defaultRoot()
	if got != "/env/root" {
		t.Fatalf("defaultRoot() = %q, want %q (env should override .amqrc)", got, "/env/root")
	}
}

func TestDefaultRootFallbackNoAmqrc(t *testing.T) {
	base := t.TempDir()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
		resetAmqrcCache()
		_ = os.Unsetenv("AM_ROOT")
	})
	_ = os.Unsetenv("AM_ROOT")
	resetAmqrcCache()

	if err := os.Chdir(base); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := defaultRoot()
	if got != ".agent-mail" {
		t.Fatalf("defaultRoot() = %q, want %q (should fall back)", got, ".agent-mail")
	}
}

func TestResolveRootFindsParent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".agent-mail")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	sub := filepath.Join(base, "nested", "dir")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})
	if err := os.Chdir(sub); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := resolveRoot(".agent-mail")
	want, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	gotEval, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("eval got: %v", err)
	}
	wantEval, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("eval want: %v", err)
	}
	if gotEval != wantEval {
		t.Fatalf("resolveRoot parent = %q, want %q", got, want)
	}
}

func TestResolveRootCurrentDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".agent-mail")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})
	if err := os.Chdir(base); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := resolveRoot(".agent-mail")
	want, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	gotEval, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("eval got: %v", err)
	}
	wantEval, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("eval want: %v", err)
	}
	if gotEval != wantEval {
		t.Fatalf("resolveRoot cwd = %q, want %q", got, want)
	}
}
