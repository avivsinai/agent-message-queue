package main

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
)

func TestInstallToWritesHarness(t *testing.T) {
	home, root, base := pinInstallShell(t)
	stdout := captureStdout(t, func() int {
		return run([]string{"install", "--to", "claude"})
	})
	if stdout != "In Buzz Desktop: New agent, select AMQ → claude (agent-message-queue)\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	path := harnessPath(home, "amq_claude.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	harness := readHarness(t, path)
	exe, err := currentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if harness.ID != "amq_claude" || harness.Command != exe || len(harness.Args) != 0 {
		t.Fatalf("harness id=%q command=%q args=%#v", harness.ID, harness.Command, harness.Args)
	}
	if harness.Env["AM_ROOT"] != root || harness.Env["AM_BASE_ROOT"] != base ||
		harness.Env["AM_SESSION"] != "session1" || harness.Env["AM_ME"] != "aviv" ||
		harness.Env["AMQ_ACP_TO"] != "claude" || harness.Env["AM_ROOT_ID"] != "root-token" ||
		harness.Env["AMQ_ACP_REMOTE_TARGET"] != "" {
		t.Fatalf("env = %#v", harness.Env)
	}
}

func TestInstallRefusesForeignHarness(t *testing.T) {
	home, _, _ := pinInstallShell(t)
	path := harnessPath(home, "amq_claude.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("{\"id\":\"amq_claude\",\"installHint\":\"hand written\"}\n")
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"install", "--to", "claude"}); code == 0 {
		t.Fatal("install overwrote a harness it did not write")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(foreign) {
		t.Fatalf("foreign harness changed: %q %v", got, err)
	}
}

