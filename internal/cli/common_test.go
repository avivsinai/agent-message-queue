package cli

import "testing"

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
