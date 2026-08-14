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
		{name: "unknown provider", executable: "bash", want: "adapter-known"},
		{name: "provider smuggling", executable: "codex-wrapper", want: "adapter-known"},
		{name: "unknown argument", executable: "codex", args: []string{"--config", "x=y"}, want: "not allowed"},
		{name: "bypass value smuggling", executable: "codex", args: []string{"--dangerously-bypass-approvals-and-sandbox", "payload"}, want: "not allowed"},
		{name: "unknown environment", executable: "claude", env: map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/attacker"}, want: "not allowed"},
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
