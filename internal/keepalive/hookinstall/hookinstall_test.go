package hookinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain isolates HOME (and USERPROFILE) to a fresh temp dir for the whole
// package before any test runs. DefaultScriptPath/DefaultClaudeConfig/
// DefaultCodexConfig resolve via os.UserHomeDir, which honors HOME, so this
// guarantees no test in the package can write to the real ~/.codex/hooks.json
// or ~/.claude/settings.json under ANY mutation — not only the two named
// tests. Tests that need their own sentinel HOME (the negative case) still set
// it via t.Setenv inside the run.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "amq-hookinstall-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hookinstall TestMain: temp home: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", home); err != nil {
		fmt.Fprintf(os.Stderr, "hookinstall TestMain: set HOME: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("USERPROFILE", home); err != nil {
		fmt.Fprintf(os.Stderr, "hookinstall TestMain: set USERPROFILE: %v\n", err)
		os.Exit(1)
	}
	// os.Exit skips defers, so clean up before exiting with the real code.
	code := m.Run()
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintf(os.Stderr, "hookinstall TestMain: remove temp home: %v\n", err)
	}
	os.Exit(code)
}

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
	if !strings.Contains(mustMarshal(t, codex), `"timeout": 6`) {
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
	if err := os.Chmod(opts.ScriptPath, 0o600); err != nil {
		t.Fatalf("chmod script before repeat install: %v", err)
	}
	second, err := Install(opts)
	if err != nil {
		t.Fatalf("second Install() error = %v", err)
	}
	if second.Configs[AgentClaude].Changed {
		t.Fatal("second config install changed file, want idempotent no-op")
	}
	if second.Script.Changed {
		t.Fatal("second script install changed identical bytes, want idempotent no-op")
	}
	scriptData, err := os.ReadFile(opts.ScriptPath)
	if err != nil || string(scriptData) != SessionStartScript {
		t.Fatalf("script bytes changed: err=%v bytes=%q", err, scriptData)
	}
	info, err := os.Stat(opts.ScriptPath)
	if err != nil {
		t.Fatalf("stat repeated script: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("script mode = %v, want 0755", info.Mode().Perm())
	}
	doc := readJSON(t, claudeConfig)
	if countCommand(doc, first.Commands[AgentClaude]) != 1 {
		t.Fatalf("command count != 1 after repeat install:\n%s", mustMarshal(t, doc))
	}
}

func TestInstallCodexCreatesFirstSessionStartEntryWhenListIsEmpty(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "hooks.json")
	mustWrite(t, configPath, []byte(`{"hooks":{"SessionStart":[]}}`))

	result, err := Install(Options{
		Agent:       AgentCodex,
		ScriptPath:  filepath.Join(dir, "hook.sh"),
		BinaryPath:  writeExecutable(t, filepath.Join(dir, "amq-keepalive")),
		CodexConfig: configPath,
		Timeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	doc := readJSON(t, configPath)
	if countCommand(doc, result.Commands[AgentCodex]) != 1 {
		t.Fatalf("Codex command not installed into empty SessionStart list:\n%s", mustMarshal(t, doc))
	}
	hooks, ok := doc["hooks"].(map[string]interface{})
	if !ok {
		t.Fatalf("hooks = %#v, want object", doc["hooks"])
	}
	if entries := interfaceArray(hooks["SessionStart"]); len(entries) != 1 {
		t.Fatalf("SessionStart entries = %d, want 1", len(entries))
	}
}

func TestInstallRefusesTrailingJSON(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "settings.json")
	original := []byte("{\"hooks\":{}}\n{\"injected\":true}\n")
	mustWrite(t, configPath, original)

	_, err := Install(Options{
		Agent:        AgentClaude,
		ScriptPath:   filepath.Join(dir, "hook.sh"),
		BinaryPath:   writeExecutable(t, filepath.Join(dir, "amq-keepalive")),
		ClaudeConfig: configPath,
		Timeout:      time.Second,
	})
	if err == nil {
		t.Fatal("Install() error = nil, want trailing JSON refusal")
	}
	if !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("Install() error = %v, want trailing JSON", err)
	}
	got, readErr := os.ReadFile(configPath)
	if readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("config changed after trailing JSON: err=%v bytes=%q", readErr, got)
	}
}

func TestInstallCodexRefusesWrongTypeInnerHooks(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "hooks.json")
	original := []byte(`{"hooks":{"SessionStart":[{"hooks":{"type":"command","command":"existing"}}]}}`)
	mustWrite(t, configPath, original)

	_, err := Install(Options{
		Agent:       AgentCodex,
		ScriptPath:  filepath.Join(dir, "hook.sh"),
		BinaryPath:  writeExecutable(t, filepath.Join(dir, "amq-keepalive")),
		CodexConfig: configPath,
		Timeout:     time.Second,
	})
	if err == nil {
		t.Fatal("Install() error = nil, want wrong-type inner hooks refusal")
	}
	if !strings.Contains(err.Error(), `"hooks" must be a JSON array`) {
		t.Fatalf("Install() error = %v, want typed inner hooks", err)
	}
	got, readErr := os.ReadFile(configPath)
	if readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("wrong-type inner hooks were wiped: err=%v bytes=%q", readErr, got)
	}
}

