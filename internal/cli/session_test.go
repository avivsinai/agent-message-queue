package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndLoadSession(t *testing.T) {
	root := t.TempDir()

	session := sessionData{
		Root: root,
		Me:   "claude",
		Wake: 12345,
	}

	// Write session
	if err := writeSession(root, session); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	// Verify file exists
	sessionPath := filepath.Join(root, sessionFileName)
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("session file not created: %v", err)
	}

	// Load session
	loaded, err := loadSession(root)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}

	if loaded.Root != session.Root {
		t.Errorf("expected root=%q, got %q", session.Root, loaded.Root)
	}
	if loaded.Me != session.Me {
		t.Errorf("expected me=%q, got %q", session.Me, loaded.Me)
	}
	if loaded.Wake != session.Wake {
		t.Errorf("expected wake=%d, got %d", session.Wake, loaded.Wake)
	}
}

func TestLoadSessionNotFound(t *testing.T) {
	root := t.TempDir()

	_, err := loadSession(root)
	if !errors.Is(err, errSessionNotFound) {
		t.Errorf("expected errSessionNotFound, got %v", err)
	}
}

func TestLoadSessionInvalidJSON(t *testing.T) {
	root := t.TempDir()

	// Write invalid JSON to session file
	sessionPath := filepath.Join(root, sessionFileName)
	if err := os.WriteFile(sessionPath, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := loadSession(root)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
	if errors.Is(err, errSessionNotFound) {
		t.Error("should not be errSessionNotFound for invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid session file") {
		t.Errorf("expected 'invalid session file' in error, got: %v", err)
	}
}

func TestFindSessionRoot(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "sub", "deep")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write session file in root
	session := sessionData{Root: root, Me: "claude"}
	if err := writeSession(root, session); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	// Change to subdir and try to find session
	if err := os.Chdir(subdir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	found, err := findSessionRoot()
	if err != nil {
		t.Fatalf("findSessionRoot: %v", err)
	}

	foundResolved, _ := filepath.EvalSymlinks(found)
	rootResolved, _ := filepath.EvalSymlinks(root)
	if foundResolved != rootResolved {
		t.Errorf("expected %q, got %q", rootResolved, foundResolved)
	}
}

func TestFindSessionRootInAgentMail(t *testing.T) {
	root := t.TempDir()
	agentMailDir := filepath.Join(root, ".agent-mail")
	if err := os.MkdirAll(agentMailDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write session file inside .agent-mail
	session := sessionData{Root: agentMailDir, Me: "codex"}
	if err := writeSession(agentMailDir, session); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	// Change to root and try to find session (should find in .agent-mail)
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	found, err := findSessionRoot()
	if err != nil {
		t.Fatalf("findSessionRoot: %v", err)
	}

	foundResolved, _ := filepath.EvalSymlinks(found)
	expectedResolved, _ := filepath.EvalSymlinks(agentMailDir)
	if foundResolved != expectedResolved {
		t.Errorf("expected %q, got %q", expectedResolved, foundResolved)
	}
}

func TestFindSessionRootNotFound(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	_, err := findSessionRoot()
	if !errors.Is(err, errSessionNotFound) {
		t.Errorf("expected errSessionNotFound, got %v", err)
	}
}

func TestResolveFromSession(t *testing.T) {
	root := t.TempDir()

	// Write session file
	session := sessionData{Root: "/custom/root", Me: "testbot"}
	if err := writeSession(root, session); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result, err := resolveFromSession()
	if err != nil {
		t.Fatalf("resolveFromSession: %v", err)
	}

	if result.Root != "/custom/root" {
		t.Errorf("expected root=/custom/root, got %q", result.Root)
	}
	if result.Me != "testbot" {
		t.Errorf("expected me=testbot, got %q", result.Me)
	}
}

func TestResolveFromSessionNotFound(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	result, err := resolveFromSession()
	if err != nil {
		t.Fatalf("resolveFromSession should not error when not found: %v", err)
	}
	if result.Root != "" || result.Me != "" {
		t.Errorf("expected empty result when no session, got root=%q me=%q", result.Root, result.Me)
	}
}

func TestResolveRootForSessionFromFlag(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Flag should take precedence
	resolved, err := resolveRootForSession("/flag/root")
	if err != nil {
		t.Fatalf("resolveRootForSession: %v", err)
	}
	if resolved != "/flag/root" {
		t.Errorf("expected /flag/root, got %q", resolved)
	}
}

func TestResolveRootForSessionFromEnv(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Setenv("AM_ROOT", "/env/root")
	defer func() { _ = os.Unsetenv("AM_ROOT") }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	resolved, err := resolveRootForSession("")
	if err != nil {
		t.Fatalf("resolveRootForSession: %v", err)
	}
	if resolved != "/env/root" {
		t.Errorf("expected /env/root, got %q", resolved)
	}
}

func TestResolveRootForSessionFromAmqrc(t *testing.T) {
	root := t.TempDir()

	// Write .amqrc with relative root
	rcContent := `{"root": ".agent-mail"}`
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte(rcContent), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	resolved, err := resolveRootForSession("")
	if err != nil {
		t.Fatalf("resolveRootForSession: %v", err)
	}

	expected := filepath.Join(root, ".agent-mail")
	resolvedEval, _ := filepath.EvalSymlinks(resolved)
	expectedEval, _ := filepath.EvalSymlinks(expected)
	if resolvedEval != expectedEval {
		t.Errorf("expected %q, got %q", expectedEval, resolvedEval)
	}
}

func TestResolveRootForSessionFromAutoDetect(t *testing.T) {
	root := t.TempDir()

	// Create .agent-mail directory (no .amqrc)
	agentMailDir := filepath.Join(root, ".agent-mail")
	if err := os.MkdirAll(agentMailDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	resolved, err := resolveRootForSession("")
	if err != nil {
		t.Fatalf("resolveRootForSession: %v", err)
	}

	if resolved != ".agent-mail" {
		t.Errorf("expected .agent-mail, got %q", resolved)
	}
}

func TestResolveRootForSessionInvalidAmqrcError(t *testing.T) {
	root := t.TempDir()

	// Write invalid .amqrc
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Should error when .amqrc is invalid and no override
	_, err := resolveRootForSession("")
	if err == nil {
		t.Error("expected error for invalid .amqrc")
	}
	if !strings.Contains(err.Error(), "invalid .amqrc") {
		t.Errorf("expected 'invalid .amqrc' in error, got: %v", err)
	}
}

func TestResolveRootForSessionInvalidAmqrcWithFlagOverride(t *testing.T) {
	root := t.TempDir()

	// Write invalid .amqrc
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Flag should override broken .amqrc
	resolved, err := resolveRootForSession("/override/root")
	if err != nil {
		t.Fatalf("expected success with flag override, got: %v", err)
	}
	if resolved != "/override/root" {
		t.Errorf("expected /override/root, got %q", resolved)
	}
}

func TestResolveRootForSessionInvalidAmqrcWithEnvOverride(t *testing.T) {
	root := t.TempDir()

	// Write invalid .amqrc
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Setenv("AM_ROOT", "/env/override")
	defer func() { _ = os.Unsetenv("AM_ROOT") }()

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Env should override broken .amqrc
	resolved, err := resolveRootForSession("")
	if err != nil {
		t.Fatalf("expected success with env override, got: %v", err)
	}
	if resolved != "/env/override" {
		t.Errorf("expected /env/override, got %q", resolved)
	}
}

func TestResolveRootForSessionInvalidAmqrcWithAutoDetect(t *testing.T) {
	root := t.TempDir()

	// Write invalid .amqrc
	if err := os.WriteFile(filepath.Join(root, ".amqrc"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write .amqrc: %v", err)
	}

	// Create .agent-mail directory (lower precedence than .amqrc)
	if err := os.MkdirAll(filepath.Join(root, ".agent-mail"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Auto-detect is LOWER precedence than .amqrc, so it should NOT override invalid .amqrc
	_, err := resolveRootForSession("")
	if err == nil {
		t.Error("expected error for invalid .amqrc even with auto-detect available")
	}
	if !strings.Contains(err.Error(), "invalid .amqrc") {
		t.Errorf("expected 'invalid .amqrc' in error, got: %v", err)
	}
}

func TestResolveRootForSessionNoConfig(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// No .amqrc, no .agent-mail, no env vars
	_, err := resolveRootForSession("")
	if err == nil {
		t.Error("expected error when no config found")
	}
	if !strings.Contains(err.Error(), "cannot determine root") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRunSessionStartCreatesDirectory(t *testing.T) {
	root := t.TempDir()
	newRoot := filepath.Join(root, "new", "nested", "dir")

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

	// Start session with --no-wake to avoid spawning background process
	err := runSessionStart([]string{"--me", "claude", "--root", newRoot, "--no-wake"})

	_ = w.Close()
	os.Stdout = oldStdout

	// Read output
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if err != nil {
		t.Fatalf("runSessionStart: %v, output: %s", err, output)
	}

	// Verify directory was created
	if _, err := os.Stat(newRoot); err != nil {
		t.Errorf("root directory not created: %v", err)
	}

	// Verify session file was created
	sessionPath := filepath.Join(newRoot, sessionFileName)
	if _, err := os.Stat(sessionPath); err != nil {
		t.Errorf("session file not created: %v", err)
	}

	// Verify session content
	session, err := loadSession(newRoot)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if session.Me != "claude" {
		t.Errorf("expected me=claude, got %q", session.Me)
	}

	// Cleanup
	_ = os.RemoveAll(newRoot)
}

func TestRunSessionStartRequiresMe(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	// Create .agent-mail directory so root can be auto-detected
	if err := os.MkdirAll(filepath.Join(root, ".agent-mail"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	err := runSessionStart([]string{"--no-wake"})
	if err == nil {
		t.Error("expected error when --me not provided")
	}
	if !strings.Contains(err.Error(), "--me is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRunSessionStartRejectsActiveSession(t *testing.T) {
	root := t.TempDir()

	// Create existing session
	session := sessionData{Root: root, Me: "existingbot"}
	if err := writeSession(root, session); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")
	_ = os.Unsetenv("AM_ME")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	err := runSessionStart([]string{"--me", "newbot", "--root", root, "--no-wake"})
	if err == nil {
		t.Error("expected error when session already active")
	}
	if !strings.Contains(err.Error(), "session already active") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRunSessionStop(t *testing.T) {
	root := t.TempDir()

	// Create session
	session := sessionData{Root: root, Me: "claude", Wake: 0}
	if err := writeSession(root, session); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSessionStop([]string{"--root", root})

	_ = w.Close()
	os.Stdout = oldStdout

	// Read output
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if err != nil {
		t.Fatalf("runSessionStop: %v, output: %s", err, output)
	}

	// Verify session file was removed
	sessionPath := filepath.Join(root, sessionFileName)
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Errorf("session file should be removed")
	}

	if !strings.Contains(output, "Session stopped") {
		t.Errorf("expected 'Session stopped' in output, got: %s", output)
	}
}

func TestRunSessionStopNoSession(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	err := runSessionStop([]string{"--root", root})
	if err == nil {
		t.Error("expected error when no session active")
	}
	if !strings.Contains(err.Error(), "no active session") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRunSessionStatus(t *testing.T) {
	root := t.TempDir()

	// Create session
	session := sessionData{Root: root, Me: "claude", Wake: 0}
	if err := writeSession(root, session); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSessionStatus([]string{"--root", root})

	_ = w.Close()
	os.Stdout = oldStdout

	// Read output
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if err != nil {
		t.Fatalf("runSessionStatus: %v, output: %s", err, output)
	}

	if !strings.Contains(output, "Session active") {
		t.Errorf("expected 'Session active' in output, got: %s", output)
	}
	if !strings.Contains(output, "me:   claude") {
		t.Errorf("expected 'me:   claude' in output, got: %s", output)
	}
}

func TestRunSessionStatusJSON(t *testing.T) {
	root := t.TempDir()

	// Create session
	session := sessionData{Root: root, Me: "codex", Wake: 0}
	if err := writeSession(root, session); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSessionStatus([]string{"--root", root, "--json"})

	_ = w.Close()
	os.Stdout = oldStdout

	// Read output
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if err != nil {
		t.Fatalf("runSessionStatus: %v, output: %s", err, output)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v, output was: %s", err, output)
	}

	if result["active"] != true {
		t.Errorf("expected active=true, got %v", result["active"])
	}
	if result["me"] != "codex" {
		t.Errorf("expected me=codex, got %v", result["me"])
	}
}

func TestRunSessionStatusNoSession(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSessionStatus([]string{"--root", root})

	_ = w.Close()
	os.Stdout = oldStdout

	// Read output
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if err != nil {
		t.Fatalf("runSessionStatus: %v, output: %s", err, output)
	}

	if !strings.Contains(output, "No active session") {
		t.Errorf("expected 'No active session' in output, got: %s", output)
	}
}

func TestRunSessionStatusNoSessionJSON(t *testing.T) {
	root := t.TempDir()

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	_ = os.Unsetenv("AM_ROOT")

	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runSessionStatus([]string{"--root", root, "--json"})

	_ = w.Close()
	os.Stdout = oldStdout

	// Read output
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	if err != nil {
		t.Fatalf("runSessionStatus: %v, output: %s", err, output)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v, output was: %s", err, output)
	}

	if result["active"] != false {
		t.Errorf("expected active=false, got %v", result["active"])
	}
}

func TestGetSessionPID(t *testing.T) {
	root := t.TempDir()

	// Create session with wake PID
	session := sessionData{Root: root, Me: "claude", Wake: 99999}
	if err := writeSession(root, session); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	pid := GetSessionPID(root)
	if pid != 99999 {
		t.Errorf("expected PID=99999, got %d", pid)
	}
}

func TestGetSessionPIDNoSession(t *testing.T) {
	root := t.TempDir()

	pid := GetSessionPID(root)
	if pid != 0 {
		t.Errorf("expected PID=0 when no session, got %d", pid)
	}
}

func TestIsSessionActive(t *testing.T) {
	root := t.TempDir()

	// No session initially
	if IsSessionActive(root) {
		t.Error("expected inactive when no session")
	}

	// Create session
	session := sessionData{Root: root, Me: "claude"}
	if err := writeSession(root, session); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	if !IsSessionActive(root) {
		t.Error("expected active when session exists")
	}
}

func TestSessionFilePath(t *testing.T) {
	root := "/some/root"
	expected := "/some/root/.session"
	got := SessionFilePath(root)
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}
