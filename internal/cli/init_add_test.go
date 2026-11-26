package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestInitAddRegistersNewAgentsInExistingRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := captureEnvStdout(t, func() error {
		return runInit([]string{"--root", root, "--agents", "claude,codex"})
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfgPath := filepath.Join(root, "meta", "config.json")
	before, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// A listed handle, or the reserved user, whose mailbox lost a leaf is
	// repaired, not skipped.
	for _, handle := range []string{"codex", reservedHumanHandle} {
		if err := os.RemoveAll(fsq.AgentMailboxPath(root, handle, fsq.MailboxDLQCur)); err != nil {
			t.Fatal(err)
		}
	}

	out, err := captureEnvStdout(t, func() error {
		return runInit([]string{"--root", root, "--agents", "zeta,gemini,codex", "--add"})
	})
	if err != nil {
		t.Fatalf("init --add: %v", err)
	}
	if !strings.Contains(out, "Added zeta,gemini to AMQ root") {
		t.Fatalf("stdout = %q", out)
	}
	after, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude", "codex", "zeta", "gemini"}; !reflect.DeepEqual(after.Agents, want) {
		t.Fatalf("agents = %v, want %v", after.Agents, want)
	}
	if after.Version != before.Version || after.CreatedUTC != before.CreatedUTC {
		t.Fatalf("init --add rewrote version/created_utc: %+v -> %+v", before, after)
	}
	for _, handle := range []string{"zeta", "gemini", "codex", reservedHumanHandle} {
		if issues := inventoryMailboxIssues(root, handle); len(issues) > 0 {
			t.Fatalf("%s mailbox incomplete after init --add: %v", handle, issues)
		}
	}

	out, err = captureEnvStdout(t, func() error {
		return runInit([]string{"--root", root, "--agents", "gemini", "--add"})
	})
	if err != nil || !strings.Contains(out, "already registered") {
		t.Fatalf("repeat init --add = %q, %v", out, err)
	}
}

func TestInitAddRejectsForce(t *testing.T) {
	err := runInit([]string{"--root", t.TempDir(), "--agents", "claude", "--add", "--force"})
	if GetExitCode(err) != ExitUsage {
		t.Fatalf("init --add --force = %v (exit %d), want usage error", err, GetExitCode(err))
	}
}

func TestInitAddRequiresInitializedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	err := runInit([]string{"--root", root, "--agents", "claude", "--add"})
	if GetExitCode(err) != ExitNotFound {
		t.Fatalf("init --add on a missing root = %v (exit %d), want not found", err, GetExitCode(err))
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("init --add created the root: %v", statErr)
	}
}

// Review finding (agy-183 R1 blocker): provisioning through the ambient path
// followed a symlinked mailbox into another agent's tree and then listed the
// handle.
func TestInitAddRefusesSymlinkedMailbox(t *testing.T) {
	root := t.TempDir()
	if _, err := captureEnvStdout(t, func() error {
		return runInit([]string{"--root", root, "--agents", "claude"})
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfgPath := filepath.Join(root, "meta", "config.json")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "agents", "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(root, "agents", "bob")); err != nil {
		t.Fatal(err)
	}

	err = runInit([]string{"--root", root, "--agents", "bob", "--add"})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("init --add through a symlinked mailbox = %v, want a symlink refusal", err)
	}
	if after, _ := os.ReadFile(cfgPath); string(after) != string(before) {
		t.Fatalf("roster changed after a refused provision: %s", after)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatalf("init --add wrote through the symlink: %v", entries)
	}
}
