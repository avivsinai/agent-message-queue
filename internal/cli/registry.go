package cli

import "fmt"

// CommandHandler is the function signature for command handlers in the registry.
type CommandHandler func([]string) error

// CommandInfo describes a top-level command or subcommand for help generation
// and future routing/completion work.
type CommandInfo struct {
	Name            string
	Summary         string
	Description     string
	LongDescription []string
	Examples        []string
	Footer          string
	Handler         CommandHandler
	Children        []CommandInfo
}

// commands is the canonical command registry. Populated in init() to avoid
// initialization cycles (group handlers reference findCommand which reads commands).
var commands []CommandInfo

func init() {
	commands = []CommandInfo{
		{Name: "init", Summary: "Initialize the queue root and agent mailboxes", Handler: runInit},
		{Name: "send", Summary: "Send a message", Handler: runSend},
		{Name: "list", Summary: "List inbox messages", Handler: runList},
		{Name: "read", Summary: "Read a message by id", Handler: runRead},
		{Name: "thread", Summary: "View a thread", Handler: runThread},
		{
			Name:        "presence",
			Summary:     "Set or list presence",
			Description: "Agent presence metadata",
			LongDescription: []string{
				"Set or inspect agent availability, status, and optional notes.",
			},
			Examples: []string{
				"amq presence set --me claude --status busy --note \"reviewing PR\"",
				"amq presence list --json",
			},
			Handler: runPresence,
			Children: []CommandInfo{
				{Name: "set", Summary: "Update presence status", Handler: runPresenceSet},
				{Name: "list", Summary: "List presence data", Handler: runPresenceList},
			},
		},
		{Name: "cleanup", Summary: "Remove stale tmp files", Handler: runCleanup},
		{Name: "watch", Summary: "Wait for new messages (uses fsnotify)", Handler: runWatch},
		{Name: "drain", Summary: "Drain new messages (read, move to cur, emit receipts)", Handler: runDrain},
		{Name: "monitor", Summary: "Combined watch+drain for co-op mode", Handler: runMonitor},
		{Name: "reply", Summary: "Reply to a message (auto thread/refs)", Handler: runReply},
		{
			Name:        "dlq",
			Summary:     "Dead letter queue management",
			Description: "Dead letter queue management",
			Handler:     runDLQ,
			Children: []CommandInfo{
				{Name: "list", Summary: "List dead-lettered messages", Handler: runDLQList},
				{Name: "read", Summary: "Read a DLQ message with failure info", Handler: runDLQRead},
				{Name: "retry", Summary: "Retry a DLQ message (move back to inbox)", Handler: runDLQRetry},
				{Name: "purge", Summary: "Permanently remove DLQ messages", Handler: runDLQPurge},
			},
		},
		{Name: "wake", Summary: "Background waker (TIOCSTI injection, experimental)", Handler: runWake},
		{Name: "upgrade", Summary: "Upgrade amq to the latest release", Handler: runUpgradeRegistry},
		{Name: "env", Summary: "Output shell commands to set environment variables", Handler: runEnv},
		{
			Name:        "coop",
			Summary:     "Co-op mode setup (init, exec)",
			Description: "Co-op mode for multi-agent collaboration",
			Examples: []string{
				"amq coop exec claude",
				"amq coop exec codex -- --dangerously-bypass-approvals-and-sandbox",
				"amq coop exec --session feature-x claude",
			},
			Handler: runCoop,
			Children: []CommandInfo{
				{Name: "init", Summary: "Initialize project for co-op mode", Handler: runCoopInit},
				{Name: "exec", Summary: "Set up co-op mode and exec into an agent", Handler: runCoopExec},
			},
		},
		{
			Name:        "swarm",
			Summary:     "Claude Code Agent Teams integration (join, tasks, bridge)",
			Description: "Claude Code Agent Teams integration",
			LongDescription: []string{
				"Register external agents and interact with the shared task list.",
			},
			Examples: []string{
				"amq swarm list",
				"amq swarm join --team my-team --me codex",
				"amq swarm bridge --team my-team --me codex",
			},
			Handler: runSwarm,
			Children: []CommandInfo{
				{Name: "list", Summary: "List discovered Agent Teams", Handler: runSwarmList},
				{Name: "join", Summary: "Register an external agent in a team", Handler: runSwarmJoin},
				{Name: "leave", Summary: "Deregister an agent from a team", Handler: runSwarmLeave},
				{Name: "tasks", Summary: "List tasks from the shared task list", Handler: runSwarmTasks},
				{Name: "claim", Summary: "Claim a task", Handler: runSwarmClaim},
				{Name: "complete", Summary: "Mark a task as completed", Handler: runSwarmComplete},
				{Name: "fail", Summary: "Mark a task as failed", Handler: runSwarmFail},
				{Name: "block", Summary: "Mark a task as blocked", Handler: runSwarmBlock},
				{Name: "bridge", Summary: "Run bridge process (sync tasks -> AMQ notifications)", Handler: runSwarmBridge},
			},
		},
		{
			Name:        "integration",
			Summary:     "Optional interoperability adapters",
			Description: "Connect AMQ to external orchestrators through lightweight adapters",
			LongDescription: []string{
				"AMQ transports messages; adapters convert external lifecycle or task events into AMQ messages.",
				"Symphony is a lightweight hook adapter. Kanban is experimental.",
			},
			Examples: []string{
				"amq integration symphony init --me claude",
				"amq integration symphony emit --event after_run --me claude",
				"amq integration kanban bridge --me claude",
			},
			Handler: runIntegration,
			Children: []CommandInfo{
				{
					Name:    "claude",
					Summary: "Claude Code session awareness (context re-injection)",
					Handler: runIntegrationClaude,
					Children: []CommandInfo{
						{Name: "context", Summary: "Emit coop session preamble for context re-injection", Handler: runClaudeContext},
					},
				},
				{
					Name:    "symphony",
					Summary: "Lightweight Symphony hook adapter",
					Handler: runIntegrationSymphony,
					Children: []CommandInfo{
						{Name: "init", Summary: "Install AMQ hooks into WORKFLOW.md", Handler: runSymphonyInit},
						{Name: "emit", Summary: "Emit a lifecycle event as an AMQ message", Handler: runSymphonyEmit},
					},
				},
				{
					Name:    "kanban",
					Summary: "Experimental Cline Kanban bridge",
					Handler: runIntegrationKanban,
					Children: []CommandInfo{
						{Name: "bridge", Summary: "Run experimental bridge (Kanban events -> AMQ messages)", Handler: runKanbanBridge},
					},
				},
			},
		},
		{
			Name:        "receipts",
			Summary:     "Message delivery receipts",
			Description: "Query and wait for message lifecycle receipts",
			Examples: []string{
				"amq receipts list --me claude --msg-id msg_001",
				"amq receipts wait --me claude --msg-id msg_001 --stage drained --timeout 60s",
			},
			Handler: runReceipts,
			Children: []CommandInfo{
				{Name: "list", Summary: "List receipts (optionally filtered)", Handler: runReceiptsList},
				{Name: "wait", Summary: "Wait for a receipt to appear", Handler: runReceiptsWait},
			},
		},
		{Name: "who", Summary: "Show sessions and agents in current project", Handler: runWho},
		{Name: "doctor", Summary: "Verify installation and configuration", Handler: runDoctor},
		{Name: "shell-setup", Summary: "Output shell aliases (amc/amx)", Handler: runShellSetup},
		// Handler is nil to avoid an init cycle (runCompletion references commands).
		// Dispatch is handled by commandHandlers in cli.go.
		{Name: "completion", Summary: "Generate shell completions (bash, zsh, fish)"},
	}
}