func TestInstallRemoveDeletesOnlyItsFile(t *testing.T) {
	home, _, _ := pinInstallShell(t)
	if code := run([]string{"install", "--to", "claude"}); code != 0 {
		t.Fatalf("install exit %d", code)
	}
	sibling := harnessPath(home, "amq_other.json")
	if err := os.WriteFile(sibling, []byte("{\"installHint\":\"hand written\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"install", "--remove", "--to", "claude"}); code != 0 {
		t.Fatalf("remove exit %d", code)
	}
	if _, err := os.Stat(harnessPath(home, "amq_claude.json")); !os.IsNotExist(err) {
		t.Fatalf("installed harness still present: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling harness: %v", err)
	}
}

// Claude #877 review 2026-09-23T13-05-33.108Z_pid93850_feca21ef: target ids
// contain a colon. The file token maps it; the environment keeps the raw id.
// Claude #877 2026-09-23T13-14-53.733Z_pid93509_0771f6ac: install pins the
// native session the endpoint reports, and remote mode does not need AM_ME.
func TestInstallRemoteTargetSetsEnv(t *testing.T) {
	home, _, _ := pinInstallShell(t)
	root := shortQueueRoot(t)
	t.Setenv("AM_ME", "")
	const target = "claude:98402"
	const native = "native-98402"
	serveNative(t, root, native, false)
	stdout := captureStdout(t, func() int {
		return run([]string{"install", "--remote-target", target})
	})
	if stdout != "In Buzz Desktop: New agent, select AMQ remote → claude:98402 (agent-message-queue)\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	harness := readHarness(t, harnessPath(home, "amq_claude_98402.json"))
	if harness.ID != "amq_claude_98402" || harness.Label != "AMQ remote → claude:98402 (agent-message-queue)" {
		t.Fatalf("id=%q label=%q", harness.ID, harness.Label)
	}
	if harness.Env["AMQ_ACP_REMOTE_TARGET"] != target || harness.Env["AMQ_ACP_REMOTE_NATIVE_SESSION"] != native ||
		harness.Env["AMQ_ACP_TO"] != "" || harness.Env["AM_ME"] != "" {
		t.Fatalf("env = %#v", harness.Env)
	}
}

// Claude #877 2026-09-23T13-14-53.733Z_pid93509_0771f6ac: an unreachable
// endpoint or an unshared target both tell the owner to start amq-remote.
func TestInstallRemoteRefusesWithoutNativeSession(t *testing.T) {
	pinInstallShell(t)
	root := shortQueueRoot(t)
	code, stderr := captureStderr(t, func() int {
		return run([]string{"install", "--remote-target", "claude:98402"})
	})
	want := "start amq-remote up --root " + root + " first"
	if code == 0 || !strings.Contains(stderr, want) {
		t.Fatalf("unreachable exit %d stderr %q", code, stderr)
	}
	serveNative(t, root, "", true)
	code, stderr = captureStderr(t, func() int {
		return run([]string{"install", "--remote-target", "claude:98402"})
	})
	if code == 0 || !strings.Contains(stderr, want) {
		t.Fatalf("unshared exit %d stderr %q", code, stderr)
	}
}

// Claude #877 review 2026-09-23T13-05-33.108Z_pid93850_feca21ef: the harness
// command is the stable PATH entry, not the versioned Cellar path.
func TestInstallWritesPATHCommand(t *testing.T) {
	home, _, _ := pinInstallShell(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "amq-acp")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if code := run([]string{"install", "--to", "claude"}); code != 0 {
		t.Fatalf("install exit %d", code)
	}
	harness := readHarness(t, harnessPath(home, "amq_claude.json"))
	if harness.Command != link {
		t.Fatalf("command = %q, want PATH entry %s", harness.Command, link)
	}
}

// Claude #877 review 2026-09-23T13-05-33.108Z_pid93850_feca21ef: a queue with
// no session pin can still install.
func TestInstallOmitsAbsentSessionPin(t *testing.T) {
	home, _, _ := pinInstallShell(t)
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_SESSION", "")
	if code := run([]string{"install", "--to", "claude"}); code != 0 {
		t.Fatalf("install exit %d", code)
	}
	harness := readHarness(t, harnessPath(home, "amq_claude.json"))
	if _, ok := harness.Env["AM_BASE_ROOT"]; ok {
		t.Fatalf("wrote empty base root: %#v", harness.Env)
	}
	if _, ok := harness.Env["AM_SESSION"]; ok {
		t.Fatalf("wrote empty session: %#v", harness.Env)
	}
	if harness.Env["AM_ME"] != "aviv" || harness.Env["AMQ_ACP_TO"] != "claude" || harness.Env["AM_ROOT"] == "" {
		t.Fatalf("env = %#v", harness.Env)
	}
}

func pinInstallShell(t *testing.T) (home, root, base string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	base = filepath.Join(t.TempDir(), "agent-message-queue", ".agent-mail")
	root = filepath.Join(base, "session1")
	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_BASE_ROOT", base)
	t.Setenv("AM_SESSION", "session1")
	t.Setenv("AM_ME", "aviv")
	t.Setenv("AM_ROOT_ID", "root-token")
	t.Setenv("AM_BASE_ROOT_ID", "base-token")
	return home, root, base
}

func harnessPath(home, name string) string {
	return filepath.Join(home, "Library", "Application Support", "xyz.block.buzz.app", "custom_harnesses", name)
}

func readHarness(t *testing.T, path string) buzzHarness {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var harness buzzHarness
	if err := json.Unmarshal(raw, &harness); err != nil {
		t.Fatal(err)
	}
	return harness
}

func shortQueueRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "ai")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("AM_ROOT", root)
	return root
}

func serveNative(t *testing.T, root, session string, unshared bool) {
	t.Helper()
	dir := filepath.Join(root, "extensions", "remote")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", ipc.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			body := `{"reply":{"native_session":"` + session + `"}}` + "\n"
			if unshared {
				body = `{"error":{"code":"unshared","message":"target has no native session identity"}}` + "\n"
			}
			_, _ = conn.Write([]byte(body))
			_ = conn.Close()
		}
	}()
}

func captureStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code := fn()
	_ = w.Close()
	os.Stderr = old
	out, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

func captureStdout(t *testing.T, fn func() int) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := fn()
	_ = w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	return string(out)
}
