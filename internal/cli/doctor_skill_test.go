package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckSkillGrokProjectLocalInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	writeSkillFile(t, filepath.Join(".grok", "skills", "amq-cli", "SKILL.md"))

	got := checkSkill("grok")
	if got.Name != "grok skill" || got.Status != "ok" || got.Message != "installed (project-local)" {
		t.Fatalf("grok project-local skill = %#v", got)
	}
}

func TestCheckSkillGrokMissingWarnsNotInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)

	got := checkSkill("grok")
	if got.Status != "warn" || !strings.Contains(got.Message, "not installed") {
		t.Fatalf("missing grok skill = %#v", got)
	}
}

func TestCheckSkillUnknownAgent(t *testing.T) {
	got := checkSkill("not-an-agent")
	if got.Status != "warn" || got.Message != "unknown agent" {
		t.Fatalf("unknown agent skill = %#v", got)
	}
}

func TestCheckSkillGrokDoesNotInheritClaudePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	writeSkillFile(t, filepath.Join(".claude", "skills", "amq-cli", "SKILL.md"))

	got := checkSkill("grok")
	if got.Status != "warn" || !strings.Contains(got.Message, "not installed") {
		t.Fatalf("grok skill with only claude path = %#v", got)
	}
}

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

// TestCheckSkillCodexAbsentEverywhereWarns is the negative: with no skill
// anywhere, doctor must warn and keep the remedy text.
func TestCheckSkillCodexAbsentEverywhereWarns(t *testing.T) {
	home := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("HOME", home)

	got := checkSkill("codex")
	if got.Status != "warn" || !strings.Contains(got.Message, "not installed") ||
		!strings.Contains(got.Message, "npx skills add") {
		t.Fatalf("absent codex skill = %#v; want warn with remedy", got)
	}
}

// TestCheckSkillCodexDirWithoutSkillMdWarns guards the partial-install case:
// a skill directory exists but SKILL.md is missing. Must warn, not report ok.
func TestCheckSkillCodexDirWithoutSkillMdWarns(t *testing.T) {
	home := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("HOME", home)
	// Directory present under the shared user-level path, but no SKILL.md inside.
	if err := os.MkdirAll(filepath.Join(home, ".agents", "skills", "amq-cli"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := checkSkill("codex")
	if got.Status != "warn" || !strings.Contains(got.Message, "SKILL.md missing") {
		t.Fatalf("codex skill dir without SKILL.md = %#v; want warn about missing SKILL.md", got)
	}
}

func TestDoctorJSONIncludesGrokSkillCheck(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "claude")
	findDoctorCheck(t, runDoctorMailboxJSON(t, root).Checks, "grok skill")
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
