package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCheckHookConfigsFlagsStaleAMQHooks runs `amq doctor --json` under an
// isolated HOME whose ~/.codex/hooks.json contains a stale AMQ SessionStart
// hook (script path missing) and asserts the "Hook configs" check flags it as
// a warning that names the missing script and the remedy command.
func TestCheckHookConfigsFlagsStaleAMQHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	binaryPath := writeDoctorHookBinary(t, filepath.Join(home, "amq-keepalive"))
	deadScript := "/nonexistent/gremlins/TestCheckHookConfigs/hook.sh"
	deadCommand := buildAMQHookCommandForTest(t, binaryPath, deadScript)
	codexConfig := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(codexConfig), 0o700); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	seed := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": deadCommand, "timeout": 6},
						map[string]any{"type": "command", "command": "echo foreign", "timeout": 6},
					},
				},
			},
		},
	}
	data, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(codexConfig, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write codex config: %v", err)
	}

	got := checkHookConfigs()
	if got.Name != "Hook configs" {
		t.Fatalf("check name = %q, want %q", got.Name, "Hook configs")
	}
	if got.Status != "warn" {
		t.Fatalf("status = %q, want warn; message=%q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, deadScript) {
		t.Fatalf("message does not name the dead script %q: %q", deadScript, got.Message)
	}
	// R7: remedy is scoped to the agent actually found stale (codex), not always both.
	if !strings.Contains(got.Message, "install-hook --agent codex") {
		t.Fatalf("message does not name the codex-scoped remedy: %q", got.Message)
	}
	if strings.Contains(got.Message, "--agent both") {
		t.Fatalf("remedy should be scoped to codex, not both: %q", got.Message)
	}
}

// TestCheckHookConfigsCleanWhenNoStaleHooks asserts the check is ok when the
// config has only a live AMQ hook and a foreign hook (no stale entries).
func TestCheckHookConfigsCleanWhenNoStaleHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	binaryPath := writeDoctorHookBinary(t, filepath.Join(home, "amq-keepalive"))
	liveScript := filepath.Join(home, "hooks", "amq-keepalive-session-start.sh")
	if err := os.MkdirAll(filepath.Dir(liveScript), 0o700); err != nil {
		t.Fatalf("mkdir live script dir: %v", err)
	}
	if err := os.WriteFile(liveScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write live script: %v", err)
	}
	liveCommand := buildAMQHookCommandForTest(t, binaryPath, liveScript)
	codexConfig := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(codexConfig), 0o700); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	seed := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": liveCommand, "timeout": 6},
						map[string]any{"type": "command", "command": "echo foreign", "timeout": 6},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(codexConfig, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write codex config: %v", err)
	}

	got := checkHookConfigs()
	if got.Status != "ok" {
		t.Fatalf("status = %q, want ok; message=%q", got.Status, got.Message)
	}
}

// TestDoctorJSONIncludesHookConfigsCheck asserts the check appears in the full
// `amq doctor --json` output next to the skill checks.
func TestDoctorJSONIncludesHookConfigsCheck(t *testing.T) {
	root := healthyDoctorMailboxRoot(t, "claude")
	// Isolate HOME so checkHookConfigs does not read the real user config.
	t.Setenv("HOME", t.TempDir())
	findDoctorCheck(t, runDoctorMailboxJSON(t, root).Checks, "Hook configs")
}

// TestCheckHookConfigsSurfacesCorruptJSON is the R6 contract: a hook config
// that exists but is corrupt JSON is NOT silently skipped. The check reports a
// warning naming the path and the error. Only a missing file (os.IsNotExist)
// is treated as "hooks not installed" and skipped.
func TestCheckHookConfigsSurfacesCorruptJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	codexConfig := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(codexConfig), 0o700); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	// Corrupt JSON: unbalanced object.
	if err := os.WriteFile(codexConfig, []byte(`{"hooks":{"SessionStart":[{"hooks":[}`), 0o600); err != nil {
		t.Fatalf("write corrupt codex config: %v", err)
	}

	got := checkHookConfigs()
	if got.Name != "Hook configs" {
		t.Fatalf("check name = %q, want %q", got.Name, "Hook configs")
	}
	if got.Status != "warn" {
		t.Fatalf("status = %q, want warn; message=%q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, codexConfig) {
		t.Fatalf("message does not name the corrupt config path: %q", got.Message)
	}
	if !strings.Contains(got.Message, "codex") {
		t.Fatalf("message does not name the agent: %q", got.Message)
	}
}

// writeDoctorHookBinary writes a minimal executable at path and returns it.
func writeDoctorHookBinary(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir binary dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	return path
}

// buildAMQHookCommandForTest builds an AMQ-owned hook command identical in
// shape to the unexported hookinstall.buildHookCommand without depending on the unexported
// function from the hookinstall package.
func buildAMQHookCommandForTest(t *testing.T, binaryPath, scriptPath string) string {
	t.Helper()
	return "AMQ_KEEPALIVE_BIN='" + binaryPath + "' AMQ_KEEPALIVE_TIMEOUT_SECONDS='" +
		strconv.Itoa(int(time.Second.Seconds())) + "' '" + scriptPath + "'"
}

// TestCheckHookConfigsReportsStaleAndCorruptTogether is the R10 contract: when
// one agent's config is corrupt (a warning) and the other has dead hooks (stale
// findings), the check reports BOTH in one message rather than hiding the stale
// hooks behind the corrupt-config warning.
func TestCheckHookConfigsReportsStaleAndCorruptTogether(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Codex config: a stale AMQ hook (dead script path).
	binaryPath := writeDoctorHookBinary(t, filepath.Join(home, "amq-keepalive"))
	deadScript := "/nonexistent/r10/dead.sh"
	deadCommand := buildAMQHookCommandForTest(t, binaryPath, deadScript)
	codexConfig := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(codexConfig), 0o700); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	codexSeed := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":%q,"timeout":6}]}]}}`, deadCommand)
	if err := os.WriteFile(codexConfig, []byte(codexSeed), 0o600); err != nil {
		t.Fatalf("write codex config: %v", err)
	}

	// Claude config: corrupt JSON (a warning, not a missing file).
	claudeConfig := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudeConfig), 0o700); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if err := os.WriteFile(claudeConfig, []byte(`{"hooks":{"SessionStart":[`), 0o600); err != nil {
		t.Fatalf("write corrupt claude config: %v", err)
	}

	got := checkHookConfigs()
	if got.Status != "warn" {
		t.Fatalf("status = %q, want warn; message=%q", got.Status, got.Message)
	}
	// The stale codex finding must be present.
	if !strings.Contains(got.Message, deadScript) {
		t.Fatalf("message missing stale codex script %q: %q", deadScript, got.Message)
	}
	if !strings.Contains(got.Message, "install-hook --agent codex") {
		t.Fatalf("message missing codex-scoped remedy: %q", got.Message)
	}
	// The corrupt claude config warning must also be present.
	if !strings.Contains(got.Message, claudeConfig) {
		t.Fatalf("message missing corrupt claude config warning: %q", got.Message)
	}
}