func TestInstallBothRefusesWhenSecondConfigFailsPreflight(t *testing.T) {
	dir := t.TempDir()
	claudeConfig := filepath.Join(dir, "claude", "settings.json")
	claudeOriginal := []byte(`{"hooks":{"SessionStart":[]}}`)
	mustWrite(t, claudeConfig, claudeOriginal)
	codexConfig := filepath.Join(dir, "codex", "hooks.json")
	if err := os.MkdirAll(codexConfig, 0o700); err != nil {
		t.Fatalf("mkdir codex config path: %v", err)
	}

	_, err := Install(Options{
		Agent:        AgentBoth,
		ScriptPath:   filepath.Join(dir, "hook.sh"),
		BinaryPath:   writeExecutable(t, filepath.Join(dir, "amq-keepalive")),
		ClaudeConfig: claudeConfig,
		CodexConfig:  codexConfig,
		Timeout:      time.Second,
	})
	if err == nil {
		t.Fatal("Install() error = nil, want second-config refusal")
	}
	if strings.Contains(err.Error(), "partial hookinstall commit") {
		t.Fatalf("preflight failure committed Claude: %v", err)
	}
	got, readErr := os.ReadFile(claudeConfig)
	if readErr != nil || !bytes.Equal(got, claudeOriginal) {
		t.Fatalf("Claude config changed after Codex preflight failure: err=%v bytes=%q", readErr, got)
	}
}

func TestBackupIfExistsUsesExclusiveSameSecondName(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "settings.json")
	original := []byte(`{"keep":true}`)
	mustWrite(t, src, original)
	occupant := []byte("occupant\n")

	for attempt := 0; attempt < 8; attempt++ {
		ts := time.Now().UTC().Format("20060102T150405Z")
		planted := fmt.Sprintf("%s.bak-%s", src, ts)
		mustWrite(t, planted, occupant)
		if time.Now().UTC().Format("20060102T150405Z") != ts {
			continue
		}
		backup, err := backupIfExists(src)
		if err != nil {
			t.Fatalf("backupIfExists() error = %v", err)
		}
		if backup == planted {
			t.Fatal("backup reused occupied same-second name")
		}
		gotOccupant, readErr := os.ReadFile(planted)
		if readErr != nil || !bytes.Equal(gotOccupant, occupant) {
			t.Fatalf("occupied backup changed: err=%v bytes=%q", readErr, gotOccupant)
		}
		got, readErr := os.ReadFile(backup)
		if readErr != nil || !bytes.Equal(got, original) {
			t.Fatalf("exclusive backup: err=%v bytes=%q", readErr, got)
		}
		return
	}
	t.Fatal("could not plant a same-second backup name")
}

func TestNormalizeOptionsTimeoutBoundary(t *testing.T) {
	dir := t.TempDir()
	base := Options{
		Agent:        AgentBoth,
		ScriptPath:   filepath.Join(dir, "hook.sh"),
		BinaryPath:   writeExecutable(t, filepath.Join(dir, "amq-keepalive")),
		ClaudeConfig: filepath.Join(dir, "claude.json"),
		CodexConfig:  filepath.Join(dir, "codex.json"),
	}
	below := base
	below.Timeout = time.Second - time.Millisecond
	if _, err := NormalizeOptions(below); err == nil {
		t.Fatal("NormalizeOptions accepted timeout below 1s")
	}
	atBoundary := base
	atBoundary.Timeout = time.Second
	if _, err := NormalizeOptions(atBoundary); err != nil {
		t.Fatalf("NormalizeOptions rejected 1s timeout: %v", err)
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
	sleepLog := filepath.Join(dir, "sleep.log")
	sleepPath := writeExecutableBody(t, filepath.Join(dir, "sleep"), `#!/bin/sh
printf '%s\n' "$1" >> "$AMQ_KEEPALIVE_SLEEP_LOG"
`)
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), "#!/bin/sh\nsleep 30\n")
	logPath := filepath.Join(dir, "session-start.log")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_LOG="+logPath,
		"AMQ_KEEPALIVE_TARGET=ghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D",
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=0",
		"AMQ_KEEPALIVE_DEFAULT_TIMEOUT_SECONDS=1",
		"AMQ_KEEPALIVE_STDIN_TIMEOUT_SECONDS=1",
		"AMQ_KEEPALIVE_SLEEP="+sleepPath,
		"AMQ_KEEPALIVE_SLEEP_LOG="+sleepLog,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
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
	sleepData, err := os.ReadFile(sleepLog)
	if err != nil {
		t.Fatalf("read sleep log: %v", err)
	}
	if got := strings.Split(strings.TrimSpace(string(sleepData)), "\n")[0]; got != "1" {
		t.Fatalf("watchdog sleep arg = %q, want normalized 1s", sleepData)
	}
}