var usageGlobalOptions = []string{
	"  --no-update-check  Disable update check",
}

var usageEnvironment = []string{
	"  AM_ROOT             Queue root directory (from flags, env, config, or coop exec session setup)",
	"  AM_ME               Default agent handle",
	"  AMQ_GLOBAL_ROOT     Global root fallback (for agents spawned by external orchestrators)",
	"  AMQ_NO_UPDATE_CHECK  Disable update check (1/true/yes/on)",
}

func runUpgradeRegistry(args []string) error {
	return runUpgrade(args, "dev")
}

// findCommand returns the top-level command by name.
func findCommand(name string) *CommandInfo {
	for i := range commands {
		if commands[i].Name == name {
			return &commands[i]
		}
	}
	return nil
}

// findChild returns a subcommand by name under the given parent command.
func findChild(parent *CommandInfo, name string) *CommandInfo {
	if parent == nil {
		return nil
	}
	for i := range parent.Children {
		if parent.Children[i].Name == name {
			return &parent.Children[i]
		}
	}
	return nil
}

// topLevelUsageLines returns the top-level usage lines from the command registry.
func topLevelUsageLines() []string {
	lines := []string{
		"amq - agent message queue",
		"",
		"Usage:",
		"  amq <command> [options]",
		"",
		"Commands:",
	}
	lines = append(lines, commandTableLines(commands)...)
	lines = append(lines,
		"",
		"Global options:",
	)
	lines = append(lines, usageGlobalOptions...)
	lines = append(lines,
		"",
		"Environment:",
	)
	lines = append(lines, usageEnvironment...)
	lines = append(lines,
		"",
		`Use "amq <command> --help" for more information about a command.`,
	)
	return lines
}

