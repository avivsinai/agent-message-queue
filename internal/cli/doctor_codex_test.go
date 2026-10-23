package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bead agent-message-queue-vd4.1: Codex applies include_only after
// shell_environment_policy.set, so a list without AM_* removes the identity
// coop exec gives a Codex seat. doctor names the variables it drops.
func TestDoctorWarnsWhenCodexIncludeOnlyDropsTheIdentity(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	config := "[shell_environment_policy]\ninherit = \"core\"\ninclude_only = [\"PATH\", \"am_me\"]\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	check, ok := checkCodexShellEnvironment()
	if !ok || check.Status != "warn" || !strings.Contains(check.Message, "AM_ROOT, AM_BASE_ROOT, AM_SESSION, AM_ROOT_ID, AM_BASE_ROOT_ID from") {
		t.Fatalf("check = %#v, %v", check, ok)
	}
}
