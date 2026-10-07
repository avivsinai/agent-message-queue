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
	"time"
)

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

// coop exec names a Codex TUI on the managed daemon only where the resumed
// TUI starts like a plain launch, runs the rollout watcher only for an
// embedded Codex (it cannot see a daemon thread), and otherwise says once why
// the session is not named.
//   - Review of #950 r5 (Pro, P1 trust): thread/start records trust for a
//     directory with no trust decision, before the TUI asks the user.
//   - Review of #950 r6 (Pro, P1): Codex trims git pointers with trim_ascii;
//     a pointer ending in U+00A0 resolves to nothing in Codex.
//   - Review of #950 r6 (Pro, P2): with terminal_visualization_instructions
//     on, the TUI resumes with developer instructions.
//   - Bead agent-message-queue-38l: with no daemon running and auto-start on,
//     the TUI starts one; AMQ never starts it itself (review of #975 r1-r2,
//     Pro P1: `codex app-server daemon start` replaces saved overrides).
//   - Review of #975 r2-r3 (Pro): bedrock_setup_wizard makes the backend
//     depend on sign-in, and a daemon that refuses the thread still runs
//     the TUI; neither may start the watcher.
//   - Review of #975 r4 (Pro): a command-line override of a gating feature,
//     --remote, a failed feature probe, and an option after "--" (a prompt)
//     do not establish an embedded Codex.
//   - Review of #975 r5 (Pro): an embedded option before --remote, an
//     exclusion env var with --remote, and an allowed key with a commented
//     TOML boolean do not establish an embedded Codex either.
//   - Review of #975 r6 (Pro): a subcommand such as agents is not a prompt.
func TestCodexNamingFollowsWhereCodexRuns(t *testing.T) {
	const plain = "daemon_auto_start x true\nbedrock_setup_wizard x false\nterminal_visualization_instructions x false\n"
	for name, tc := range map[string]struct {
		features string
		daemon   bool
		args     []string
		repo     func(t *testing.T, codexHome string) string
		watcher  bool
	}{
		"untrusted repo": {plain, true, nil, func(t *testing.T, codexHome string) string {
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			return repo
		}, false},
		"worktree pointer with a non-ASCII space": {plain, true, nil, func(t *testing.T, codexHome string) string {
			main, worktree := makeLinkedWorktree(t, "\u00a0")
			writeCodexConfig(t, codexHome, "[projects.\""+main+"\"]\ntrust_level = \"trusted\"\n")
			return worktree
		}, false},
		"terminal instructions on":               {"daemon_auto_start x true\nbedrock_setup_wizard x false\nterminal_visualization_instructions x true\n", true, nil, nil, false},
		"bedrock wizard on":                      {"daemon_auto_start x true\nbedrock_setup_wizard x true\nterminal_visualization_instructions x false\n", true, nil, nil, false},
		"daemon refuses the thread":              {plain, true, nil, nil, false},
		"no daemon, auto-start on":               {plain, false, nil, nil, false},
		"no daemon, auto-start off":              {"daemon_auto_start x false\nbedrock_setup_wizard x false\n", false, nil, nil, true},
		"embedded by a -c override":              {plain, true, []string{"-c", "approvals_reviewer=user"}, nil, true},
		"auto-start enabled on the command line": {"daemon_auto_start x false\nbedrock_setup_wizard x false\n", false, []string{"--enable", "daemon_auto_start"}, nil, false},
		"remote app-server":                      {plain, true, []string{"--remote", "ws://127.0.0.1:1"}, nil, false},
		"feature probe fails":                    {"exit 1", true, nil, nil, false},
		"option text after --":                   {plain, true, []string{"--", "--no-daemon"}, nil, false},
		"search then remote":                     {plain, true, []string{"--search", "--remote", "ws://127.0.0.1:1"}, nil, false},
		"exec server env then remote":            {plain, true, []string{"--remote", "ws://127.0.0.1:1"}, nil, false},
		"agents subcommand":                      {plain, true, []string{"--search", "agents"}, nil, false},
		"commented TOML boolean":                 {plain, true, []string{"-c", "suppress_unstable_features_warning=true # quiet"}, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			codexHome, contacts := t.TempDir(), new(atomic.Int32)
			if tc.daemon {
				codexHome, contacts = startFakeCodexDaemon(t)
			}
			repo := t.TempDir()
			if tc.repo != nil {
				repo = tc.repo(t, codexHome)
			} else {
				writeCodexConfig(t, codexHome, "[projects.\""+repo+"\"]\ntrust_level = \"trusted\"\n")
			}
			bin := filepath.Join(t.TempDir(), "codex")
			script := "#!/bin/sh\n[ \"$1 $2\" = \"features list\" ] && printf '" + strings.ReplaceAll(tc.features, "\n", "\\n") + "'\n"
			if tc.features == "exit 1" {
				script = "#!/bin/sh\nexit 1\n"
			}
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", codexHome)
			if name == "exec server env then remote" {
				t.Setenv("CODEX_EXEC_SERVER_URL", "none")
			}
			t.Chdir(repo)
			watchers := 0
			original := startCoopNamedTUIInjector
			startCoopNamedTUIInjector = func(string, string, time.Time) error { watchers++; return nil }
			t.Cleanup(func() { startCoopNamedTUIInjector = original })
			var args []string
			_, stderr, err := captureEnvOutput(t, func() error {
				var err error
				args, err = applyCoopNamedBeforeExecAt(coopNamedChoice{enabled: true}, bin, tc.args, "s1/codex", time.Now())
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			reminders := strings.Count(stderr, "enter \"/rename s1/codex\" manually")
			wantContact := name == "daemon refuses the thread"
			if !slices.Equal(args, tc.args) || (watchers == 1) != tc.watcher || watchers > 1 || (reminders == 1) == tc.watcher || reminders > 1 || (contacts.Load() > 0) != wantContact {
				t.Fatalf("args = %q, watchers = %d, reminders = %d, daemon contacts = %d; want unchanged args, watcher %v, one reminder otherwise, contact %v\n%s",
					args, watchers, reminders, contacts.Load(), tc.watcher, wantContact, stderr)
			}
		})
	}
}
