package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	defaultCoopRoot   = ".agent-mail"
	defaultCoopAgents = "claude,codex"
)

func runCoop(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printCoopUsage()
	}

	switch args[0] {
	case "init":
		return runCoopInit(args[1:])
	case "shell":
		return runCoopShell(args[1:])
	case "start":
		return runCoopStart(args[1:])
	default:
		return fmt.Errorf("unknown coop subcommand: %s", args[0])
	}
}

func printCoopUsage() error {
	lines := []string{
		"amq coop - simplified co-op mode setup",
		"",
		"Subcommands:",
		"  init   Initialize project for co-op mode (creates .amqrc and mailboxes)",
		"  shell  Output shell commands to configure terminal session",
		"  start  Initialize (if needed) and start an agent with environment configured",
		"",
		"Quick start:",
		"  amq coop start claude                              # Start Claude Code",
		"  amq coop start codex -- --dangerously-skip-permissions  # Start Codex with flags",
		"",
		"Manual setup:",
		"  amq coop init                       # Initialize with defaults (claude,codex)",
		"  eval \"$(amq coop shell --me claude)\" # Configure terminal for Claude",
		"",
		"Run 'amq coop <subcommand> --help' for details.",
	}
	for _, line := range lines {
		if err := writeStdoutLine(line); err != nil {
			return err
		}
	}
	return nil
}

func runCoopInit(args []string) error {
	fs := flag.NewFlagSet("coop init", flag.ContinueOnError)
	rootFlag := fs.String("root", defaultCoopRoot, "Root directory for the queue")
	agentsFlag := fs.String("agents", defaultCoopAgents, "Comma-separated agent handles")
	forceFlag := fs.Bool("force", false, "Overwrite existing config if present")
	jsonFlag := fs.Bool("json", false, "Output as JSON")

	usage := usageWithFlags(fs, "amq coop init [options]",
		"Initialize a project for co-op mode with sensible defaults.",
		"",
		"Creates:",
		"  - .amqrc file with root configuration",
		"  - Mailbox directories for each agent",
		"  - Updates .gitignore (if present)",
		"",
		"Defaults:",
		fmt.Sprintf("  --root=%s  --agents=%s", defaultCoopRoot, defaultCoopAgents),
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	// Parse and validate agents
	agents, err := parseHandles(*agentsFlag)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		return UsageError("at least one agent required")
	}
	agents = dedupeStrings(agents)
	sort.Strings(agents)

	root := *rootFlag

	// Check if already initialized (search parents too)
	existing, existingErr := findAndLoadAmqrc()
	amqrcPath := ".amqrc"
	amqrcExistsInCwd := false
	if _, err := os.Stat(amqrcPath); err == nil {
		amqrcExistsInCwd = true
	}

	// Handle .amqrc detection results
	if existingErr == nil {
		cwd, _ := os.Getwd()
		if existing.Dir != cwd {
			// Found in parent directory - warn about subdirectory init
			if !*forceFlag {
				return fmt.Errorf("already initialized in parent directory %s (root=%s). Use --force to create a separate .amqrc here", existing.Dir, existing.Config.Root)
			}
		} else if existing.Config.Root != root && !*forceFlag {
			// Same directory but different root
			return fmt.Errorf(".amqrc already exists with root=%q (use --force to overwrite)", existing.Config.Root)
		}
		// Same directory, same root (or --force): continue to ensure dirs exist
	} else if existingErr != errAmqrcNotFound {
		// Parse error or read error in .amqrc - surface it unless --force
		if !*forceFlag {
			return fmt.Errorf("invalid .amqrc found: %w (use --force to overwrite)", existingErr)
		}
		// With --force, warn but continue
		_ = writeStderr("warning: %v (overwriting with --force)\n", existingErr)
	}

	// Create root directories
	if err := fsq.EnsureRootDirs(root); err != nil {
		return fmt.Errorf("failed to create root directories: %w", err)
	}

	// Create agent mailboxes
	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			return fmt.Errorf("failed to create mailbox for %s: %w", agent, err)
		}
	}

	// Write config.json only if it doesn't exist or --force is set
	cfgPath := filepath.Join(root, "meta", "config.json")
	configExists := false
	if _, err := os.Stat(cfgPath); err == nil {
		configExists = true
	}

	configWritten := false
	if !configExists || *forceFlag {
		cfg := config.Config{
			Version:    format.CurrentVersion,
			CreatedUTC: time.Now().UTC().Format(time.RFC3339),
			Agents:     agents,
		}
		if err := config.WriteConfig(cfgPath, cfg, *forceFlag); err != nil {
			return fmt.Errorf("failed to write config: %w", err)
		}
		configWritten = true
	}

	// Write .amqrc only if it doesn't exist in cwd or --force is set
	amqrcWritten := false
	if !amqrcExistsInCwd || *forceFlag {
		amqrcData := amqrc{Root: root}
		amqrcJSON, err := json.MarshalIndent(amqrcData, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal .amqrc: %w", err)
		}
		if err := os.WriteFile(amqrcPath, append(amqrcJSON, '\n'), 0644); err != nil {
			return fmt.Errorf("failed to write .amqrc: %w", err)
		}
		amqrcWritten = true
	}

	// Update .gitignore if present (only for relative paths)
	gitignorePath := ".gitignore"
	gitignoreUpdated := false
	if _, err := os.Stat(gitignorePath); err == nil {
		gitignoreUpdated = updateGitignore(gitignorePath, root)
	}

	// Output
	if *jsonFlag {
		out := struct {
			Root             string   `json:"root"`
			Agents           []string `json:"agents"`
			AmqrcWritten     bool     `json:"amqrc_written"`
			ConfigWritten    bool     `json:"config_written"`
			GitignoreUpdated bool     `json:"gitignore_updated"`
		}{
			Root:             root,
			Agents:           agents,
			AmqrcWritten:     amqrcWritten,
			ConfigWritten:    configWritten,
			GitignoreUpdated: gitignoreUpdated,
		}
		return writeJSON(os.Stdout, out)
	}

	if err := writeStdout("Initialized co-op mode:\n"); err != nil {
		return err
	}
	if err := writeStdout("  Root: %s\n", root); err != nil {
		return err
	}
	if err := writeStdout("  Agents: %s\n", strings.Join(agents, ", ")); err != nil {
		return err
	}
	if amqrcWritten {
		if err := writeStdout("  Created: .amqrc\n"); err != nil {
			return err
		}
	}
	if gitignoreUpdated {
		if err := writeStdout("  Updated: .gitignore\n"); err != nil {
			return err
		}
	}
	if err := writeStdoutLine(""); err != nil {
		return err
	}
	if err := writeStdoutLine("Next steps:"); err != nil {
		return err
	}
	if err := writeStdout("  Terminal 1: eval \"$(amq coop shell --me %s)\"\n", agents[0]); err != nil {
		return err
	}
	if len(agents) > 1 {
		if err := writeStdout("  Terminal 2: eval \"$(amq coop shell --me %s)\"\n", agents[1]); err != nil {
			return err
		}
	}
	return nil
}