func TestSessionStartWatchdogSleepsNormalizedTimeout(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeSessionStartScript(t, dir)
	sleepLog := filepath.Join(dir, "sleep.log")
	sleepPath := writeExecutableBody(t, filepath.Join(dir, "sleep"), `#!/bin/sh
printf '%s\n' "$1" >> "$AMQ_KEEPALIVE_SLEEP_LOG"
`)
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), "#!/bin/sh\nsleep 30\n")
	logPath := filepath.Join(dir, "session-start.log")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_LOG="+logPath,
		"AMQ_KEEPALIVE_TARGET=ghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D",
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=2",
		"AMQ_KEEPALIVE_SLEEP="+sleepPath,
		"AMQ_KEEPALIVE_SLEEP_LOG="+sleepLog,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v", err)
	}
	if got := stdout.String(); got != "{}\n" {
		t.Fatalf("stdout = %q, want empty hook response", got)
	}
	sleepData, err := os.ReadFile(sleepLog)
	if err != nil {
		t.Fatalf("read sleep log: %v (watchdog never slept)", err)
	}
	if got := strings.Split(strings.TrimSpace(string(sleepData)), "\n")[0]; got != "2" {
		t.Fatalf("watchdog sleep arg = %q, want 2", sleepData)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "reattach timed out after 2s") {
		t.Fatalf("missing timeout log:\n%s", logData)
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

	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v", err)
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
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=5",
		"AMQ_KEEPALIVE_WAKE_TIMEOUT_MILLISECONDS=1000",
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
		"--retire-detached\n",
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

func TestSessionStartScriptSkipsTargetlessGhostty(t *testing.T) {
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
	if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
		t.Fatalf("targetless Ghostty invoked binary: stat error = %v", err)
	}
	logData, err := os.ReadFile(filepath.Join(dir, "session-start.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "skip: no exact terminal target adapter=ghostty") {
		t.Fatalf("missing targetless skip log:\n%s", logData)
	}
}

func TestSessionStartScriptSkipsClearAndCompactSources(t *testing.T) {
	for _, source := range []string{"clear", "compact"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			scriptPath := writeSessionStartScript(t, dir)
			argsPath := filepath.Join(dir, "args.log")
			logPath := filepath.Join(dir, "session-start.log")
			binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), `#!/bin/sh
printf '%s\n' "$@" > "$AMQ_KEEPALIVE_CAPTURE"
`)

			cmd := exec.Command("bash", scriptPath)
			cmd.Stdin = strings.NewReader(`{"source":"` + source + `"}` + "\n")
			cmd.Env = append(os.Environ(),
				"AMQ_KEEPALIVE_BIN="+binaryPath,
				"AMQ_KEEPALIVE_CAPTURE="+argsPath,
				"AMQ_KEEPALIVE_LOG="+logPath,
				"AMQ_KEEPALIVE_TARGET=ghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D",
			)
			if err := cmd.Run(); err != nil {
				t.Fatalf("hook run error = %v", err)
			}
			if _, err := os.Stat(argsPath); !os.IsNotExist(err) {
				t.Fatalf("source %q invoked binary: stat error = %v", source, err)
			}
			logData, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read log: %v", err)
			}
			if !strings.Contains(string(logData), "skip: SessionStart source="+source) {
				t.Fatalf("missing source skip log:\n%s", logData)
			}
		})
	}
}

func TestSessionStartScriptUsesExplicitGhosttyTarget(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeSessionStartScript(t, dir)
	argsPath := filepath.Join(dir, "args.log")
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), `#!/bin/sh
printf '%s\n' "$@" > "$AMQ_KEEPALIVE_CAPTURE"
`)

	cmd := exec.Command("bash", scriptPath)
	cmd.Stdin = strings.NewReader(`{"source":"startup"}` + "\n")
	cmd.Env = append(os.Environ(),
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_CAPTURE="+argsPath,
		"AMQ_KEEPALIVE_LOG="+filepath.Join(dir, "session-start.log"),
		"AMQ_KEEPALIVE_ADAPTER=ghostty",
		"AMQ_KEEPALIVE_TARGET=ghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D",
	)
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v", err)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	argsText := string(argsData)
	for _, want := range []string{
		"--adapter\nghostty\n",
		"--retire-detached\n",
		"--target\nghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D\n",
	} {
		if !strings.Contains(argsText, want) {
			t.Fatalf("args missing %q:\n%s", want, argsText)
		}
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
		"AMQ_KEEPALIVE_TARGET=ghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D",
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=3",
		"AMQ_KEEPALIVE_WAKE_TIMEOUT_MILLISECONDS=3000",
	)
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v", err)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if !strings.Contains(string(argsData), "--wake-ready-timeout\n2500ms\n") {
		t.Fatalf("inner timeout was not clamped below outer watchdog:\n%s", argsData)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "wake timeout 3000ms must be shorter than outer 3000ms; using 2500ms") {
		t.Fatalf("clamp was not logged:\n%s", logData)
	}
}

