package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestEnqueueCLIWritesSpoolWithoutRendezvous(t *testing.T) {
	root := newBridgeRoot(t, "codex")
	wrongRoot := newBridgeRoot(t, "other")
	configPath := writeEnqueueConfigFile(t, root, EnqueueConfigFile{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude"},
	})
	message := testMessage(t, "cli-enqueue", "thread-cli", "codex", "from bot")

	var httpHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpHits++
		http.Error(w, "must not contact rendezvous", http.StatusTeapot)
	}))
	t.Cleanup(server.Close)

	t.Setenv("AM_ROOT", wrongRoot)
	cmd := exec.Command("go", "run", ".", "enqueue", "--config", configPath, "--dest-alias", "mac/claude")
	cmd.Dir = bridgeMainDir(t)
	cmd.Stdin = bytes.NewReader(message)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("enqueue: %v stderr=%s", err, stderr.String())
	}
	if httpHits != 0 {
		t.Fatalf("rendezvous contacted %d times", httpHits)
	}

	spoolPath := strings.TrimSpace(string(out))
	want := filepath.Join(root, "bridge", "outbox", "codex", "new", "cli-enqueue.md")
	if spoolPath != want {
		t.Fatalf("stdout path = %q, want %q", spoolPath, want)
	}
	got, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(message) {
		t.Fatalf("spool bytes = %q, want %q", got, message)
	}
	if _, err := os.Stat(filepath.Join(wrongRoot, "bridge")); !os.IsNotExist(err) {
		t.Fatalf("AM_ROOT was used instead of config root: %v", err)
	}
}

func TestEnqueueCLIRejectsRootFlagAndUnknownConfigField(t *testing.T) {
	root := newBridgeRoot(t, "codex")
	configPath := writeEnqueueConfigFile(t, root, EnqueueConfigFile{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude"},
	})
	message := testMessage(t, "reject-root", "thread-reject", "codex", "nope")

	if err := runEnqueue([]string{"--config", configPath, "--dest-alias", "mac/claude", "--root", root}); err == nil || !strings.Contains(err.Error(), "--root is not allowed") {
		t.Fatalf("root flag error = %v", err)
	}

	badConfig := filepath.Join(root, "bad-config.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["rendezvous"] = "https://example.test"
	badRaw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badConfig, badRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	runEnqueueCLI(t, []string{"enqueue", "--config", badConfig, "--dest-alias", "mac/claude"}, message, 1, "unknown")
}

func TestEnqueueCLIRejectsPermissiveConfigAndDestAllowlist(t *testing.T) {
	root := newBridgeRoot(t, "codex")
	permissive := filepath.Join(root, "permissive.json")
	body := mustEnqueueConfigJSON(t, EnqueueConfigFile{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude"},
	})
	if err := os.WriteFile(permissive, body, 0o644); err != nil {
		t.Fatal(err)
	}
	message := testMessage(t, "perm-check", "thread-perm", "codex", "nope")
	runEnqueueCLI(t, []string{"enqueue", "--config", permissive, "--dest-alias", "mac/claude"}, message, 1, "0600")

	configPath := writeEnqueueConfigFile(t, root, EnqueueConfigFile{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude"},
	})
	runEnqueueCLI(t, []string{"enqueue", "--config", configPath, "--dest-alias", "mac/cursor"}, message, 1, "allowlist")
}

func TestEnqueueCLIRejectsInvalidMessageFilename(t *testing.T) {
	root := newBridgeRoot(t, "codex")
	configPath := writeEnqueueConfigFile(t, root, EnqueueConfigFile{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude"},
	})
	message := testMessage(t, "../escape", "thread-bad", "codex", "nope")
	runEnqueueCLI(t, []string{"enqueue", "--config", configPath, "--dest-alias", "mac/claude"}, message, 1, "filename")
}

type EnqueueConfigFile struct {
	Root               string   `json:"root"`
	SourceHost         string   `json:"source_host"`
	SourceHandle       string   `json:"source_handle"`
	AllowedDestAliases []string `json:"allowed_dest_aliases"`
}

func writeEnqueueConfigFile(t *testing.T, dir string, cfg EnqueueConfigFile) string {
	t.Helper()
	path := filepath.Join(dir, "enqueue-config.json")
	if err := os.WriteFile(path, mustEnqueueConfigJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustEnqueueConfigJSON(t *testing.T, cfg EnqueueConfigFile) []byte {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func runEnqueueCLI(t *testing.T, args []string, stdin []byte, wantExit int, wantErr string) {
	t.Helper()
	cmd := exec.Command("go", append([]string{"run", "."}, args...)...)
	cmd.Dir = bridgeMainDir(t)
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if wantExit == 0 {
		if err != nil {
			t.Fatalf("enqueue success wanted, got %v stderr=%s", err, stderr.String())
		}
		return
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("enqueue error = %v stderr=%s, want exit %d", err, stderr.String(), wantExit)
	}
	if exitErr.ExitCode() != wantExit {
		t.Fatalf("enqueue exit = %d stderr=%s, want %d", exitErr.ExitCode(), stderr.String(), wantExit)
	}
	combined := stderr.String()
	if wantErr != "" && !strings.Contains(combined, wantErr) {
		t.Fatalf("enqueue stderr = %q, want substring %q", combined, wantErr)
	}
}

func bridgeMainDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}

func TestRunEnqueueUnitRejectsMissingFlags(t *testing.T) {
	if err := runEnqueue([]string{}); err == nil || !strings.Contains(err.Error(), "--config is required") {
		t.Fatalf("missing config error = %v", err)
	}
	if err := runEnqueue([]string{"--config", "/tmp/enqueue.json"}); err == nil || !strings.Contains(err.Error(), "--dest-alias is required") {
		t.Fatalf("missing dest error = %v", err)
	}
}

func TestRunEnqueueUnitReadsStdin(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	configPath := writeEnqueueConfigFile(t, root, EnqueueConfigFile{
		Root:               root,
		SourceHost:         "grok-host",
		SourceHandle:       "codex",
		AllowedDestAliases: []string{"mac/claude"},
	})
	message := testMessage(t, "unit-enqueue", "thread-unit", "codex", "stdin")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })
	go func() {
		_, _ = w.Write(message)
		_ = w.Close()
	}()

	oldStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = pw
	t.Cleanup(func() { os.Stdout = oldStdout })

	if err := runEnqueue([]string{"--config", configPath, "--dest-alias", "mac/claude"}); err != nil {
		t.Fatalf("runEnqueue: %v", err)
	}
	_ = pw.Close()
	out, err := io.ReadAll(pr)
	if err != nil {
		t.Fatal(err)
	}
	spoolPath := strings.TrimSpace(string(out))
	got, err := os.ReadFile(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(message) {
		t.Fatalf("spool = %q, want %q", got, message)
	}
}

func TestRunEnqueueUnitRejectsRootFlag(t *testing.T) {
	if err := runEnqueue([]string{"--config", "/tmp/enqueue.json", "--dest-alias", "mac/claude", "--root", "/tmp"}); err == nil || !strings.Contains(err.Error(), "--root is not allowed") {
		t.Fatalf("root flag error = %v", err)
	}
}

func TestRunEnqueueUnitRejectsPromptControlledFlags(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--rendezvous", "--me", "--spool", "-root"} {
		err := runEnqueue([]string{flag, "x", "--config", "/tmp/enqueue.json", "--dest-alias", "mac/claude"})
		if err == nil {
			t.Fatalf("%s: accepted", flag)
		}
		if !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("%s error = %v, want not allowed", flag, err)
		}
	}
}
