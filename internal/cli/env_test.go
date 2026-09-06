package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func expectedPosixIdentityLines(t *testing.T, root, baseRoot string) string {
	t.Helper()
	rootID, rootErr := resolveTreeIdentityToken(root)
	baseRootID, baseErr := resolveTreeIdentityToken(baseRoot)
	var out strings.Builder
	if rootErr == nil {
		fmt.Fprintf(&out, "export AM_ROOT_ID=%s\n", shellQuotePosix(rootID))
	} else {
		out.WriteString("unset AM_ROOT_ID\n")
	}
	if baseErr == nil {
		fmt.Fprintf(&out, "export AM_BASE_ROOT_ID=%s\n", shellQuotePosix(baseRootID))
	} else {
		out.WriteString("unset AM_BASE_ROOT_ID\n")
	}
	return out.String()
}

func captureEnvStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	runErr := fn()

	_ = w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	_ = r.Close()

	return buf.String(), runErr
}

func captureEnvOutput(t *testing.T, fn func() error) (stdout, stderr string, runErr error) {
	t.Helper()

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wOut
	os.Stderr = wErr
	t.Cleanup(func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	})

	runErr = fn()

	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr

	var outBuf bytes.Buffer
	_, _ = outBuf.ReadFrom(rOut)
	_ = rOut.Close()

	var errBuf bytes.Buffer
	_, _ = errBuf.ReadFrom(rErr)
	_ = rErr.Close()

	return outBuf.String(), errBuf.String(), runErr
}

