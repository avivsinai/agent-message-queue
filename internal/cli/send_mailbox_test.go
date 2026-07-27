package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestSendCompletesConfiguredDestinationMailboxBeforeDelivery(t *testing.T) {
	root := initializedSendMailboxRoot(t, "claude", "codex")
	if err := os.RemoveAll(filepath.Join(root, "agents", "claude")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "claude",
			"--subject", "configured",
			"--body", "hello",
		})
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	assertCompleteSendMailbox(t, root, "claude")

	listed, _, err := captureEnvOutput(t, func() error {
		return runList([]string{"--root", root, "--me", "claude", "--new"})
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(listed, "configured") {
		t.Fatalf("list output missing delivered subject: %q", listed)
	}

	drained, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", root, "--me", "claude", "--include-body"})
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !strings.Contains(drained, "hello") {
		t.Fatalf("drain output missing delivered body: %q", drained)
	}
}

func TestSendCompletesUnknownDestinationMailboxInInitializedRoot(t *testing.T) {
	root := initializedSendMailboxRoot(t, "claude", "codex")

	_, stderr, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "clade",
			"--subject", "unknown",
			"--body", "hello",
		})
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(stderr, `warning: handle "clade" not in config.json`) {
		t.Fatalf("unknown-handle warning changed: %q", stderr)
	}
	assertCompleteSendMailbox(t, root, "clade")

	listed, _, err := captureEnvOutput(t, func() error {
		return runList([]string{"--root", root, "--me", "clade", "--new"})
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(listed, "unknown") {
		t.Fatalf("list output missing delivered subject: %q", listed)
	}

	if _, _, err := captureEnvOutput(t, func() error {
		return runDrain([]string{"--root", root, "--me", "clade", "--include-body"})
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

func TestSendStrictUnknownDestinationDoesNotCreateMailbox(t *testing.T) {
	root := initializedSendMailboxRoot(t, "claude", "codex")

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "clade",
			"--strict",
			"--body", "hello",
		})
	})
	if err == nil || !strings.Contains(err.Error(), `handle "clade" not in config.json`) {
		t.Fatalf("strict send error = %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "agents", "clade")); !os.IsNotExist(statErr) {
		t.Fatalf("strict unknown send created a mailbox: %v", statErr)
	}
}

func TestSendRefusesUninitializedRootWithoutWriting(t *testing.T) {
	root := t.TempDir()
	clearSendMailboxTestEnv(t)

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "claude",
			"--body", "hello",
		})
	})
	if GetExitCode(err) != ExitNotFound {
		t.Fatalf("send error = %v (exit %d), want not-found exit %d", err, GetExitCode(err), ExitNotFound)
	}
	for _, remedy := range []string{"amq init", "amq coop init"} {
		if !strings.Contains(err.Error(), remedy) {
			t.Fatalf("uninitialized-root error missing %q: %v", remedy, err)
		}
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("uninitialized-root send wrote entries: %#v", entries)
	}
}

func TestSendConfiguredMailboxRepairRefusesSymlinkWithoutDelivery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform privileges")
	}
	root := initializedSendMailboxRoot(t, "claude", "codex")
	cur := fsq.AgentInboxCur(root, "claude")
	if err := os.Remove(cur); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, cur); err != nil {
		t.Fatal(err)
	}

	_, _, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "claude",
			"--body", "must not deliver",
		})
	})
	if err == nil {
		t.Fatal("send through incomplete symlinked mailbox succeeded")
	}
	entries, readErr := os.ReadDir(fsq.AgentInboxNew(root, "claude"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("refused repair delivered messages: %#v", entries)
	}
	info, statErr := os.Lstat(cur)
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("repair changed symlink: info=%v err=%v", info, statErr)
	}
}

func TestSendUnknownMailboxRepairRefusesSymlinkWithoutDelivery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform privileges")
	}
	root := initializedSendMailboxRoot(t, "claude", "codex")
	outside := t.TempDir()
	unknownInbox := filepath.Join(root, "agents", "clade", "inbox")
	if err := os.MkdirAll(filepath.Dir(unknownInbox), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, unknownInbox); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := captureEnvOutput(t, func() error {
		return runSend([]string{
			"--root", root,
			"--me", "codex",
			"--to", "clade",
			"--body", "must not deliver",
		})
	})
	if err == nil {
		t.Fatal("send through unknown symlinked mailbox succeeded")
	}
	if !strings.Contains(stderr, `warning: handle "clade" not in config.json`) {
		t.Fatalf("unknown-handle warning changed: %q", stderr)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("refused repair wrote through symlink: %#v", entries)
	}
	info, statErr := os.Lstat(unknownInbox)
	if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("repair changed symlink: info=%v err=%v", info, statErr)
	}
}

func initializedSendMailboxRoot(t *testing.T, agents ...string) string {
	t.Helper()
	clearSendMailboxTestEnv(t)
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			t.Fatal(err)
		}
	}
	configureSendTestRoot(t, root, agents...)
	return root
}

func configureSendTestRoot(t *testing.T, root string, agents ...string) {
	t.Helper()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "meta", "config.json")
	cfg := config.Config{Version: 1}
	if existing, err := config.LoadConfig(configPath); err == nil {
		cfg = existing
	} else if !os.IsNotExist(err) {
		t.Fatalf("LoadConfig(%s): %v", configPath, err)
	}
	seen := make(map[string]bool, len(cfg.Agents)+len(agents))
	for _, agent := range cfg.Agents {
		seen[agent] = true
	}
	for _, agent := range agents {
		if !seen[agent] {
			seen[agent] = true
			cfg.Agents = append(cfg.Agents, agent)
		}
	}
	if err := config.WriteConfig(configPath, cfg, true); err != nil {
		t.Fatal(err)
	}
}

func assertCompleteSendMailbox(t *testing.T, root, agent string) {
	t.Helper()
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		path := fsq.AgentMailboxPath(root, agent, leaf)
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Fatalf("required mailbox path is not a directory: %s info=%v err=%v", path, info, err)
		}
	}
}

func clearSendMailboxTestEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		envRoot,
		envMe,
		envBaseRoot,
		envSession,
		envGlobalRoot,
		envRootID,
		envBaseRootID,
	} {
		setOptionalEnv(t, key, "", false)
	}
}