func TestSessionStartTimeoutMarkerDoesNotFollowPIDSymlink(t *testing.T) {
	dir := t.TempDir()
	tmpdir := t.TempDir()
	scriptPath := writeSessionStartScript(t, dir)
	victimPath := filepath.Join(dir, "victim")
	victimBytes := []byte("do-not-truncate\n")
	mustWrite(t, victimPath, victimBytes)
	startedPath := filepath.Join(dir, "started")
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), `#!/bin/sh
printf 'started\n' > "$AMQ_KEEPALIVE_STARTED"
sleep 30
`)
	logPath := filepath.Join(dir, "session-start.log")

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", scriptPath)
	cmd.Stdin = strings.NewReader("{}\n")
	cmd.Env = append(withoutEnv(os.Environ(),
		"AMQ_KEEPALIVE_ADAPTER",
		"AMQ_KEEPALIVE_TARGET",
		"AMQ_KEEPALIVE_BIN",
		"AMQ_KEEPALIVE_SLEEP",
		"AMQ_KEEPALIVE_DISABLED",
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS",
		"AMQ_KEEPALIVE_DEFAULT_TIMEOUT_SECONDS",
		"AMQ_KEEPALIVE_STDIN_TIMEOUT_SECONDS",
		"AMQ_KEEPALIVE_WAKE_TIMEOUT_MILLISECONDS",
		"CMUX_SURFACE_ID",
		"TMPDIR",
	),
		"TMPDIR="+tmpdir,
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_LOG="+logPath,
		"AMQ_KEEPALIVE_TARGET=ghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D",
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=2",
		"AMQ_KEEPALIVE_STDIN_TIMEOUT_SECONDS=1",
		"AMQ_KEEPALIVE_STARTED="+startedPath,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("hook start error = %v", err)
	}
	waitForFile(t, startedPath, 8*time.Second)

	planted := filepath.Join(tmpdir, fmt.Sprintf("amq-keepalive-timeout.%d", cmd.Process.Pid))
	if err := os.Symlink(victimPath, planted); err != nil {
		t.Fatalf("plant pid-named symlink: %v", err)
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("hook run error = %v", err)
	}
	if got := stdout.String(); got != "{}\n" {
		t.Fatalf("stdout = %q, want empty hook response", got)
	}
	gotVictim, err := os.ReadFile(victimPath)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if !bytes.Equal(gotVictim, victimBytes) {
		t.Fatalf("victim truncated via pid-named timeout path: got %q", gotVictim)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "reattach timed out after 2s") {
		t.Fatalf("missing timeout log:\n%s", logData)
	}
}

func TestSessionStartTimeoutKillsReattachProcessGroup(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeSessionStartScript(t, dir)
	grandchildPidPath := filepath.Join(dir, "grandchild.pid")
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), `#!/bin/sh
sleep 60 &
printf '%s\n' "$!" > "$AMQ_KEEPALIVE_GRANDCHILD_PID"
wait
`)
	logPath := filepath.Join(dir, "session-start.log")

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", scriptPath)
	cmd.Stdin = strings.NewReader("{}\n")
	cmd.Env = append(withoutEnv(os.Environ(),
		"AMQ_KEEPALIVE_ADAPTER",
		"AMQ_KEEPALIVE_TARGET",
		"AMQ_KEEPALIVE_BIN",
		"AMQ_KEEPALIVE_SLEEP",
		"AMQ_KEEPALIVE_DISABLED",
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS",
		"AMQ_KEEPALIVE_DEFAULT_TIMEOUT_SECONDS",
		"AMQ_KEEPALIVE_STDIN_TIMEOUT_SECONDS",
		"AMQ_KEEPALIVE_WAKE_TIMEOUT_MILLISECONDS",
		"CMUX_SURFACE_ID",
		"TMPDIR",
	),
		"TMPDIR="+dir,
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_LOG="+logPath,
		"AMQ_KEEPALIVE_TARGET=ghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D",
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=2",
		"AMQ_KEEPALIVE_STDIN_TIMEOUT_SECONDS=1",
		"AMQ_KEEPALIVE_GRANDCHILD_PID="+grandchildPidPath,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("hook start error = %v", err)
	}
	pid := readPIDFile(t, grandchildPidPath)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("hook run error = %v", err)
	}
	if got := stdout.String(); got != "{}\n" {
		t.Fatalf("stdout = %q, want empty hook response", got)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logData), "reattach timed out after 2s") {
		t.Fatalf("missing timeout log:\n%s", logData)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processRunning(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processRunning(pid) {
		t.Fatalf("grandchild pid %d still running after process-group timeout", pid)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, "amq-keepalive-timeout.*"))
	if err != nil {
		t.Fatalf("glob timeout_dir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("timeout_dir leftover: %v", leftovers)
	}
}

func TestSessionStartSuccessDoesNotKillReadyWake(t *testing.T) {
	dir := t.TempDir()
	scriptPath := writeSessionStartScript(t, dir)
	wakePidPath := filepath.Join(dir, "wake.pid")
	binaryPath := writeExecutableBody(t, filepath.Join(dir, "amq-keepalive"), `#!/bin/sh
sleep 30 &
printf '%s\n' "$!" > "$AMQ_KEEPALIVE_WAKE_PID"
exit 0
`)
	logPath := filepath.Join(dir, "session-start.log")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", scriptPath)
	cmd.Stdin = strings.NewReader("{}\n")
	cmd.Env = append(withoutEnv(os.Environ(),
		"AMQ_KEEPALIVE_ADAPTER",
		"AMQ_KEEPALIVE_TARGET",
		"AMQ_KEEPALIVE_BIN",
		"AMQ_KEEPALIVE_SLEEP",
		"AMQ_KEEPALIVE_DISABLED",
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS",
		"AMQ_KEEPALIVE_DEFAULT_TIMEOUT_SECONDS",
		"AMQ_KEEPALIVE_STDIN_TIMEOUT_SECONDS",
		"AMQ_KEEPALIVE_WAKE_TIMEOUT_MILLISECONDS",
		"CMUX_SURFACE_ID",
		"TMPDIR",
	),
		"AMQ_KEEPALIVE_BIN="+binaryPath,
		"AMQ_KEEPALIVE_LOG="+logPath,
		"AMQ_KEEPALIVE_TARGET=ghostty:terminal:BEDE3893-CE56-4309-8AEC-3D930F11225D",
		"AMQ_KEEPALIVE_TIMEOUT_SECONDS=3",
		"AMQ_KEEPALIVE_STDIN_TIMEOUT_SECONDS=1",
		"AMQ_KEEPALIVE_WAKE_PID="+wakePidPath,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook run error = %v", err)
	}
	if got := stdout.String(); got != "{}\n" {
		t.Fatalf("stdout = %q, want empty hook response", got)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "reattach ok") {
		t.Fatalf("missing success log:\n%s", logText)
	}
	if strings.Contains(logText, "reattach timed out") {
		t.Fatalf("success path logged a timeout:\n%s", logText)
	}
	pid := readPIDFile(t, wakePidPath)
	t.Cleanup(func() { killPID(pid) })
	if !processRunning(pid) {
		t.Fatalf("already-ready wake pid %d was killed by watchdog cancel", pid)
	}
}

func waitForFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	waitForFile(t, path, 8*time.Second)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid file %s = %q, want a pid", path, data)
	}
	return pid
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

