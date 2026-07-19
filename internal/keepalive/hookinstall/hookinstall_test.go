package hookinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallBothWritesScriptAndMergesConfigs(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "hooks", "amq-keepalive-session-start.sh")
	binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
	claudeConfig := filepath.Join(dir, "claude", "settings.json")
	codexConfig := filepath.Join(dir, "codex", "hooks.json")
	mustWrite(t, claudeConfig, []byte(`{"hooks":{"SessionStart":[{"matcher":"resume","hooks":[{"type":"command","command":"existing"}]}]}}`))
	mustWrite(t, codexConfig, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"existing","timeout":5000}]}]}}`))

	result, err := Install(Options{
		Agent:        AgentBoth,
		ScriptPath:   scriptPath,
		BinaryPath:   binaryPath,
		ClaudeConfig: claudeConfig,
		CodexConfig:  codexConfig,
		Timeout:      time.Second,
	})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if !result.Script.Changed {
		t.Fatal("script changed = false, want true")
	}
	if result.Configs[AgentClaude].Backup == "" || result.Configs[AgentCodex].Backup == "" {
		t.Fatalf("expected config backups, got %#v", result.Configs)
	}
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat script: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("script mode = %o, want 755", info.Mode().Perm())
	}
	scriptData, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	if string(scriptData) != SessionStartScript {
		t.Fatal("installed script does not match embedded script")
	}

	claude := readJSON(t, claudeConfig)
	if countCommand(claude, result.Commands[AgentClaude]) != 1 {
		t.Fatalf("Claude command not installed exactly once:\n%s", mustMarshal(t, claude))
	}
	if !strings.Contains(result.Commands[AgentClaude], "AMQ_KEEPALIVE_TIMEOUT_SECONDS='1'") {
		t.Fatalf("Claude command missing timeout env: %q", result.Commands[AgentClaude])
	}

	codex := readJSON(t, codexConfig)
	if countCommand(codex, result.Commands[AgentCodex]) != 1 {
		t.Fatalf("Codex command not installed exactly once:\n%s", mustMarshal(t, codex))
	}
	if !strings.Contains(mustMarshal(t, codex), `"timeout": 6000`) {
		t.Fatalf("Codex hook timeout missing or wrong:\n%s", mustMarshal(t, codex))
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
	claudeConfig := filepath.Join(dir, "settings.json")

	opts := Options{
		Agent:        AgentClaude,
		ScriptPath:   filepath.Join(dir, "hook.sh"),
		BinaryPath:   binaryPath,
		ClaudeConfig: claudeConfig,
		Timeout:      time.Second,
	}
	first, err := Install(opts)
	if err != nil {
		t.Fatalf("first Install() error = %v", err)
	}
	second, err := Install(opts)
	if err != nil {
		t.Fatalf("second Install() error = %v", err)
	}
	if second.Configs[AgentClaude].Changed {
		t.Fatal("second config install changed file, want idempotent no-op")
	}
	doc := readJSON(t, claudeConfig)
	if countCommand(doc, first.Commands[AgentClaude]) != 1 {
		t.Fatalf("command count != 1 after repeat install:\n%s", mustMarshal(t, doc))
	}
}

func TestDryRunDoesNotWriteFiles(t *testing.T) {
	dir := t.TempDir()
	binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
	scriptPath := filepath.Join(dir, "hook.sh")
	configPath := filepath.Join(dir, "settings.json")

	result, err := Install(Options{
		Agent:        AgentClaude,
		ScriptPath:   scriptPath,
		BinaryPath:   binaryPath,
		ClaudeConfig: configPath,
		Timeout:      time.Second,
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Install(dry-run) error = %v", err)
	}
	if !result.DryRun || result.Commands[AgentClaude] == "" {
		t.Fatalf("bad dry-run result: %#v", result)
	}
	if _, err := os.Stat(scriptPath); !os.IsNotExist(err) {
		t.Fatalf("script stat err = %v, want not exist", err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("config stat err = %v, want not exist", err)
	}
}

func TestEmbeddedScriptMatchesRepositoryHook(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "amq-keepalive-session-start.sh"))
	if err != nil {
		t.Fatalf("read repository hook: %v", err)
	}
	if string(data) != SessionStartScript {
		t.Fatal("embedded SessionStart script drifted from hooks/amq-keepalive-session-start.sh")
	}
}

func TestSessionStartScriptNormalizesInvalidTimeoutAndReturns(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeSessionStartScript(t, dir)
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), "#!/bin/sh\nsleep 5\n")
	logPath := filepath.Join(dir, "session-start.log")

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_LOG="+logPath,
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=0",
		"AMQ_KEEPALIVE_DEFAULT_TIMEOUT_SECONDS=1",
		"AMQ_KEEPALIVE_STDIN_TIMEOUT_SECONDS=1",
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("hook took %s, want bounded return", elapsed)
	}
	if got := stdout.String(); got != "{}\n" {
		t.Fatalf("stdout = %q, want empty hook response", got)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "invalid timeout 0; using 1s") {
		t.Fatalf("log missing invalid timeout normalization:\n%s", logText)
	}
	if !strings.Contains(logText, "reattach timed out after 1s") {
		t.Fatalf("log missing timeout:\n%s", logText)
	}
}

func TestSessionStartScriptDoesNotBlockOnOpenStdin(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeSessionStartScript(t, dir)
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), "#!/bin/sh\nexit 0\n")
	logPath := filepath.Join(dir, "session-start.log")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", scriptPath)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	cmd.Stdin = reader
	cmd.Env = append(os.Environ(),
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_LOG="+logPath,
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=2",
		"AMQ_KEEPALIVE_STDIN_TIMEOUT_SECONDS=1",
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("hook took %s, want stdin read bounded", elapsed)
	}
	if got := stdout.String(); got != "{}\n" {
		t.Fatalf("stdout = %q, want empty hook response", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want silent quick success", got)
	}
}

func TestSessionStartScriptAutoSelectsExactCmuxSurfaceAndLogsFailure(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeSessionStartScript(t, dir)
	argsPath := filepath.Join(dir, "args.log")
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), `#!/bin/sh
printf '%s\n' "$@" > "$AMQ_KEEPALIVE_CAPTURE"
echo 'existing wake target differs' >&2
exit 7
`)
	logPath := filepath.Join(dir, "session-start.log")

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader("{}\n")
	cmd.Env = append(withoutEnv(os.Environ(),
		"AMQ_KEEPALIVE_ADAPTER",
		"AMQ_KEEPALIVE_TARGET",
		"CMUX_SURFACE_ID",
	),
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_CAPTURE="+argsPath,
		"AMQ_KEEPALIVE_LOG="+logPath,
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=2",
		"CMUX_SURFACE_ID=F901D722-6789-4BBB-9818-C4E97F20BEB3",
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v", err)
	}
	if got := stdout.String(); got != "{}\n" {
		t.Fatalf("stdout = %q, want empty hook response", got)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	argsText := string(argsData)
	for _, want := range []string{
		"reattach\n",
		"--adapter\ncmux\n",
		"--wake-ready-timeout\n1000ms\n",
		"--target\ncmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3\n",
	} {
		if !strings.Contains(argsText, want) {
			t.Fatalf("args missing %q:\n%s", want, argsText)
		}
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "existing wake target differs") ||
		!strings.Contains(logText, "reattach failed status=7 adapter=cmux target=cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3") {
		t.Fatalf("log does not expose target mismatch:\n%s", logText)
	}
}

func TestSessionStartScriptFallsBackToGhosttyOutsideCmux(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeSessionStartScript(t, dir)
	argsPath := filepath.Join(dir, "args.log")
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), `#!/bin/sh
printf '%s\n' "$@" > "$AMQ_KEEPALIVE_CAPTURE"
`)

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader("{}\n")
	cmd.Env = append(withoutEnv(os.Environ(),
		"AMQ_KEEPALIVE_ADAPTER",
		"AMQ_KEEPALIVE_TARGET",
		"CMUX_SURFACE_ID",
	),
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_CAPTURE="+argsPath,
		"AMQ_KEEPALIVE_LOG="+filepath.Join(dir, "session-start.log"),
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=2",
	)
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v", err)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	argsText := string(argsData)
	if !strings.Contains(argsText, "--adapter\nghostty\n") {
		t.Fatalf("args do not fall back to Ghostty:\n%s", argsText)
	}
	if strings.Contains(argsText, "--target\n") {
		t.Fatalf("fallback unexpectedly supplied a target:\n%s", argsText)
	}
}

func TestSessionStartScriptClampsInnerWakeTimeoutBelowOuterWatchdog(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeSessionStartScript(t, dir)
	argsPath := filepath.Join(dir, "args.log")
	logPath := filepath.Join(dir, "session-start.log")
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), `#!/bin/sh
printf '%s\n' "$@" > "$AMQ_KEEPALIVE_CAPTURE"
`)

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader("{}\n")
	cmd.Env = append(os.Environ(),
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_CAPTURE="+argsPath,
		"AMQ_KEEPALIVE_LOG="+logPath,
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=2",
		"AMQ_KEEPALIVE_WAKE_TIMEOUT_MILLISECONDS=2000",
	)
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v", err)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if !strings.Contains(string(argsData), "--wake-ready-timeout\n1500ms\n") {
		t.Fatalf("inner timeout was not clamped below outer watchdog:\n%s", argsData)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "wake timeout 2000ms must be shorter than outer 2000ms; using 1500ms") {
		t.Fatalf("clamp was not logged:\n%s", logData)
	}
}

func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	return writeExecutableBody(t, path, "#!/bin/sh\nexit 0\n")
}

func withoutEnv(env []string, keys ...string) []string {
	blocked := map[string]bool{}
	for _, key := range keys {
		blocked[key] = true
	}
	out := make([]string, 0, len(env))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if !blocked[key] {
			out = append(out, item)
		}
	}
	return out
}

func writeSessionStartScript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "hook.sh")
	return writeExecutableBody(t, path, SessionStartScript)
}

func writeExecutableBody(t *testing.T, path string, body string) string {
	t.Helper()
	mustWrite(t, path, []byte(body))
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod executable: %v", err)
	}
	return path
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readJSON(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal json: %v\n%s", err, data)
	}
	return doc
}

func countCommand(doc map[string]interface{}, command string) int {
	count := 0
	hooks, _ := doc["hooks"].(map[string]interface{})
	for _, entry := range interfaceArray(hooks["SessionStart"]) {
		entryObj, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		for _, hook := range interfaceArray(entryObj["hooks"]) {
			hookObj, ok := hook.(map[string]interface{})
			if ok && hookObj["command"] == command {
				count++
			}
		}
	}
	return count
}

func mustMarshal(t *testing.T, v interface{}) string {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}