func runCoopShell(args []string) error {
	fs := flag.NewFlagSet("coop shell", flag.ContinueOnError)
	meFlag := fs.String("me", "", "Agent handle (required)")
	rootFlag := fs.String("root", "", "Root directory (override auto-detection)")
	shellFlag := fs.String("shell", "sh", "Shell format: sh, bash, zsh, fish")
	wakeFlag := fs.Bool("wake", false, "Include amq wake & in output (for interactive terminals)")
	jsonFlag := fs.Bool("json", false, "Output as JSON")

	usage := usageWithFlags(fs, "amq coop shell --me <agent> [options]",
		"Output shell commands to configure a terminal session for co-op mode.",
		"",
		"Automatically detects root from .amqrc or .agent-mail/ directory.",
		"Use --root to override for isolated multi-pair setups.",
		"",
		"Usage:",
		"  eval \"$(amq coop shell --me claude)\"        # Configure for Claude",
		"  eval \"$(amq coop shell --me codex --wake)\" # Configure for Codex with notifications",
		"  eval \"$(amq coop shell --me claude --root .agent-mail/auth)\" # Isolated pair",
	)

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	if *meFlag == "" {
		return UsageError("--me is required")
	}

	// Delegate to amq env with the same flags
	// This reuses the existing env logic for root detection
	envArgs := []string{"--me", *meFlag, "--shell", *shellFlag}
	if *rootFlag != "" {
		envArgs = append(envArgs, "--root", *rootFlag)
	}
	if *wakeFlag {
		envArgs = append(envArgs, "--wake")
	}
	if *jsonFlag {
		envArgs = append(envArgs, "--json")
	}
	return runEnv(envArgs)
}

