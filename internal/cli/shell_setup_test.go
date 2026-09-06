package cli

import (
	"strings"
	"testing"
)

func TestShellSnippetPosixDefaults(t *testing.T) {
	snippet := posixSnippet(defaultClaudeAlias, defaultCodexAlias, defaultGrokAlias)
	if !strings.Contains(snippet, "function amc()") {
		t.Error("posix snippet missing amc function")
	}
	if !strings.Contains(snippet, "function amx()") {
		t.Error("posix snippet missing amx function")
	}
	if !strings.Contains(snippet, "function amg()") {
		t.Error("posix snippet missing amg function")
	}
	if !strings.Contains(snippet, "--session") {
		t.Error("posix snippet missing --session flag")
	}
}

func TestShellSnippetPosixGrokIsBarePassthrough(t *testing.T) {
	snippet := posixSnippet(defaultClaudeAlias, defaultCodexAlias, defaultGrokAlias)
	if strings.Contains(snippet, "grok -- --dangerously-bypass-approvals-and-sandbox") {
		t.Error("grok function should not bake in a permission-bypass flag")
	}
	if !strings.Contains(snippet, `amq coop exec "${session_args[@]}" grok "$@"`) {
		t.Error("posix snippet missing bare grok pass-through invocation")
	}
}
