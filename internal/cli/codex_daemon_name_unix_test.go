//go:build darwin || linux

package cli

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// coop exec must not start a Codex daemon thread where the TUI start would
// differ from a plain Codex start.
//   - Review of #950 r5 (Pro, P1 trust): thread/start records trust for a
//     directory with no trust decision, before the TUI asks the user.
//   - Review of #950 r6 (Pro, P1): Codex trims git pointers with trim_ascii;
//     a pointer ending in U+00A0 resolves to nothing in Codex.
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

// Bead agent-message-queue-38l: options that keep Codex on the managed daemon
// and take effect on resume (-m, -a, -s, --yolo, --add-dir, -C) still name
// the session, and -C sets the thread directory. Options that make Codex run
// its own app-server, or that resume refuses, keep the original path.
func TestCodexDaemonThreadDir(t *testing.T) {
	wd := t.TempDir()
	sub := filepath.Join(wd, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		dir  string
		ok   bool
	}{
		{nil, wd, true},
		{[]string{"-m", "gpt-5", "-a", "on-request", "--sandbox=workspace-write", "--add-dir", "/x", "--add-dir", "/y", "--no-alt-screen"}, wd, true},
		{[]string{"--yolo", "-m", "gpt-5"}, wd, true},
		{[]string{"-C", "sub"}, sub, true},
		{[]string{"--cd=" + sub}, sub, true},
		// Review of #975 (Pro, P2): Codex refuses these at argument parsing,
		// after AMQ would have started and named a thread.
		{[]string{"--yolo", "-a", "on-request"}, "", false},
		{[]string{"-a", "untrusted"}, "", false},
		{[]string{"-m", "a", "-m", "b"}, "", false},
		{[]string{"-C", "missing"}, "", false},
		{[]string{"-p", "work"}, "", false},
		{[]string{"-c", "model=x"}, "", false},
		{[]string{"--search"}, "", false},
		{[]string{"--worktree"}, "", false},
		{[]string{"fix the build"}, "", false},
		{[]string{"-m"}, "", false},
	} {
		dir, ok := codexDaemonThreadDir(tc.args, wd)
		if dir != tc.dir || ok != tc.ok {
			t.Errorf("codexDaemonThreadDir(%q) = %q, %v; want %q, %v", tc.args, dir, ok, tc.dir, tc.ok)
		}
	}
}

// AMQ reads Codex's effective features through `codex features list` and
// takes the daemon path only where the TUI runs on the daemon.
//   - Review of #950 r6 (Pro, P2): with terminal_visualization_instructions
//     on, the TUI resumes with developer instructions.
//   - Review of #975 r2 (Pro, P1): with bedrock_setup_wizard on, a
//     signed-out TUI runs embedded even when a daemon runs.
//   - Bead agent-message-queue-38l: with no daemon running and auto-start on,
//     the TUI starts one, where the rollout path can never find its thread;
//     AMQ reports that at once and never starts the daemon itself (review of
//     #975 r1-r2, Pro P1: `codex app-server daemon start` replaces saved
//     feature overrides).
func TestCodexDaemonNamingFollowsCodexFeatures(t *testing.T) {
	const off = "terminal_visualization_instructions x false\nbedrock_setup_wizard x false\n"
	for name, tc := range map[string]struct {
		features string
		daemon   bool
		done     bool
	}{
		"terminal instructions on":  {"daemon_auto_start x true\nbedrock_setup_wizard x false\nterminal_visualization_instructions x true\n", true, false},
		"bedrock wizard on":         {"daemon_auto_start x true\nterminal_visualization_instructions x false\nbedrock_setup_wizard x true\n", true, false},
		"no daemon, auto-start on":  {"daemon_auto_start x true\n" + off, false, true},
		"no daemon, auto-start off": {"daemon_auto_start x false\n" + off, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			codexHome, contacts := t.TempDir(), new(atomic.Int32)
			if tc.daemon {
				codexHome, contacts = startFakeCodexDaemon(t)
			}
			repo := t.TempDir()
			writeCodexConfig(t, codexHome, "[projects.\""+repo+"\"]\ntrust_level = \"trusted\"\n")
			marker := filepath.Join(t.TempDir(), "daemon-start")
			bin := filepath.Join(t.TempDir(), "codex")
			script := "#!/bin/sh\ncase \"$1 $2\" in\n\"features list\") printf '" + strings.ReplaceAll(tc.features, "\n", "\\n") + "' ;;\n*) : > " + marker + " ;;\nesac\n"
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", codexHome)
			t.Chdir(repo)
			args, done := startCodexOnNamedDaemonThread(bin, []string{"-m", "x"}, "s1/codex")
			if done != tc.done || contacts.Load() != 0 || done && !slices.Equal(args, []string{"-m", "x"}) {
				t.Fatalf("done = %v args = %q, daemon contacts = %d; want done %v, unchanged args, no contact", done, args, contacts.Load(), tc.done)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("AMQ ran a Codex command other than features list")
			}
		})
	}
}
