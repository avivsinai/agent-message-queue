package launch

import (
	"strings"
	"testing"
)

// TestValidClaudeAllowedToolsGrammar exercises the single scoped-pattern
// grammar for Claude's --allowedTools (issue #648 item 2). The ACCEPT set is
// the issue's verified value-grammar probes; the REJECT set closes the prior
// strictness inversion where flag-looking values were admitted while real
// Claude tool-pattern syntax was rejected.
func TestValidClaudeAllowedToolsGrammar(t *testing.T) {
	accept := []string{
		"Bash",
		"Bash,Read",
		"Edit",
		"mcp__x__y",
		"Bash(ls)",
		"Bash(ls:*)",
		"Bash(gh pr create:*)",
		"Bash(gh pr view:*,gh pr create:*)",
		"Bash(a,b)",
		"Bash(gh pr view:*,gh pr create:*),Read",
		"Bash(git -C x:*)",
	}
	for _, value := range accept {
		t.Run("accept/"+value, func(t *testing.T) {
			if !validClaudeAllowedTools(value) {
				t.Fatalf("validClaudeAllowedTools(%q) = false, want true", value)
			}
		})
	}

	reject := []string{
		"Bash:*",
		"--dangerously-skip-permissions",
		"--verbose",
		"Bash(--dangerously-skip-permissions)",
		"Bash(--verbose)",
		"Bash(-la)",
		"Bash(",
		"Bash()",
		"Bash(a)b",
		"Bash(a\nb)",
		"Bash (ls)",
		"Bash,,Read",
		"Bash(a,(b))",
		"Bash(a,",
		"a),b",
		"",
		",Bash",
		"Bash,",
		" Bash",
		"Bash ",
		"-Bash",
		"Bash(a(b)c)",
		"Bash(a)b(c)",
		"Bash( ls)",
		"Bash(ls )",
		"Bash\x00ls",
		"Bash(ls\x00)",
		"Bash(ls\ta)",
		"Bash(a\tb)",
		"Bash(\x01)",
		strings.Repeat("Bash", 200),
	}
	for _, value := range reject {
		t.Run("reject/"+value, func(t *testing.T) {
			if validClaudeAllowedTools(value) {
				t.Fatalf("validClaudeAllowedTools(%q) = true, want false", value)
			}
		})
	}
}

// TestValidClaudeAllowedToolsScopedListAcceptsRealisticEntries confirms the
// length cap (512) admits a realistic scoped list that the previous 128-byte
// cap rejected, forcing a consumer to widen to blanket Bash.
func TestValidClaudeAllowedToolsScopedListAcceptsRealisticEntries(t *testing.T) {
	value := "Bash(gh pr create:*),Read,Edit"
	if !validClaudeAllowedTools(value) {
		t.Fatalf("validClaudeAllowedTools(%q) = false, want true", value)
	}
	if len(value) > claudeAllowedToolsMaxBytes {
		t.Fatalf("test value exceeds cap: %d > %d", len(value), claudeAllowedToolsMaxBytes)
	}
}
