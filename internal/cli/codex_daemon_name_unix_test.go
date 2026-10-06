//go:build darwin || linux

package cli

import (
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Review of #950 r5 (Pro, P1 trust): thread/start on the Codex daemon
// records trust for a directory that has no trust decision, before the TUI
// asks the user. coop exec must not contact the daemon from such a directory.
func TestCodexDaemonNamingSkipsAnUntrustedRepo(t *testing.T) {
	codexHome, err := os.MkdirTemp("", "cx") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(codexHome) })
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	controlDir := filepath.Join(codexHome, "app-server-control")
	if err := os.MkdirAll(controlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(controlDir, "app-server-control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var contacts atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			contacts.Add(1)
			_ = conn.Close()
		}
	}()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	t.Chdir(repo)

	if _, ok := startCodexOnNamedDaemonThread("codex", nil, "s1/codex"); ok || contacts.Load() != 0 {
		t.Fatalf("daemon path taken = %v, daemon contacts = %d; want neither", ok, contacts.Load())
	}
}

// A linked worktree takes its trust from the main checkout, as Codex does.
func TestCodexTrustsALinkedWorktreeOfATrustedCheckout(t *testing.T) {
	codexHome, base := t.TempDir(), t.TempDir()
	main, worktree := filepath.Join(base, "main"), filepath.Join(base, "wt")
	gitDir := filepath.Join(main, ".git", "worktrees", "wt")
	for _, dir := range []string{gitDir, worktree} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(main, ".git", "HEAD"):     "ref: refs/heads/main\n",
		filepath.Join(gitDir, "HEAD"):           "ref: refs/heads/wt\n",
		filepath.Join(gitDir, "gitdir"):         filepath.Join(worktree, ".git") + "\n",
		filepath.Join(gitDir, "commondir"):      "../..\n",
		filepath.Join(worktree, ".git"):         "gitdir: " + gitDir + "\n",
		filepath.Join(codexHome, "config.toml"): "[projects.\"" + main + "\"]\ntrust_level = \"trusted\"\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !codexTrustsDir(codexHome, worktree) {
		t.Fatal("worktree of a trusted checkout reads as untrusted")
	}
}
