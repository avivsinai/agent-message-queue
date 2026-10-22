package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultClaudeAlias = "amc"
	defaultCodexAlias  = "amx"
	defaultGrokAlias   = "amg"
	defaultAgyAlias    = "amy"
)

func runShellSetup(args []string) error {
	fs := flag.NewFlagSet("shell-setup", flag.ContinueOnError)
	shellFlag := fs.String("shell", detectShell(), "Shell format: bash, zsh, fish")
	claudeAliasFlag := fs.String("claude-alias", defaultClaudeAlias, "Function name for Claude Code shortcut")
	codexAliasFlag := fs.String("codex-alias", defaultCodexAlias, "Function name for Codex CLI shortcut")
	grokAliasFlag := fs.String("grok-alias", defaultGrokAlias, "Function name for Grok CLI shortcut")
	agyAliasFlag := fs.String("agy-alias", defaultAgyAlias, "Function name for Antigravity CLI (agy) shortcut")

	usage := usageWithFlags(fs, "amq shell-setup [options]",
		"Outputs shell aliases for quick co-op session management.",
		"",
		"Defines four functions (names customizable via flags):",
		fmt.Sprintf("  %s [session] [flags]  → amq coop exec [--session <s>] claude [flags]", defaultClaudeAlias),
		fmt.Sprintf("  %s [session] [flags]  → amq coop exec [--session <s>] codex -- --dangerously-bypass-approvals-and-sandbox [flags]", defaultCodexAlias),
		fmt.Sprintf("  %s [session] [flags]  → amq coop exec [--session <s>] grok [flags]", defaultGrokAlias),
		fmt.Sprintf("  %s [session] [flags]  → amq coop exec [--session <s>] agy [flags]", defaultAgyAlias),
		"",
		"Usage:",
		"  eval \"$(amq shell-setup)\"           # Add to current shell (default names)",
		"  amq shell-setup --shell fish         # Fish shell output",
		"  amq shell-setup --claude-alias cc --codex-alias cx --grok-alias gk --agy-alias ag  # Custom names",
		"",
		"Examples after setup:",
		fmt.Sprintf("  %s                    # Start Claude Code (default session)", defaultClaudeAlias),
		fmt.Sprintf("  %s feature-x          # Isolated session for feature-x", defaultClaudeAlias),
		fmt.Sprintf("  %s feature-x          # Codex in same isolated session", defaultCodexAlias),
		fmt.Sprintf("  %s feature-x          # Grok in same isolated session", defaultGrokAlias),
		fmt.Sprintf("  %s feature-x          # agy in same isolated session", defaultAgyAlias),
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	shell := strings.ToLower(strings.TrimSpace(*shellFlag))
	if !isValidSetupShell(shell) {
		return UsageError("invalid shell %q (supported: bash, zsh, fish)", shell)
	}

	claudeAlias := *claudeAliasFlag
	codexAlias := *codexAliasFlag
	grokAlias := *grokAliasFlag
	agyAlias := *agyAliasFlag

	if err := validateAliasName(claudeAlias); err != nil {
		return err
	}
	if err := validateAliasName(codexAlias); err != nil {
		return err
	}
	if err := validateAliasName(grokAlias); err != nil {
		return err
	}
	if err := validateAliasName(agyAlias); err != nil {
		return err
	}

	snippet := shellSnippet(shell, claudeAlias, codexAlias, grokAlias, agyAlias)
	return writeStdout("%s", snippet)
}

func validateAliasName(name string) error {
	if name == "" {
		return UsageError("alias name cannot be empty")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return UsageError("invalid alias name %q (allowed: letters, digits, -, _)", name)
	}
	return nil
}

func shellSnippet(shell, claudeAlias, codexAlias, grokAlias, agyAlias string) string {
	switch shell {
	case "fish":
		return fishSnippet(claudeAlias, codexAlias, grokAlias, agyAlias)
	default:
		return posixSnippet(claudeAlias, codexAlias, grokAlias, agyAlias)
	}
}

func posixSnippet(claudeAlias, codexAlias, grokAlias, agyAlias string) string {
	return fmt.Sprintf(`# AMQ co-op aliases (added by amq shell-setup)
function %s() {
  local session_args=()
  if [ -n "${1:-}" ] && [ "${1#-}" = "$1" ]; then
    session_args=(--session "$1")
    shift
  fi
  amq coop exec "${session_args[@]}" claude "$@"
}
function %s() {
  local session_args=()
  if [ -n "${1:-}" ] && [ "${1#-}" = "$1" ]; then
    session_args=(--session "$1")
    shift
  fi
  amq coop exec "${session_args[@]}" codex -- --dangerously-bypass-approvals-and-sandbox "$@"
}
function %s() {
  local session_args=()
  if [ -n "${1:-}" ] && [ "${1#-}" = "$1" ]; then
    session_args=(--session "$1")
    shift
  fi
  amq coop exec "${session_args[@]}" grok "$@"
}
function %s() {
  local session_args=()
  if [ -n "${1:-}" ] && [ "${1#-}" = "$1" ]; then
    session_args=(--session "$1")
    shift
  fi
  amq coop exec "${session_args[@]}" agy "$@"
}
`, claudeAlias, codexAlias, grokAlias, agyAlias)
}

func fishSnippet(claudeAlias, codexAlias, grokAlias, agyAlias string) string {
	return fmt.Sprintf(`# AMQ co-op aliases (added by amq shell-setup)
function %s
  set -l session_args
  if test (count $argv) -gt 0; and not string match -q -- '-*' $argv[1]
    set session_args --session $argv[1]
    set -e argv[1]
  end
  amq coop exec $session_args claude $argv
end
function %s
  set -l session_args
  if test (count $argv) -gt 0; and not string match -q -- '-*' $argv[1]
    set session_args --session $argv[1]
    set -e argv[1]
  end
  amq coop exec $session_args codex -- --dangerously-bypass-approvals-and-sandbox $argv
end
function %s
  set -l session_args
  if test (count $argv) -gt 0; and not string match -q -- '-*' $argv[1]
    set session_args --session $argv[1]
    set -e argv[1]
  end
  amq coop exec $session_args grok $argv
end
function %s
  set -l session_args
  if test (count $argv) -gt 0; and not string match -q -- '-*' $argv[1]
    set session_args --session $argv[1]
    set -e argv[1]
  end
  amq coop exec $session_args agy $argv
end
`, claudeAlias, codexAlias, grokAlias, agyAlias)
}

func detectShell() string {
	shellEnv := os.Getenv("SHELL")
	base := filepath.Base(shellEnv)
	switch base {
	case "zsh":
		return "zsh"
	case "fish":
		return "fish"
	case "bash":
		return "bash"
	default:
		return "bash"
	}
}

func isValidSetupShell(shell string) bool {
	switch shell {
	case "bash", "zsh", "fish":
		return true
	default:
		return false
	}
}