// groupUsageLines returns registry-backed lines for a subcommand group.
func groupUsageLines(cmd *CommandInfo) ([]string, error) {
	if cmd == nil {
		return nil, fmt.Errorf("group command is required")
	}
	if len(cmd.Children) == 0 {
		return nil, fmt.Errorf("command %q has no subcommands", cmd.Name)
	}

	desc := cmd.Description
	if desc == "" {
		desc = cmd.Summary
	}

	lines := []string{
		fmt.Sprintf("amq %s - %s", cmd.Name, desc),
	}

	if len(cmd.LongDescription) > 0 {
		lines = append(lines, "")
		lines = append(lines, cmd.LongDescription...)
	}

	lines = append(lines,
		"",
		"Subcommands:",
	)
	lines = append(lines, commandTableLines(cmd.Children)...)

	if len(cmd.Examples) > 0 {
		lines = append(lines,
			"",
			"Examples:",
		)
		for _, example := range cmd.Examples {
			lines = append(lines, "  "+example)
		}
	}

	footer := cmd.Footer
	if footer == "" {
		footer = fmt.Sprintf(`Use "amq %s <subcommand> --help" for details.`, cmd.Name)
	}
	lines = append(lines,
		"",
		footer,
	)

	return lines, nil
}

// printUsageRegistry prints the top-level usage text generated from the command registry.
func printUsageRegistry() error {
	return writeLines(topLevelUsageLines())
}

// printGroupUsage renders help for a subcommand group using the registry.
func printGroupUsage(cmd *CommandInfo) error {
	lines, err := groupUsageLines(cmd)
	if err != nil {
		return err
	}
	return writeLines(lines)
}

func commandTableLines(entries []CommandInfo) []string {
	if len(entries) == 0 {
		return nil
	}
	width := 0
	for _, entry := range entries {
		if len(entry.Name) > width {
			width = len(entry.Name)
		}
	}

	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("  %-*s  %s", width, entry.Name, entry.Summary))
	}
	return lines
}

func writeLines(lines []string) error {
	for _, line := range lines {
		if err := writeStdoutLine(line); err != nil {
			return err
		}
	}
	return nil
}

// commandNames returns top-level command names in their registry order.
func commandNames() []string {
	names := make([]string, 0, len(commands))
	for _, cmd := range commands {
		names = append(names, cmd.Name)
	}
	return names
}

// childNames returns child command names in registry order.
func childNames(cmd *CommandInfo) []string {
	if cmd == nil {
		return nil
	}
	names := make([]string, 0, len(cmd.Children))
	for _, child := range cmd.Children {
		names = append(names, child.Name)
	}
	return names
}