// withIsolatedHome runs fn with HOME (and USERPROFILE) pointed at a fresh temp
// dir. TestMain already isolates HOME for the whole package; this helper is for
// tests that need a SECOND, distinct sentinel HOME inside the run (the negative
// case that asserts a different home tree is byte-identical before/after).
func withIsolatedHome(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Some platforms consult USERPROFILE; set it too so the isolation is robust
	// when the suite runs under mutation testing or a cross-platform harness.
	t.Setenv("USERPROFILE", home)
	fn(t)
}

// TestInstallDoesNotTouchRealHomeWhenDefaultsResolve asserts the regression at
// the core of WP-646: when a caller omits CodexConfig (so NormalizeOptions
// resolves it via DefaultCodexConfig -> os.UserHomeDir), install writes only
// into the isolated HOME's tree and never touches a second sentinel HOME.
func TestInstallDoesNotTouchRealHomeWhenDefaultsResolve(t *testing.T) {
	// First, capture a sentinel representing "the real user home" and pre-seed
	// its ~/.codex/hooks.json so we can assert byte-identity after install.
	sentinelHome := t.TempDir()
	mustWrite(t, filepath.Join(sentinelHome, ".codex", "hooks.json"),
		[]byte(`{"hooks":{"SessionStart":[]}}`))
	before, err := os.ReadFile(filepath.Join(sentinelHome, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("read sentinel before: %v", err)
	}

	// Run install under an isolated HOME that is NOT the sentinel. Omit
	// CodexConfig so the default resolution path is exercised.
	withIsolatedHome(t, func(t *testing.T) {
		dir := t.TempDir()
		binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
		scriptPath := filepath.Join(dir, "hook.sh")
		claudeConfig := filepath.Join(dir, "settings.json")

		result, err := Install(Options{
			Agent:        AgentBoth,
			ScriptPath:   scriptPath,
			BinaryPath:   binaryPath,
			ClaudeConfig: claudeConfig,
			// CodexConfig intentionally omitted: exercises DefaultCodexConfig.
			Timeout: time.Second,
		})
		if err != nil {
			t.Fatalf("Install() error = %v", err)
		}
		// The default codex config resolved under the isolated HOME, so it must
		// exist there and contain exactly one AMQ command.
		resolvedCodex := filepath.Join(os.Getenv("HOME"), ".codex", "hooks.json")
		codexDoc := readJSON(t, resolvedCodex)
		if countCommand(codexDoc, result.Commands[AgentCodex]) != 1 {
			t.Fatalf("default CodexConfig not installed exactly once:\n%s", mustMarshal(t, codexDoc))
		}
	})

	// Negative assertion: the sentinel HOME's ~/.codex/hooks.json is byte-identical.
	after, err := os.ReadFile(filepath.Join(sentinelHome, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("read sentinel after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("sentinel ~/.codex/hooks.json was mutated by install: before=%s after=%s", before, after)
	}
}

// TestSelfHealPrunesStaleAMQHooksAndPreservesForeign is the WP-646 self-heal
// contract: a config seeded with 3 dead AMQ entries + 1 foreign entry + 1 live
// AMQ entry results, after install, in the foreign entry preserved verbatim,
// exactly one live AMQ entry, and all dead entries gone.
func TestSelfHealPrunesStaleAMQHooksAndPreservesForeign(t *testing.T) {
	withIsolatedHome(t, func(t *testing.T) {
		dir := t.TempDir()
		liveScript := filepath.Join(dir, "hooks", "amq-keepalive-session-start.sh")
		binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
		codexConfig := filepath.Join(dir, "codex", "hooks.json")

		// Build three dead AMQ commands pointing at nonexistent scripts, plus a
		// foreign entry and one live AMQ entry (script exists on disk).
		dead1 := buildHookCommand(binaryPath, "/nonexistent/gremlins-1/hook.sh", time.Second)
		dead2 := buildHookCommand(binaryPath, "/nonexistent/gremlins-2/hook.sh", time.Second)
		dead3 := buildHookCommand(binaryPath, filepath.Join(dir, "also-missing.sh"), time.Second)
		live := buildHookCommand(binaryPath, liveScript, time.Second)
		foreign := `echo "not amq"`
		seed := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[`+
			`{"type":"command","command":%q,`+`"timeout":6},`+
			`{"type":"command","command":%q,`+`"timeout":6},`+
			`{"type":"command","command":%q,`+`"timeout":6},`+
			`{"type":"command","command":%q,`+`"timeout":6},`+
			`{"type":"command","command":%q,`+`"timeout":6}`+
			`]}]}}`, dead1, dead2, dead3, foreign, live)
		mustWrite(t, codexConfig, []byte(seed))
		// Create the live script on disk so the live entry survives pruning.
		mustWrite(t, liveScript, []byte(SessionStartScript))
		if err := os.Chmod(liveScript, 0o755); err != nil {
			t.Fatalf("chmod live script: %v", err)
		}

		result, err := Install(Options{
			Agent:       AgentCodex,
			ScriptPath:  liveScript,
			BinaryPath:  binaryPath,
			CodexConfig: codexConfig,
			Timeout:     time.Second,
		})
		if err != nil {
			t.Fatalf("Install() error = %v", err)
		}

		doc := readJSON(t, codexConfig)
		// Foreign entry preserved verbatim.
		if countCommand(doc, foreign) != 1 {
			t.Fatalf("foreign hook not preserved verbatim:\n%s", mustMarshal(t, doc))
		}
		// Exactly one live AMQ entry (the install command equals the live one,
		// so self-heal keeps it and install does not append a duplicate).
		if countCommand(doc, result.Commands[AgentCodex]) != 1 {
			t.Fatalf("live AMQ command count != 1 after install:\n%s", mustMarshal(t, doc))
		}
		// All dead entries gone.
		if countCommand(doc, dead1) != 0 || countCommand(doc, dead2) != 0 || countCommand(doc, dead3) != 0 {
			t.Fatalf("dead AMQ entries survived self-heal:\n%s", mustMarshal(t, doc))
		}
	})
}

// TestSelfHealPreservesForeignWhenNoAMQEntryExists confirms the self-heal pass
// touches only AMQ-owned hooks: a foreign-only config is rewritten only by the
// normal install append, never by pruning.
func TestSelfHealPreservesForeignWhenNoAMQEntryExists(t *testing.T) {
	withIsolatedHome(t, func(t *testing.T) {
		dir := t.TempDir()
		scriptPath := filepath.Join(dir, "hook.sh")
		binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
		claudeConfig := filepath.Join(dir, "settings.json")
		foreign := `echo foreign-tool`
		mustWrite(t, claudeConfig, []byte(`{"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"`+foreign+`"}]}]}}`))

		_, err := Install(Options{
			Agent:        AgentClaude,
			ScriptPath:   scriptPath,
			BinaryPath:   binaryPath,
			ClaudeConfig: claudeConfig,
			Timeout:      time.Second,
		})
		if err != nil {
			t.Fatalf("Install() error = %v", err)
		}
		doc := readJSON(t, claudeConfig)
		if countCommand(doc, foreign) != 1 {
			t.Fatalf("foreign hook altered when no stale AMQ hook present:\n%s", mustMarshal(t, doc))
		}
	})
}

