package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
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

func TestInstallRemoteTargetSetsEnv(t *testing.T) {
	home, _, _ := pinInstallShell(t)
	if code := run([]string{"install", "--remote-target", "phone1"}); code != 0 {
		t.Fatalf("install exit %d", code)
	}
	harness := readHarness(t, harnessPath(home, "amq_phone1.json"))
	if harness.Env["AMQ_ACP_REMOTE_TARGET"] != "phone1" || harness.Env["AMQ_ACP_TO"] != "" {
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
