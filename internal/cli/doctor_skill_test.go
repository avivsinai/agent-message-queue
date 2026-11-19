package cli

import (
	"os"
	"os/exec"
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
	writeSkillFile(t, filepath.Join(home, ".agents", "skills", "amq-cli", "SKILL.md"), "# skill\n")

	got := checkSkill("codex")
	if got.Name != "codex skill" || got.Status != "ok" || got.Message != "installed" {
		t.Fatalf("codex skill under ~/.agents/skills = %#v; want status=ok message=installed", got)
	}
}

func TestSkillChecksAgyFoundUnderGeminiConfig(t *testing.T) {
	home := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("HOME", home)
	writeSkillFile(t, filepath.Join(home, ".gemini", "config", "skills", "amq-cli", "SKILL.md"), "# skill\n")
	lookPathAgy = func() bool { return true }
	t.Cleanup(func() { lookPathAgy = func() bool { _, err := exec.LookPath("agy"); return err == nil } })

	for _, c := range skillChecks() {
		if c.Name == "agy skill" {
			if c.Status != "ok" {
				t.Fatalf("agy skill = %#v; want ok", c)
			}
			return
		}
	}
	t.Fatal("no agy skill row")
}

// Regression (agent-message-queue-fc67): doctor reported "installed" for
// a user skill at 0.146.1 with the 0.156.0 binary.
func TestCheckSkillWarnsWhenTheSkillIsOlderThanTheBinary(t *testing.T) {
	home := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(home, ".agents", "skills", "amq-cli", "SKILL.md")
	writeSkillFile(t, path, "---\nname: amq-cli\nversion: 0.146.1 # x-release-please-version\n---\n")
	previousVersion := cliVersion
	cliVersion = "0.156.0"
	t.Cleanup(func() { cliVersion = previousVersion })

	want := "installed, but its version 0.146.1 is older than amq 0.156.0 (run: npx skills add avivsinai/agent-message-queue -g -y, then restart the agent)"
	got := checkSkill("claude")
	if got.Status != "warn" || got.Message != want {
		t.Fatalf("claude skill = %#v. Want status warn and message %q.", got, want)
	}
}

func writeSkillFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
