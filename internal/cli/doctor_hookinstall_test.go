package cli

import (
	"encoding/json"
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
