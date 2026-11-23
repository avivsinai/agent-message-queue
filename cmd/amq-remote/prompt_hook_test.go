package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// install-prompt-hook writes the prompt hook into the settings file of the
// home and uninstall-prompt-hook takes it out, each with its own line; an
// extra argument is a usage error that changes nothing.
func TestClaudePromptHookInstallAndUninstall(t *testing.T) {
	serviceSandbox(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := hookExecutable()
	if err != nil {
		t.Fatal(err)
	}
	settings := claude.SettingsPath(home)
	if !strings.HasPrefix(settings, home+string(filepath.Separator)) {
		t.Fatalf("settings file %s is not under the test home %s", settings, home)
	}
	rows := []struct {
		sub, line string
		state     string
	}{
		{sub: "install-prompt-hook", line: "prompt hook installed: a Claude Code session on this machine can take a link session grant while it is in a mode that asks for permissions", state: claude.HookPresent},
		{sub: "uninstall-prompt-hook", line: "prompt hook removed", state: claude.HookMissing},
	}
	for _, row := range rows {
		before, _ := claude.PromptHookStateAt(settings)
		var out bytes.Buffer
		if code := claudeSubcommand([]string{row.sub, "extra"}, strings.NewReader(""), &out); code != protocol.ExitUsage || out.Len() != 0 {
			t.Fatalf("%s extra: exit %d, printed %q; want a usage error", row.sub, code, out.String())
		}
		if state, _ := claude.PromptHookStateAt(settings); state != before {
			t.Fatalf("%s extra changed the hook: %q, was %q", row.sub, state, before)
		}
		if code := claudeSubcommand([]string{row.sub}, strings.NewReader(""), &out); code != 0 || strings.TrimRight(out.String(), "\n") != row.line {
			t.Fatalf("%s: exit %d, printed %q; want 0 and %q", row.sub, code, out.String(), row.line)
		}
		if state, err := claude.PromptHookStateAt(settings); err != nil || state != row.state {
			t.Fatalf("after %s the prompt hook is %q (%v), want %q", row.sub, state, err, row.state)
		}
		raw, err := os.ReadFile(settings)
		if err != nil {
			t.Fatal(err)
		}
		if has := strings.Contains(string(raw), "'"+exe+"' claude prompt-hook"); has != (row.state == claude.HookPresent) {
			t.Fatalf("after %s the settings file holds the prompt hook command: %v\n%s", row.sub, has, raw)
		}
	}
}
