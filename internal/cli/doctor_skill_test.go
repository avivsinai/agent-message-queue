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
