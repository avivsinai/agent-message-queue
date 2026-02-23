package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
	case "exec":
		return runCoopExec(args[1:])
	case "spec":
		return runCoopSpec(args[1:])
	default:
		return fmt.Errorf("unknown coop subcommand: %s\nRun 'amq coop --help' for usage", args[0])
	}
}

func printCoopUsage() error {
	lines := []string{
		"amq coop - co-op mode for multi-agent collaboration",
		"",
		"Subcommands:",
		"  init   Initialize project for co-op mode (creates .amqrc and mailboxes)",
		"  exec   Initialize, set env, start wake, and exec into agent (replaces process)",
		"  spec   Collaborative specification workflow",
		"",
		"Quick start:",
		"  amq coop exec claude                              # Start Claude Code",
		"  amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox  # Start Codex with flags",
		"  amq coop exec --session feature-x claude          # Isolated session",
		"",
		"Manual setup (for scripts/CI):",
		"  amq coop init                       # Initialize with defaults (claude,codex)",
		"  eval \"$(amq env --me claude)\"       # Configure terminal manually",
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
	return runCoopInitInternal(args, true)
}

func runCoopInitInternal(args []string, printNextSteps bool) error {
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

	// The root in .amqrc is the literal queue root.
	queueRoot := root

	// Create root directories
	if err := fsq.EnsureRootDirs(queueRoot); err != nil {
		return fmt.Errorf("failed to create root directories: %w", err)
	}

	// Create agent mailboxes under the session subdirectory
	for _, agent := range agents {
		if err := fsq.EnsureAgentDirs(queueRoot, agent); err != nil {
			return fmt.Errorf("failed to create mailbox for %s: %w", agent, err)
		}
	}

	// Write config.json only if it doesn't exist or --force is set
	cfgPath := filepath.Join(queueRoot, "meta", "config.json")
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

	// Update .gitignore (creates if needed, only for relative paths)
	gitignoreUpdated := ensureGitignore(root)

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
	if printNextSteps {
		if err := writeStdoutLine(""); err != nil {
			return err
		}
		if err := writeStdoutLine("Next steps:"); err != nil {
			return err
		}
		if err := writeStdout("  Terminal 1: amq coop exec %s\n", agents[0]); err != nil {
			return err
		}
		if len(agents) > 1 {
			if err := writeStdout("  Terminal 2: amq coop exec %s\n", agents[1]); err != nil {
				return err
			}
		}
		if err := writeStdoutLine(""); err != nil {
			return err
		}
		if err := writeStdoutLine("Tip: eval \"$(amq shell-setup)\" to add co-op aliases to your shell"); err != nil {
			return err
		}
	}
	return nil
}
