package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultClaudeAlias = "amc"
	defaultCodexAlias  = "amx"

	shellSetupMarker = "# AMQ co-op aliases (added by amq shell-setup)"
)

func runShellSetup(args []string) error {
	fs := flag.NewFlagSet("shell-setup", flag.ContinueOnError)
	shellFlag := fs.String("shell", detectShell(), "Shell format: bash, zsh, fish")
	installFlag := fs.Bool("install", false, "Append to shell rc file (~/.zshrc, ~/.bashrc, etc.)")
	claudeAliasFlag := fs.String("claude-alias", defaultClaudeAlias, "Function name for Claude Code shortcut")
	codexAliasFlag := fs.String("codex-alias", defaultCodexAlias, "Function name for Codex CLI shortcut")

	usage := usageWithFlags(fs, "amq shell-setup [options]",
		"Outputs shell aliases for quick co-op session management.",
		"",
		"Defines two functions (names customizable via flags or interactive prompt):",
		fmt.Sprintf("  %s [session] [flags]  → amq coop exec [--session <s>] claude [flags]", defaultClaudeAlias),
		fmt.Sprintf("  %s [session] [flags]  → amq coop exec [--session <s>] codex -- --dangerously-bypass-approvals-and-sandbox [flags]", defaultCodexAlias),
		"",
		"Usage:",
		"  eval \"$(amq shell-setup)\"           # Add to current shell (default names)",
		"  amq shell-setup --install            # Interactive install to shell rc file",
		"  amq shell-setup --shell fish         # Fish shell output",
		"  amq shell-setup --claude-alias cc --codex-alias cx  # Custom names",
		"",
		"Examples after setup:",
		fmt.Sprintf("  %s                    # Start Claude Code (default session)", defaultClaudeAlias),
		fmt.Sprintf("  %s feature-x          # Isolated session for feature-x", defaultClaudeAlias),
		fmt.Sprintf("  %s feature-x          # Codex in same isolated session", defaultCodexAlias),
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

	// Interactive 3-step prompt when --install is used and alias flags weren't explicitly set.
	if *installFlag {
		claudeSet := false
		codexSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "claude-alias" {
				claudeSet = true
			}
			if f.Name == "codex-alias" {
				codexSet = true
			}
		})

		if !claudeSet || !codexSet {
			// Step 1: Ask whether to install.
			ok, err := confirmPrompt("Install shell aliases for AMQ co-op mode (quickly start Claude/Codex in shared or isolated sessions)?")
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}

			// Step 2: Claude alias.
			if !claudeSet {
				claudeAlias, err = promptAlias("Claude co-op alias", defaultClaudeAlias)
				if err != nil {
					return err
				}
			}

			// Step 3: Codex alias.
			if !codexSet {
				codexAlias, err = promptAlias("Codex co-op alias", defaultCodexAlias)
				if err != nil {
					return err
				}
			}
		}
	}

	if err := validateAliasName(claudeAlias); err != nil {
		return err
	}
	if err := validateAliasName(codexAlias); err != nil {
		return err
	}

	snippet := shellSnippet(shell, claudeAlias, codexAlias)

	if *installFlag {
		return installToRCFile(shell, snippet, claudeAlias, codexAlias)
	}

	return writeStdout("%s", snippet)
}

// promptShellSetup runs the interactive 3-step shell-setup flow.
// Used by coop init and install.sh to offer alias setup inline.
// Returns true if aliases were installed, false if user declined.
// Skips the prompt entirely if aliases are already installed.
func promptShellSetup() bool {
	// Check if already installed in the user's rc file.
	shell := detectShell()
	rcPath := rcFilePath(shell)
	if data, err := os.ReadFile(rcPath); err == nil {
		if strings.Contains(string(data), shellSetupMarker) {
			return false // Already installed, skip prompt
		}
	}

	ok, err := confirmPrompt("Install shell aliases for AMQ co-op mode (quickly start Claude/Codex in shared or isolated sessions)?")
	if err != nil || !ok {
		return false
	}

	claudeAlias, err := promptAlias("Claude co-op alias", defaultClaudeAlias)
	if err != nil {
		return false
	}
	codexAlias, err := promptAlias("Codex co-op alias", defaultCodexAlias)
	if err != nil {
		return false
	}

	if validateAliasName(claudeAlias) != nil || validateAliasName(codexAlias) != nil {
		_ = writeStderr("warning: invalid alias name, skipping shell-setup\n")
		return false
	}

	snippet := shellSnippet(shell, claudeAlias, codexAlias)
	if err := installToRCFile(shell, snippet, claudeAlias, codexAlias); err != nil {
		_ = writeStderr("warning: shell-setup failed: %v\n", err)
		return false
	}
	return true
}

func promptAlias(label, defaultName string) (string, error) {
	if err := writeStdout("  %s [%s]: ", label, defaultName); err != nil {
		return "", err
	}

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			return defaultName, nil
		}
		return "", err
	}

	line = strings.TrimSpace(line)
	if line == "" {
		return defaultName, nil
	}
	return line, nil
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

func shellSnippet(shell, claudeAlias, codexAlias string) string {
	switch shell {
	case "fish":
		return fishSnippet(claudeAlias, codexAlias)
	default:
		return posixSnippet(claudeAlias, codexAlias)
	}
}

func posixSnippet(claudeAlias, codexAlias string) string {
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
`, claudeAlias, codexAlias)
}

func fishSnippet(claudeAlias, codexAlias string) string {
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
`, claudeAlias, codexAlias)
}

func installToRCFile(shell, snippet, claudeAlias, codexAlias string) error {
	rcPath := rcFilePath(shell)

	// Check if already installed.
	if data, err := os.ReadFile(rcPath); err == nil {
		if strings.Contains(string(data), shellSetupMarker) {
			return writeStdout("Already installed in %s\n", rcPath)
		}
	}

	f, err := os.OpenFile(rcPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", rcPath, err)
	}
	defer f.Close()

	if _, err := f.WriteString("\n" + snippet); err != nil {
		return fmt.Errorf("failed to write to %s: %w", rcPath, err)
	}

	return writeStdout("Installed aliases: %s (Claude), %s (Codex) → %s\nRestart your shell or run: source %s\n", claudeAlias, codexAlias, rcPath, rcPath)
}

func rcFilePath(shell string) string {
	home, _ := os.UserHomeDir()
	switch shell {
	case "zsh":
		return filepath.Join(home, ".zshrc")
	case "fish":
		return filepath.Join(home, ".config", "fish", "config.fish")
	default:
		return filepath.Join(home, ".bashrc")
	}
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
