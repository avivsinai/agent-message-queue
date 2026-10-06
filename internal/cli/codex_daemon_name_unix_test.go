//go:build darwin || linux

package cli

import (
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// coop exec must not start a Codex daemon thread where the TUI start would
// differ from a plain Codex start.
//   - Review of #950 r5 (Pro, P1 trust): thread/start records trust for a
//     directory with no trust decision, before the TUI asks the user.
//   - Review of #950 r6 (Pro, P1): Codex trims git pointers with trim_ascii;
//     a pointer ending in U+00A0 resolves to nothing in Codex.
//   - Review of #950 r6 (Pro, P2): with terminal_visualization_instructions
//     on, the TUI resumes with developer instructions.
func TestCodexDaemonNamingSkipsWhereCodexWouldDiffer(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, codexHome string) string{
		"unfamiliar repo": func(t *testing.T, codexHome string) string {
			writeCodexConfig(t, codexHome, "model = \"x\"\n")
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			return repo
		},
		"worktree pointer with a non-ASCII space": func(t *testing.T, codexHome string) string {
			main, worktree := makeLinkedWorktree(t, " ")
			writeCodexConfig(t, codexHome, "[projects.\""+main+"\"]\ntrust_level = \"trusted\"\n")
			return worktree
		},
		"terminal instructions feature on": func(t *testing.T, codexHome string) string {
			repo := t.TempDir()
			writeCodexConfig(t, codexHome, "[features]\nterminal_visualization_instructions = true\n\n[projects.\""+repo+"\"]\ntrust_level = \"trusted\"\n")
			return repo
		},
	} {
		t.Run(name, func(t *testing.T) {
			codexHome, contacts := startFakeCodexDaemon(t)
			dir := setup(t, codexHome)
			t.Setenv("CODEX_HOME", codexHome)
			t.Chdir(dir)
			if _, ok := startCodexOnNamedDaemonThread("codex", nil, "s1/codex"); ok || contacts.Load() != 0 {
				t.Fatalf("daemon path taken = %v, daemon contacts = %d; want neither", ok, contacts.Load())
			}
		})
	}
}

// A linked worktree takes its trust from the main checkout, as Codex does.
func TestCodexTrustsALinkedWorktreeOfATrustedCheckout(t *testing.T) {
	codexHome := t.TempDir()
	main, worktree := makeLinkedWorktree(t, "")
	writeCodexConfig(t, codexHome, "[projects.\""+main+"\"]\ntrust_level = \"trusted\"\n")
	if !codexTrustsDir(codexHome, worktree) {
		t.Fatal("worktree of a trusted checkout reads as untrusted")
	}
}

// startFakeCodexDaemon listens on the daemon control socket of a new Codex
// home and counts connections.
func startFakeCodexDaemon(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	codexHome, err := os.MkdirTemp("", "cx") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(codexHome) })
	controlDir := filepath.Join(codexHome, "app-server-control")
	if err := os.MkdirAll(controlDir, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(controlDir, "app-server-control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	contacts := new(atomic.Int32)
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
	return codexHome, contacts
}

func writeCodexConfig(t *testing.T, codexHome, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// makeLinkedWorktree lays out a main checkout and a linked worktree whose
// .git pointer ends with pointerSuffix.
func makeLinkedWorktree(t *testing.T, pointerSuffix string) (main, worktree string) {
	t.Helper()
	base := t.TempDir()
	main, worktree = filepath.Join(base, "main"), filepath.Join(base, "wt")
	gitDir := filepath.Join(main, ".git", "worktrees", "wt")
	for _, dir := range []string{gitDir, worktree} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(main, ".git", "HEAD"): "ref: refs/heads/main\n",
		filepath.Join(gitDir, "HEAD"):       "ref: refs/heads/wt\n",
		filepath.Join(gitDir, "gitdir"):     filepath.Join(worktree, ".git") + "\n",
		filepath.Join(gitDir, "commondir"):  "../..\n",
		filepath.Join(worktree, ".git"):     "gitdir: " + gitDir + pointerSuffix + "\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return main, worktree
}