// TestStaleSessionStartHooksDetectsDeadAMQEntries exercises the read-only scan
// used by amq doctor: it reports AMQ-owned commands whose script is missing and
// ignores foreign and live entries.
func TestStaleSessionStartHooksDetectsDeadAMQEntries(t *testing.T) {
	withIsolatedHome(t, func(t *testing.T) {
		dir := t.TempDir()
		liveScript := filepath.Join(dir, "live.sh")
		mustWrite(t, liveScript, []byte("#!/bin/sh\nexit 0\n"))
		if err := os.Chmod(liveScript, 0o755); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
		dead := buildHookCommand(binaryPath, "/nonexistent/dead.sh", time.Second)
		live := buildHookCommand(binaryPath, liveScript, time.Second)
		codexConfig := filepath.Join(dir, "codex", "hooks.json")
		mustWrite(t, codexConfig, []byte(fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[`+
			`{"type":"command","command":%q,"timeout":6},`+
			`{"type":"command","command":"echo foreign","timeout":6},`+
			`{"type":"command","command":%q,"timeout":6}`+
			`]}]}}`, dead, live)))

		stale, err := StaleSessionStartHooks(codexConfig, AgentCodex)
		if err != nil {
			t.Fatalf("StaleSessionStartHooks error = %v", err)
		}
		if len(stale) != 1 {
			t.Fatalf("want 1 stale hook, got %d: %#v", len(stale), stale)
		}
		if stale[0].ScriptPath != "/nonexistent/dead.sh" {
			t.Fatalf("stale script path = %q, want /nonexistent/dead.sh", stale[0].ScriptPath)
		}
		if stale[0].Agent != AgentCodex {
			t.Fatalf("stale agent = %q, want %s", stale[0].Agent, AgentCodex)
		}
	})
}

// TestIsAMQOwnedCommandRejectsForeignEchoOfEnvVar is the R2 negative: a foreign
// hook that merely echoes the AMQ env var name (e.g. `echo AMQ_KEEPALIVE_BIN=x`)
// must NOT be classified as AMQ-owned, so it is preserved verbatim by self-heal
// and never reported stale by the doctor scan. isAMQOwnedCommand matches the
// prefix at the start of the command, not anywhere inside it.
func TestIsAMQOwnedCommandRejectsForeignEchoOfEnvVar(t *testing.T) {
	withIsolatedHome(t, func(t *testing.T) {
		dir := t.TempDir()
		scriptPath := filepath.Join(dir, "hook.sh")
		binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
		claudeConfig := filepath.Join(dir, "settings.json")
		foreign := `echo AMQ_KEEPALIVE_BIN=x`
		// Seed only the foreign echo; install must not prune it and must add the
		// real AMQ command alongside it.
		mustWrite(t, claudeConfig, []byte(`{"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":"`+foreign+`"}]}]}}`))

		result, err := Install(Options{
			Agent:        AgentClaude,
			ScriptPath:   scriptPath,
			BinaryPath:   binaryPath,
			ClaudeConfig: claudeConfig,
			Timeout:      time.Second,
		})
		if err != nil {
			t.Fatalf("Install() error = %v", err)
		}
		doc := readJSON(t, claudeConfig)
		if countCommand(doc, foreign) != 1 {
			t.Fatalf("foreign echo of env var was pruned or duplicated:\n%s", mustMarshal(t, doc))
		}
		if countCommand(doc, result.Commands[AgentClaude]) != 1 {
			t.Fatalf("AMQ command not installed exactly once:\n%s", mustMarshal(t, doc))
		}
		// Doctor scan must not report the foreign echo as stale.
		stale, err := StaleSessionStartHooks(claudeConfig, AgentClaude)
		if err != nil {
			t.Fatalf("StaleSessionStartHooks error = %v", err)
		}
		for _, s := range stale {
			if s.ScriptPath == "x" || strings.Contains(s.ScriptPath, "AMQ_KEEPALIVE_BIN") {
				t.Fatalf("foreign echo reported as stale: %#v", s)
			}
		}
	})
}

// TestSelfHealPreservesUnparseableAMQCommand is the R4 contract: an AMQ-owned
// command whose script path cannot be parsed (here: a malformed command missing
// the trailing single quote that buildHookCommand always emits) is preserved
// verbatim, never deleted. Self-heal only drops a hook when the path parsed and
// os.Stat reports it missing.
func TestSelfHealPreservesUnparseableAMQCommand(t *testing.T) {
	withIsolatedHome(t, func(t *testing.T) {
		dir := t.TempDir()
		scriptPath := filepath.Join(dir, "hook.sh")
		binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
		claudeConfig := filepath.Join(dir, "settings.json")
		// Malformed AMQ-owned command: has the prefix but no closing single quote
		// on the final token, so scriptPathFromAMQCommand cannot parse it.
		unparseable := "AMQ_KEEPALIVE_BIN='" + binaryPath + "' AMQ_KEEPALIVE_TIMEOUT_SECONDS='1' '/nonexistent/missing-trailing-quote"
		mustWrite(t, claudeConfig, []byte(fmt.Sprintf(`{"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":%q}]}]}}`, unparseable)))

		_, err := Install(Options{
			Agent:        AgentClaude,
			ScriptPath:   scriptPath,
			BinaryPath:   binaryPath,
			ClaudeConfig: claudeConfig,
			Timeout:      time.Second,
		})
		if err != nil {
			t.Fatalf("Install() error = %v", err)
		}
		doc := readJSON(t, claudeConfig)
		if countCommand(doc, unparseable) != 1 {
			t.Fatalf("unparseable AMQ-owned command was dropped by self-heal:\n%s", mustMarshal(t, doc))
		}
		// Doctor scan must not report an unparseable command as stale.
		stale, err := StaleSessionStartHooks(claudeConfig, AgentClaude)
		if err != nil {
			t.Fatalf("StaleSessionStartHooks error = %v", err)
		}
		if len(stale) != 0 {
			t.Fatalf("unparseable command reported as stale: %#v", stale)
		}
	})
}

// TestScriptPathFromAMQCommandRoundTrip is the R8 contract: for paths with
// plain bytes, spaces, embedded quotes, multibyte runes, and trailing quotes,
// scriptPathFromAMQCommand must exactly invert buildHookCommand. A command
// carrying a 4th token must not parse.
func TestScriptPathFromAMQCommandRoundTrip(t *testing.T) {
	bin := "/bin/amq-keepalive"
	cases := []string{
		"/plain/path.sh",
		"/with space/hook.sh",
		"/it's/a.sh",
		"/ünïcode/路径/hook.sh",
		"/trailing'quote/a.sh",
	}
	for _, p := range cases {
		cmd := buildHookCommand(bin, p, time.Second)
		got, ok := scriptPathFromAMQCommand(cmd)
		if !ok {
			t.Errorf("path=%q: not parseable from %q", p, cmd)
			continue
		}
		if got != p {
			t.Errorf("path=%q: decoded %q from %q", p, got, cmd)
		}
	}
	// Negative: a 4th token after the script path is not the buildHookCommand
	// shape, so it must not parse.
	extra := "AMQ_KEEPALIVE_BIN='/b' AMQ_KEEPALIVE_TIMEOUT_SECONDS='1' '/p.sh' extra"
	if _, ok := scriptPathFromAMQCommand(extra); ok {
		t.Errorf("4th-token command should not parse: %q", extra)
	}
}

// TestScriptPathFromAMQCommandRejectsNonFixedShape is the B1 contract: a command
// that has the AMQ prefix but is not the exact fixed shape buildHookCommand
// emits must NOT parse, so self-heal preserves it and the doctor does not
// report it stale. Cases: missing AMQ_KEEPALIVE_TIMEOUT_SECONDS= prefix; a
// non-digit timeout token.
func TestScriptPathFromAMQCommandRejectsNonFixedShape(t *testing.T) {
	cases := []struct {
		name    string
		command string
	}{
		{
			name:    "missing timeout prefix",
			command: "AMQ_KEEPALIVE_BIN='/b' 'not-timeout' '/dead.sh'",
		},
		{
			name:    "non-digit timeout",
			command: "AMQ_KEEPALIVE_BIN='/b' AMQ_KEEPALIVE_TIMEOUT_SECONDS='abc' '/dead.sh'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := scriptPathFromAMQCommand(tc.command); ok {
				t.Fatalf("command parsed but is not the fixed shape: %q", tc.command)
			}
		})
	}
}

// TestSelfHealPreservesNonFixedShapeAMQCommand is the B1 self-heal side: a
// non-fixed-shape owned command is preserved verbatim (not pruned) and not
// reported stale by the doctor, because it is owned-but-unparseable.
func TestSelfHealPreservesNonFixedShapeAMQCommand(t *testing.T) {
	withIsolatedHome(t, func(t *testing.T) {
		dir := t.TempDir()
		scriptPath := filepath.Join(dir, "hook.sh")
		binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
		claudeConfig := filepath.Join(dir, "settings.json")
		// Missing the AMQ_KEEPALIVE_TIMEOUT_SECONDS= prefix: owned but unparseable.
		nonFixed := "AMQ_KEEPALIVE_BIN='" + binaryPath + "' 'not-timeout' '/nonexistent/dead.sh'"
		mustWrite(t, claudeConfig, []byte(fmt.Sprintf(`{"hooks":{"SessionStart":[{"matcher":"*","hooks":[{"type":"command","command":%q}]}]}}`, nonFixed)))

		_, err := Install(Options{
			Agent:        AgentClaude,
			ScriptPath:   scriptPath,
			BinaryPath:   binaryPath,
			ClaudeConfig: claudeConfig,
			Timeout:      time.Second,
		})
		if err != nil {
			t.Fatalf("Install() error = %v", err)
		}
		doc := readJSON(t, claudeConfig)
		if countCommand(doc, nonFixed) != 1 {
			t.Fatalf("non-fixed-shape owned command was pruned by self-heal:\n%s", mustMarshal(t, doc))
		}
		stale, err := StaleSessionStartHooks(claudeConfig, AgentClaude)
		if err != nil {
			t.Fatalf("StaleSessionStartHooks error = %v", err)
		}
		if len(stale) != 0 {
			t.Fatalf("non-fixed-shape command reported as stale: %#v", stale)
		}
	})
}

// TestSelfHealDedupKeysOnFullCommand is the B3 contract: duplicate collapse
// keys on the exact full command string, not the decoded script path. Two LIVE
// owned hooks that share a script path but differ in timeout are distinct
// commands and must both be preserved; only byte-identical commands collapse to
// one.
func TestSelfHealDedupKeysOnFullCommand(t *testing.T) {
	withIsolatedHome(t, func(t *testing.T) {
		dir := t.TempDir()
		liveScript := filepath.Join(dir, "hooks", "amq-keepalive-session-start.sh")
		binaryPath := writeExecutable(t, filepath.Join(dir, "amq-keepalive"))
		codexConfig := filepath.Join(dir, "codex", "hooks.json")
		mustWrite(t, liveScript, []byte(SessionStartScript))
		if err := os.Chmod(liveScript, 0o755); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		// Two live AMQ hooks, same script, different timeout -> distinct commands.
		cmdTimeout1 := buildHookCommand(binaryPath, liveScript, 1*time.Second)
		cmdTimeout2 := buildHookCommand(binaryPath, liveScript, 2*time.Second)
		// A byte-identical duplicate of cmdTimeout1 -> should collapse to one.
		seed := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[`+
			`{"type":"command","command":%q,"timeout":6},`+
			`{"type":"command","command":%q,"timeout":6},`+
			`{"type":"command","command":%q,"timeout":6}`+
			`]}]}}`, cmdTimeout1, cmdTimeout2, cmdTimeout1)
		mustWrite(t, codexConfig, []byte(seed))

		if _, err := Install(Options{
			Agent:       AgentCodex,
			ScriptPath:  liveScript,
			BinaryPath:  binaryPath,
			CodexConfig: codexConfig,
			Timeout:     time.Second,
		}); err != nil {
			t.Fatalf("Install() error = %v", err)
		}
		doc := readJSON(t, codexConfig)
		// Both distinct-timeout live hooks preserved.
		if countCommand(doc, cmdTimeout1) != 1 {
			t.Fatalf("timeout-1 live hook not preserved exactly once:\n%s", mustMarshal(t, doc))
		}
		if countCommand(doc, cmdTimeout2) != 1 {
			t.Fatalf("timeout-2 live hook not preserved exactly once:\n%s", mustMarshal(t, doc))
		}
	})
}
