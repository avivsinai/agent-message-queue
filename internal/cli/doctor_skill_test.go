package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheckSkillCodexFoundUnderSharedAgentsDir is the qtv regression: the
// remedy (`npx skills add avivsinai/agent-message-queue -g -y`) installs to
// ~/.agents/skills/amq-cli, but the old check only looked in ~/.codex/skills,
// so doctor always warned right after running its own remedy. The shared
// ~/.agents/skills candidate must satisfy the codex check.
func TestCheckSkillCodexFoundUnderSharedAgentsDir(t *testing.T) {
	home := t.TempDir()
	// CWD is a separate clean dir with no project-local skills, so only the
	// user-level ~/.agents/skills candidate can satisfy the check.
	t.Chdir(t.TempDir())
	t.Setenv("HOME", home)
	writeSkillFile(t, filepath.Join(home, ".agents", "skills", "amq-cli", "SKILL.md"))

	got := checkSkill("codex")
	if got.Name != "codex skill" || got.Status != "ok" || got.Message != "installed" {
		t.Fatalf("codex skill under ~/.agents/skills = %#v; want status=ok message=installed", got)
	}
}

func writeSkillFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