func runEnvJSONForTest(t *testing.T, args ...string) envOutput {
	t.Helper()

	outArgs := append([]string{"--json"}, args...)
	output, err := captureEnvStdout(t, func() error {
		return runEnv(outArgs)
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	var result envOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v, output was: %s", err, output)
	}
	return result
}

func expectSamePath(t *testing.T, got, want string) {
	t.Helper()

	resolvedGot := canonicalTestPath(t, got)
	resolvedWant := canonicalTestPath(t, want)
	if resolvedGot != resolvedWant {
		t.Errorf("expected path %q, got %q", resolvedWant, resolvedGot)
	}
}

// canonicalTestPath resolves symlinks in the longest existing prefix while
// preserving any nonexistent suffix. EvalSymlinks returns an empty result on
// failure, so ignoring its error can make two different missing paths compare
// equal and let a resolution regression pass unnoticed.
func canonicalTestPath(t *testing.T, path string) string {
	t.Helper()

	current, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("absolute path for %q: %v", path, err)
	}
	missing := make([]string, 0, 2)
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		if !os.IsNotExist(err) {
			t.Fatalf("resolve path %q at existing prefix %q: %v", path, current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatalf("resolve path %q: no existing path prefix", path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func setCLIVersionForTest(t *testing.T, version string) {
	t.Helper()

	// This mutates package state; do not use from parallel tests.
	oldVersion := cliVersion
	cliVersion = version
	t.Cleanup(func() { cliVersion = oldVersion })
}

func TestResolveEnvConfigRelativeRootFromSubdir(t *testing.T) {
	// This tests the fix for: relative root should be resolved against .amqrc location,
	// not CWD
	root := t.TempDir()
	subdir := filepath.Join(root, "sub", "deep")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write .amqrc in root with relative path
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	// Change to subdir (different from where .amqrc is)
	if err := os.Chdir(subdir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	rootVal, _, err := resolveEnvConfig("", "")
	if err != nil {
		t.Fatalf("resolveEnvConfig: %v", err)
	}

	// Root should be resolved relative to .amqrc location (literal)
	expectedRoot := filepath.Join(root, ".agent-mail")
	expectSamePath(t, rootVal, expectedRoot)
}

func TestResolveEnvConfigFlagOverridesEnv(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	// Set env vars
	_ = os.Setenv("AM_ROOT", "/env/root")
	_ = os.Setenv("AM_ME", "envagent")
	defer func() { _ = os.Unsetenv("AM_ROOT") }()
	defer func() { _ = os.Unsetenv("AM_ME") }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Flags should override both env and .amqrc
	rootVal, meVal, err := resolveEnvConfig("/flag/root", "flagagent")
	if err != nil {
		t.Fatalf("resolveEnvConfig: %v", err)
	}

	if rootVal != "/flag/root" {
		t.Errorf("expected root=/flag/root (flag), got %q", rootVal)
	}
	if meVal != "flagagent" {
		t.Errorf("expected me=flagagent (flag), got %q", meVal)
	}
}

func TestRunEnvJSON(t *testing.T) {
	root := t.TempDir()
	setCLIVersionForTest(t, "test-version")

	// Write .amqrc
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t)

	// Root is the literal .amqrc root
	expectedRoot := filepath.Join(root, ".agent-mail")
	expectSamePath(t, result.Root, expectedRoot)
	expectSamePath(t, result.BaseRoot, expectedRoot)
	if result.SchemaVersion != 1 {
		t.Errorf("expected schema_version=1, got %d", result.SchemaVersion)
	}
	if result.AMQVersion != "test-version" {
		t.Errorf("expected amq_version=%q, got %q", "test-version", result.AMQVersion)
	}
	if result.RootSource != string(rootSourceProjectRC) {
		t.Errorf("expected root_source=%q, got %q", rootSourceProjectRC, result.RootSource)
	}
	if result.InSession {
		t.Error("expected in_session=false")
	}
	if result.SessionName != "" {
		t.Errorf("expected session_name=empty, got %q", result.SessionName)
	}
	// 'me' is not in .amqrc
	if result.Me != "" {
		t.Errorf("expected me=empty, got %q", result.Me)
	}
	// Project defaults to directory basename when not set explicitly
	expectedProject := filepath.Base(root)
	if result.Project != expectedProject {
		t.Errorf("expected project=%q, got %q", expectedProject, result.Project)
	}
	// No peers configured
	if len(result.Peers) != 0 {
		t.Errorf("expected peers={}, got %v", result.Peers)
	}
}

func TestRunEnvJSONWithPeers(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc with project + peers
	rcContent := `{"root": ".agent-mail", "project": "my-app", "peers": {"infra": "/tmp/infra/.agent-mail", "api": "/tmp/api/.agent-mail", "shared": "../shared/.agent-mail"}}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t, "--me", "claude")

	if result.Project != "my-app" {
		t.Errorf("expected project=%q, got %q", "my-app", result.Project)
	}
	if result.Me != "claude" {
		t.Errorf("expected me=%q, got %q", "claude", result.Me)
	}
	if len(result.Peers) != 3 {
		t.Fatalf("expected 3 peers, got %d", len(result.Peers))
	}
	if result.Peers["infra"] != "/tmp/infra/.agent-mail" {
		t.Errorf("expected peer infra=%q, got %q", "/tmp/infra/.agent-mail", result.Peers["infra"])
	}
	if result.Peers["api"] != "/tmp/api/.agent-mail" {
		t.Errorf("expected peer api=%q, got %q", "/tmp/api/.agent-mail", result.Peers["api"])
	}
	expectedShared, err := filepath.Abs(filepath.Join(root, "../shared/.agent-mail"))
	if err != nil {
		t.Fatalf("abs shared peer: %v", err)
	}
	expectSamePath(t, result.Peers["shared"], expectedShared)
}

func TestRunEnvJSONGlobalAmqrcNoProject(t *testing.T) {
	// Regression: global ~/.amqrc should not infer project from home dir basename.
	fakeHome := t.TempDir()

	// Write ~/.amqrc (global, no project field)
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	// Create the .agent-mail dir so root resolves
	if err := os.MkdirAll(filepath.Join(fakeHome, ".agent-mail"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Use an unrelated cwd with no project .amqrc
	cwd := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	oldHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", oldHome) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")
	_ = os.Unsetenv("AMQ_GLOBAL_ROOT")
	_ = os.Setenv("HOME", fakeHome)

	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t)

	// Project should be empty — global ~/.amqrc is a queue locator, not a project identity
	if result.Project != "" {
		t.Errorf("expected project=empty for global ~/.amqrc, got %q", result.Project)
	}
	if result.RootSource != string(rootSourceGlobalRC) {
		t.Errorf("expected root_source=%q, got %q", rootSourceGlobalRC, result.RootSource)
	}
	if len(result.Peers) != 0 {
		t.Errorf("expected peers={}, got %v", result.Peers)
	}
}

func TestRunEnvJSONV1SessionFlag(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".claude", "agents"), 0o700); err != nil {
		t.Fatalf("mkdir unrelated agents dir: %v", err)
	}

	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")
	_ = os.Unsetenv("AMQ_GLOBAL_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t, "--session", "feature-x", "--me", "codex")

	expectedBase := filepath.Join(root, ".agent-mail")
	expectedRoot := filepath.Join(expectedBase, "feature-x")
	expectSamePath(t, result.Root, expectedRoot)
	expectSamePath(t, result.BaseRoot, expectedBase)
	if !result.InSession {
		t.Error("expected in_session=true")
	}
	if result.SessionName != "feature-x" {
		t.Errorf("expected session_name=%q, got %q", "feature-x", result.SessionName)
	}
	if result.Me != "codex" {
		t.Errorf("expected me=%q, got %q", "codex", result.Me)
	}
	if result.RootSource != string(rootSourceFlag) {
		t.Errorf("expected root_source=%q, got %q", rootSourceFlag, result.RootSource)
	}
}

func TestRunEnvSessionRejectsAMRootOutsideLegacyPin(t *testing.T) {
	baseRoot := t.TempDir()
	currentRoot := filepath.Join(baseRoot, "current")
	targetRoot := filepath.Join(baseRoot, "feature-x")
	foreignRoot := filepath.Join(t.TempDir(), "foreign")
	for _, root := range []string{currentRoot, targetRoot, foreignRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("mkdir session root %s: %v", root, err)
		}
	}
	t.Setenv(envRoot, foreignRoot)
	t.Setenv(envBaseRoot, baseRoot)
	t.Setenv(envSession, "current")
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)

	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "feature-x", "--me", "codex", "--json"})
	})
	if err == nil || !strings.Contains(err.Error(), "differs from pinned root") {
		t.Fatalf("runEnv error = %v, want AM_ROOT/legacy-pin mismatch refusal", err)
	}
	if stdout != "" {
		t.Fatalf("runEnv emitted a replacement context after mismatch: %q", stdout)
	}
}

func TestRunEnvExportSessionEmitsBaseRootAndPinNote(t *testing.T) {
	root := t.TempDir()
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("AM_ROOT", "")
	t.Setenv("AM_BASE_ROOT", "")
	t.Setenv("AM_ME", "")
	t.Setenv("AMQ_GLOBAL_ROOT", "")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	projectRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	stdout, stderr, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "feature-x", "--me", "codex", "--export"})
	})
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	expectedBase := filepath.Join(projectRoot, ".agent-mail")
	expectedRoot := filepath.Join(expectedBase, "feature-x")
	want := "export AM_ROOT=" + shellQuotePosix(expectedRoot) + "\n" +
		"export AM_BASE_ROOT=" + shellQuotePosix(expectedBase) + "\n" +
		expectedPosixIdentityLines(t, expectedRoot, expectedBase) +
		"export AM_SESSION=feature-x\n" +
		"export AM_ME=codex\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "pinned to AMQ session feature-x") {
		t.Fatalf("stderr should contain session pin note, got %q", stderr)
	}
	if !strings.Contains(stderr, "one terminal, one session") {
		t.Fatalf("stderr should mention one terminal, one session, got %q", stderr)
	}
}

func TestRunEnvJSONV1AutoDetect(t *testing.T) {
	cwd := t.TempDir()
	fakeHome := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, ".agent-mail"), 0o755); err != nil {
		t.Fatalf("mkdir .agent-mail: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("HOME", fakeHome)
	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")
	_ = os.Unsetenv("AMQ_GLOBAL_ROOT")

	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result := runEnvJSONForTest(t)

	expectedRoot := filepath.Join(cwd, ".agent-mail")
	if !filepath.IsAbs(result.Root) || !filepath.IsAbs(result.BaseRoot) {
		t.Errorf("expected absolute root/base_root, got root=%q base=%q", result.Root, result.BaseRoot)
	}
	expectSamePath(t, result.Root, expectedRoot)
	expectSamePath(t, result.BaseRoot, expectedRoot)
	if result.RootSource != string(rootSourceAutoDetect) {
		t.Errorf("expected root_source=%q, got %q", rootSourceAutoDetect, result.RootSource)
	}
	if result.InSession {
		t.Error("expected in_session=false")
	}
	if result.SessionName != "" {
		t.Errorf("expected session_name=empty, got %q", result.SessionName)
	}
}

func TestRunEnvFish(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc with absolute path
	rcContent := `{"root": "/tmp/test-root"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runEnv([]string{"--shell", "fish"})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "set -gx AM_ROOT /tmp/test-root\n") {
		t.Errorf("expected set -gx AM_ROOT /tmp/test-root, got: %s", output)
	}
	// A missing identity clears any stale terminal identity.
	if !strings.Contains(output, "set -e AM_ME\n") {
		t.Errorf("expected stale AM_ME to be cleared, got: %s", output)
	}
}

func TestRunEnvWake(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc (wake test doesn't check root value, just wake output)
	rcContent := `{"root": "/tmp/test-root"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runEnv([]string{"--wake"})

	_ = w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	buf := make([]byte, 1024)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if !strings.Contains(output, "amq wake &") {
		t.Errorf("expected 'amq wake &', got: %s", output)
	}
}

// --- Global root resolution tests ---

func TestFindAndLoadAmqrcRejectsUntrustedProvenance(t *testing.T) {
	root := t.TempDir()
	old, _ := os.Getwd()
	defer func() { _ = os.Chdir(old) }()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, ".amqrc")
	if err := os.WriteFile(path, []byte(`{"root":".agent-mail"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := findAndLoadAmqrc(); err == nil || !strings.Contains(err.Error(), "group/world-writable") {
		t.Fatalf("expected writable .amqrc rejection, got %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "real.amqrc")
	if err := os.WriteFile(target, []byte(`{"root":".agent-mail"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := findAndLoadAmqrc(); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestGlobalAmqrcFallbackRefusedInUnconfiguredLinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for linked worktree routing")
	}

	fakeHome := t.TempDir()
	primary := filepath.Join(fakeHome, "workspace", "primary")
	linked := filepath.Join(fakeHome, "workspace", "linked")
	if err := os.MkdirAll(primary, 0o700); err != nil {
		t.Fatalf("mkdir primary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(primary, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGitForTest(t, primary, "init")
	runGitForTest(t, primary, "add", "README.md")
	runGitForTest(t, primary, "-c", "user.name=AMQ Test", "-c", "user.email=amq@example.invalid", "commit", "-m", "fixture")
	runGitForTest(t, primary, "worktree", "add", "-b", "linked", linked)

	primaryRoot := filepath.Join(primary, ".agent-mail")
	globalRoot := filepath.Join(fakeHome, "global-agent-mail")
	for _, root := range []string{primaryRoot, globalRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("mkdir root %s: %v", root, err)
		}
	}
	if err := os.WriteFile(filepath.Join(primary, ".amqrc"), []byte(`{"root":".agent-mail","project":"primary"}`), 0o600); err != nil {
		t.Fatalf("write primary .amqrc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".amqrc"), []byte(`{"root":"`+globalRoot+`","project":"unrelated"}`), 0o600); err != nil {
		t.Fatalf("write global .amqrc: %v", err)
	}

	t.Setenv("HOME", fakeHome)
	setOptionalEnv(t, envRoot, "", false)
	setOptionalEnv(t, envBaseRoot, "", false)
	setOptionalEnv(t, envSession, "", false)
	setOptionalEnv(t, envRootID, "", false)
	setOptionalEnv(t, envBaseRootID, "", false)
	setOptionalEnv(t, envMe, "", false)
	setOptionalEnv(t, envGlobalRoot, "", false)
	t.Chdir(linked)
	t.Cleanup(resetAmqrcCache)
	resetAmqrcCache()

	if _, err := os.Stat(filepath.Join(linked, ".amqrc")); !os.IsNotExist(err) {
		t.Fatalf("linked .amqrc unexpectedly exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(linked, ".agent-mail")); !os.IsNotExist(err) {
		t.Fatalf("linked .agent-mail unexpectedly exists: %v", err)
	}
	if _, err := findAndLoadAmqrc(); !errors.Is(err, errAmqrcNotFound) {
		t.Fatalf("linked worktree project config = %v, want errAmqrcNotFound", err)
	}

	_, _, _, err := resolveEnvConfigWithSource("", "")
	if err == nil {
		t.Fatal("expected implicit ~/.amqrc fallback to be refused in an unconfigured linked worktree")
	}
	for _, want := range []string{"Git worktree", linked, "~/.amqrc", "--session", "AMQ_GLOBAL_ROOT"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
	stdout, _, err := captureEnvOutput(t, func() error {
		return runEnv([]string{"--session", "session1", "--me", "codex-lead"})
	})
	if err == nil || GetExitCode(err) != ExitContextMismatch || !strings.Contains(err.Error(), "Git worktree") {
		t.Fatalf("amq env --session error = %v, want context-mismatch refusal", err)
	}
	if stdout != "" {
		t.Fatalf("amq env --session emitted a wrong-root context: %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(globalRoot, "session1")); !os.IsNotExist(err) {
		t.Fatalf("wrong-root session was mutated: %v", err)
	}
	if _, found, err := resolveDiscoveredBaseRoot(); err == nil || found {
		t.Fatalf("coop base discovery = found %v, err %v; want refusal", found, err)
	}

	t.Setenv(envGlobalRoot, primaryRoot)
	root, source, _, err := resolveEnvConfigWithSource("", "")
	if err != nil {
		t.Fatalf("explicit AMQ_GLOBAL_ROOT: %v", err)
	}
	expectSamePath(t, root, primaryRoot)
	if source != rootSourceGlobalEnv {
		t.Fatalf("source = %q, want %q", source, rootSourceGlobalEnv)
	}
	result := runEnvJSONForTest(t, "--session", "session1", "--me", "codex-lead")
	expectSamePath(t, result.Root, filepath.Join(primaryRoot, "session1"))
	if result.RootSource != string(rootSourceFlag) {
		t.Fatalf("session root source = %q, want %q", result.RootSource, rootSourceFlag)
	}
}
