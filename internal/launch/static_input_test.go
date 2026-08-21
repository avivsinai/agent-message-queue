package launch

import (
	"strings"
	"testing"
)

func TestValidateStaticProviderInput(t *testing.T) {
	tests := []struct {
		name       string
		executable string
		args       []string
		env        map[string]string
		want       string
	}{
		{name: "codex bypass", executable: "/opt/bin/codex", args: []string{"--dangerously-bypass-approvals-and-sandbox"}, env: map[string]string{"LANG": "C"}},
		{name: "claude bypass", executable: "claude", args: []string{"--dangerously-skip-permissions"}},
		{name: "claude allowed tools", executable: "claude", args: []string{"--allowedTools", "Bash,Read,Write,Edit"}},
		{name: "grok model", executable: "grok", args: []string{"--model", "grok-4.5"}},
		{name: "grok tools", executable: "grok", args: []string{"--tools", "shell,custom-provider-tool"}},
		{name: "grok disallowed tools", executable: "grok", args: []string{"--disallowed-tools", "legacy_tool"}},
		{name: "grok managed flags", executable: "grok", args: []string{"--effort", "high", "--no-auto-update", "--no-alt-screen"}},
		{name: "cursor current alias", executable: "agent", args: []string{"--model", "auto"}},
		{name: "cursor current alias exe", executable: "agent.exe", args: []string{"--model", "auto"}},
		{name: "cursor legacy alias", executable: "cursor-agent", args: []string{"--model", "auto"}},
		{name: "codex reasoning minimal", executable: "codex", args: []string{"-c", "model_reasoning_effort=minimal"}},
		{name: "codex reasoning xhigh", executable: "codex", args: []string{"-c", "model_reasoning_effort=xhigh"}},
		{name: "unknown provider", executable: "bash", want: "adapter-known"},
		{name: "provider smuggling", executable: "codex-wrapper", want: "adapter-known"},
		{name: "unknown argument", executable: "codex", args: []string{"--config", "x=y"}, want: "not allowed"},
		{name: "grok continue", executable: "grok", args: []string{"--continue"}, want: "not allowed"},
		{name: "grok always approve", executable: "grok", args: []string{"--always-approve"}, want: "not allowed"},
		{name: "grok yolo", executable: "grok", args: []string{"--yolo"}, want: "not allowed"},
		{name: "grok Claude tool alias", executable: "grok", args: []string{"--allowedTools", "Bash,Read,Write"}, want: "not allowed"},
		{name: "grok session-id", executable: "grok", args: []string{"--session-id", "01999999-9999-7999-8999-999999999999"}, want: "not allowed"},
		{name: "grok resume", executable: "grok", args: []string{"--resume", "01999999-9999-7999-8999-999999999999"}, want: "not allowed"},
		{name: "grok opaque tool name", executable: "grok", args: []string{"--tools", "run_terminal_cmd(Agent);custom"}},
		{name: "duplicate grok tools", executable: "grok", args: []string{"--tools", "shell", "--tools", "computer"}, want: "duplicated"},
		{name: "duplicate grok disallowed tools", executable: "grok", args: []string{"--disallowed-tools", "legacy_tool", "--disallowed-tools", "shell"}, want: "duplicated"},
		{name: "bypass value smuggling", executable: "codex", args: []string{"--dangerously-bypass-approvals-and-sandbox", "payload"}, want: "not allowed"},
		{name: "unknown environment", executable: "claude", env: map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/attacker"}, want: "not allowed"},
		{name: "duplicate allowed tools", executable: "claude", args: []string{"--allowedTools", "Read", "--allowedTools", "Write"}, want: "duplicated"},
		{name: "empty allowed tool", executable: "claude", args: []string{"--allowedTools", "Read,,Write"}, want: "invalid value"},
		{name: "spaced allowed tool", executable: "claude", args: []string{"--allowedTools", "Read, Write"}, want: "invalid value"},
		{name: "unknown codex key", executable: "codex", args: []string{"-c", "notify=[]"}, want: "invalid value"},
		{name: "unknown codex value", executable: "codex", args: []string{"-c", "model_reasoning_effort=ultra"}, want: "invalid value"},
		{name: "duplicate codex key", executable: "codex", args: []string{"-c", "model_reasoning_effort=low", "-c", "model_reasoning_effort=high"}, want: "duplicated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ValidateStaticProviderInput(test.executable, test.args, test.env)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateStaticProviderInput error = %v, want %q", err, test.want)
			}
		})
	}
}
