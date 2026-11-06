package cli

import "testing"

// Review of the shared Claude and Codex directory resolvers: with HOME
// unset they returned nothing although CLAUDE_CONFIG_DIR or CODEX_HOME was
// set; the environment value wins without a home.
func TestHarnessDirsFollowTheEnvironmentWithoutAHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "/claude-config")
	t.Setenv("CODEX_HOME", "/codex-home")
	if got := claudeConfigDir(); got != "/claude-config" {
		t.Errorf("claudeConfigDir() = %q", got)
	}
	if got, err := codexHomeDir(); err != nil || got != "/codex-home" {
		t.Errorf("codexHomeDir() = %q, %v", got, err)
	}
}