func runCoopStart(args []string) error {
	fs := flag.NewFlagSet("coop start", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "Root directory (override auto-detection)")
	noInitFlag := fs.Bool("no-init", false, "Don't auto-initialize if .amqrc is missing")
	yesFlag := fs.Bool("y", false, "Skip confirmation prompts")

	usage := usageWithFlags(fs, "amq coop start [options] <agent> [-- agent-flags...]",
		"Start an agent with co-op environment configured.",
		"",
		"Auto-initializes project if .amqrc is missing (use --no-init to disable).",
		"Arguments after -- are passed directly to the agent.",
		"",
		"Note: Options must come BEFORE the agent name.",
		"",
		"Examples:",
		"  amq coop start claude                              # Start Claude Code",
		"  amq coop start codex                               # Start Codex CLI",
		"  amq coop start claude -- --dangerously-skip-permissions",
		"  amq coop start --no-init codex -- --full-auto",
	)

	// Split at -- separator first
	// Format: [options] <agent> [-- agent-flags...]
	var agentArgs []string
	flagArgs := args
	for i, arg := range args {
		if arg == "--" {
			flagArgs = args[:i]
			agentArgs = args[i+1:]
			break
		}
	}

	// Let flag.Parse handle options; it stops at first non-flag arg
	if handled, err := parseFlags(fs, flagArgs, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	// fs.Args() contains non-flag args: should be just the agent name
	remaining := fs.Args()
	if len(remaining) == 0 {
		return UsageError("agent name required (e.g., 'claude' or 'codex')")
	}
	if len(remaining) > 1 {
		return UsageError("unexpected arguments after agent name; use -- to pass flags to agent")
	}
	agentName := strings.ToLower(remaining[0])

	// Validate agent name
	if agentName != "claude" && agentName != "codex" {
		return UsageError("agent must be 'claude' or 'codex'")
	}

	// Check for existing .amqrc
	existing, existingErr := findAndLoadAmqrc()
	root := *rootFlag

	if existingErr == errAmqrcNotFound {
		if *noInitFlag {
			return fmt.Errorf("no .amqrc found; run 'amq coop init' first or remove --no-init")
		}

		// Prompt for init unless -y
		if !*yesFlag {
			ok, err := confirmPromptYes("No .amqrc found. Initialize co-op mode in current directory?")
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("initialization cancelled")
			}
		}

		// Run init
		initArgs := []string{}
		if root != "" {
			initArgs = append(initArgs, "--root", root)
		}
		if err := runCoopInit(initArgs); err != nil {
			return fmt.Errorf("init failed: %w", err)
		}

		// Reload .amqrc after init
		existing, existingErr = findAndLoadAmqrc()
	}

	if existingErr != nil && existingErr != errAmqrcNotFound {
		return fmt.Errorf("invalid .amqrc: %w", existingErr)
	}

	// Determine root from .amqrc if not explicitly set
	if root == "" {
		if existing.Dir != "" {
			root = existing.Config.Root
			// Make root absolute if .amqrc is in parent directory
			cwd, _ := os.Getwd()
			if existing.Dir != cwd {
				root = filepath.Join(existing.Dir, root)
			}
		} else {
			root = defaultCoopRoot
		}
	}

	// Set environment variables
	if err := os.Setenv("AM_ME", agentName); err != nil {
		return fmt.Errorf("failed to set AM_ME: %w", err)
	}
	if err := os.Setenv("AM_ROOT", root); err != nil {
		return fmt.Errorf("failed to set AM_ROOT: %w", err)
	}

	// Agent binary name matches the agent name (claude or codex)
	binaryName := agentName

	// Check if binary exists
	binaryPath, err := exec.LookPath(binaryName)
	if err != nil {
		return fmt.Errorf("%s binary not found in PATH; please install it first", binaryName)
	}

	// Run agent
	cmd := exec.Command(binaryPath, agentArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Copy current environment plus our additions
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		// Don't treat exit codes as errors - the agent exited normally
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("failed to run %s: %w", binaryName, err)
	}

	return nil
}

// updateGitignore adds the root directory to .gitignore if not already present.
// Returns true if the file was updated.
// Skips absolute paths since they don't make sense in .gitignore.
func updateGitignore(path, root string) bool {
	// Skip absolute paths - they don't belong in .gitignore
	if filepath.IsAbs(root) {
		return false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	// Normalize root for gitignore (add trailing slash for directory)
	pattern := root
	if !strings.HasSuffix(pattern, "/") {
		pattern += "/"
	}

	// Check if already present
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == root || trimmed == pattern || trimmed == "/"+root || trimmed == "/"+pattern {
			return false // Already present
		}
	}

	// Append to file
	toAppend := "\n# Agent Message Queue\n" + pattern + "\n"
	if err := os.WriteFile(path, append(data, []byte(toAppend)...), 0644); err != nil {
		return false
	}
	return true
}
