package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/swarm"
)

func runSwarm(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printSwarmUsage()
	}

	switch args[0] {
	case "list":
		return runSwarmList(args[1:])
	case "join":
		return runSwarmJoin(args[1:])
	case "leave":
		return runSwarmLeave(args[1:])
	case "tasks":
		return runSwarmTasks(args[1:])
	case "claim":
		return runSwarmClaim(args[1:])
	case "complete":
		return runSwarmComplete(args[1:])
	case "bridge":
		return runSwarmBridge(args[1:])
	default:
		return fmt.Errorf("unknown swarm subcommand: %s", args[0])
	}
}

func printSwarmUsage() error {
	lines := []string{
		"amq swarm - Claude Code Agent Teams integration",
		"",
		"Register external agents (Codex, etc.) in Claude Code Agent Teams",
		"and interact with the shared task list.",
		"",
		"Subcommands:",
		"  list      List discovered Agent Teams",
		"  join      Register an external agent in a team",
		"  leave     Deregister an agent from a team",
		"  tasks     List tasks from the shared task list",
		"  claim     Claim a task",
		"  complete  Mark a task as completed",
		"  bridge    Run bridge process (sync tasks → AMQ notifications)",
		"",
		"Quick start:",
		"  amq swarm list                                        # Discover teams",
		"  amq swarm join --team my-team --me codex              # Join a team",
		"  amq swarm tasks --team my-team                        # View tasks",
		"  amq swarm claim --team my-team --task t1 --me codex   # Claim work",
		"  amq swarm bridge --team my-team --me codex            # Run bridge",
		"",
		"Run 'amq swarm <subcommand> --help' for details.",
	}
	for _, line := range lines {
		if err := writeStdoutLine(line); err != nil {
			return err
		}
	}
	return nil
}

// --- list ---

func runSwarmList(args []string) error {
	fs := flag.NewFlagSet("swarm list", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Emit JSON output")

	usage := usageWithFlags(fs, "amq swarm list [options]",
		"List all discovered Claude Code Agent Teams.",
		"",
		"Scans ~/.claude/teams/ for team configurations.")

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	teams, err := swarm.DiscoverTeams()
	if err != nil {
		return err
	}

	if *jsonFlag {
		return writeJSON(os.Stdout, teams)
	}

	if len(teams) == 0 {
		return writeStdoutLine("No Agent Teams found in ~/.claude/teams/")
	}

	if err := writeStdoutLine("Agent Teams:"); err != nil {
		return err
	}
	for _, t := range teams {
		if err := writeStdout("  %s  (%d members)  %s\n", t.Name, t.MemberCount, t.ConfigPath); err != nil {
			return err
		}
	}
	return nil
}

// --- join ---

type swarmJoinOutput struct {
	Team    string `json:"team"`
	Name    string `json:"name"`
	AgentID string `json:"agent_id"`
	Type    string `json:"agent_type"`
	Joined  bool   `json:"joined"`
}

func runSwarmJoin(args []string) error {
	fs := flag.NewFlagSet("swarm join", flag.ContinueOnError)
	teamFlag := fs.String("team", "", "Team name (required)")
	meFlag := fs.String("me", defaultMe(), "Agent handle (e.g., codex)")
	typeFlag := fs.String("type", swarm.AgentTypeCodex, "Agent type (codex, external)")
	agentIDFlag := fs.String("agent-id", "", "Agent ID (auto-generated if empty)")
	jsonFlag := fs.Bool("json", false, "Emit JSON output")

	usage := usageWithFlags(fs, "amq swarm join --team <name> --me <agent> [options]",
		"Register an external agent in a Claude Code Agent Team.",
		"",
		"The agent is added to the team's config.json with the specified type.",
		"This makes the agent discoverable by the team lead and other teammates.")

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	if *teamFlag == "" {
		return UsageError("--team is required")
	}
	if err := requireMe(*meFlag); err != nil {
		return err
	}
	me, err := normalizeHandle(*meFlag)
	if err != nil {
		return err
	}

	agentID := *agentIDFlag
	if agentID == "" {
		agentID = swarm.NewExternalAgentID(me)
	}

	member := swarm.Member{
		Name:      me,
		AgentID:   agentID,
		AgentType: *typeFlag,
	}

	if err := swarm.RegisterMember(*teamFlag, member); err != nil {
		return err
	}

	if *jsonFlag {
		return writeJSON(os.Stdout, swarmJoinOutput{
			Team:    *teamFlag,
			Name:    me,
			AgentID: agentID,
			Type:    *typeFlag,
			Joined:  true,
		})
	}

	if err := writeStdout("Joined team %q as %s (id=%s, type=%s)\n", *teamFlag, me, agentID, *typeFlag); err != nil {
		return err
	}
	return nil
}

// --- leave ---

func runSwarmLeave(args []string) error {
	fs := flag.NewFlagSet("swarm leave", flag.ContinueOnError)
	teamFlag := fs.String("team", "", "Team name (required)")
	agentIDFlag := fs.String("agent-id", "", "Agent ID to remove (required)")
	jsonFlag := fs.Bool("json", false, "Emit JSON output")

	usage := usageWithFlags(fs, "amq swarm leave --team <name> --agent-id <id>",
		"Deregister an agent from a Claude Code Agent Team.")

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	if *teamFlag == "" {
		return UsageError("--team is required")
	}
	if *agentIDFlag == "" {
		return UsageError("--agent-id is required")
	}

	if err := swarm.UnregisterMember(*teamFlag, *agentIDFlag); err != nil {
		return err
	}

	if *jsonFlag {
		return writeJSON(os.Stdout, map[string]any{
			"team":     *teamFlag,
			"agent_id": *agentIDFlag,
			"removed":  true,
		})
	}

	return writeStdout("Left team %q (agent_id=%s)\n", *teamFlag, *agentIDFlag)
}

// --- tasks ---

func runSwarmTasks(args []string) error {
	fs := flag.NewFlagSet("swarm tasks", flag.ContinueOnError)
	teamFlag := fs.String("team", "", "Team name (required)")
	statusFlag := fs.String("status", "", "Filter by status: pending, in_progress, completed")
	jsonFlag := fs.Bool("json", false, "Emit JSON output")

	usage := usageWithFlags(fs, "amq swarm tasks --team <name> [options]",
		"List tasks from the shared Agent Teams task list.",
		"",
		"Tasks are stored at ~/.claude/tasks/{team-name}/.")

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	if *teamFlag == "" {
		return UsageError("--team is required")
	}

	tasks, err := swarm.ListTasks(*teamFlag)
	if err != nil {
		return err
	}

	// Filter by status if specified
	statusFilter := strings.TrimSpace(*statusFlag)
	if statusFilter != "" {
		filtered := make([]swarm.Task, 0, len(tasks))
		for _, t := range tasks {
			if t.Status == statusFilter {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}

	if *jsonFlag {
		return writeJSON(os.Stdout, tasks)
	}

	if len(tasks) == 0 {
		return writeStdoutLine("No tasks found.")
	}

	if err := writeStdout("Tasks for team %q:\n\n", *teamFlag); err != nil {
		return err
	}
	for _, t := range tasks {
		assigned := t.AssignedTo
		if assigned == "" {
			assigned = "(unassigned)"
		}
		if err := writeStdout("  [%s] %s  %s  assigned=%s\n", t.Status, t.ID, t.Title, assigned); err != nil {
			return err
		}
	}
	return nil
}

// --- claim ---

func runSwarmClaim(args []string) error {
	fs := flag.NewFlagSet("swarm claim", flag.ContinueOnError)
	teamFlag := fs.String("team", "", "Team name (required)")
	taskFlag := fs.String("task", "", "Task ID to claim (required)")
	meFlag := fs.String("me", defaultMe(), "Agent handle")
	agentIDFlag := fs.String("agent-id", "", "Agent ID to use as assigned_to (auto-detect from team config)")
	jsonFlag := fs.Bool("json", false, "Emit JSON output")

	usage := usageWithFlags(fs, "amq swarm claim --team <name> --task <id> --me <agent>",
		"Claim a task from the shared task list.",
		"",
		"Sets the task status to in_progress and assigns it to the agent.",
		"Uses agent_id from the team config as assigned_to for CC interop.")

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	if *teamFlag == "" {
		return UsageError("--team is required")
	}
	if *taskFlag == "" {
		return UsageError("--task is required")
	}
	if err := requireMe(*meFlag); err != nil {
		return err
	}
	me, err := normalizeHandle(*meFlag)
	if err != nil {
		return err
	}

	assignee, err := resolveAgentID(*agentIDFlag, *teamFlag, me)
	if err != nil {
		return err
	}

	if err := swarm.ClaimTask(*teamFlag, *taskFlag, assignee); err != nil {
		return err
	}

	if *jsonFlag {
		return writeJSON(os.Stdout, map[string]any{
			"team":        *teamFlag,
			"task":        *taskFlag,
			"assigned_to": assignee,
			"status":      swarm.TaskStatusInProgress,
			"claimed":     true,
		})
	}

	return writeStdout("Claimed task %q in team %q (assigned to %s)\n", *taskFlag, *teamFlag, assignee)
}

// --- complete ---

func runSwarmComplete(args []string) error {
	fs := flag.NewFlagSet("swarm complete", flag.ContinueOnError)
	teamFlag := fs.String("team", "", "Team name (required)")
	taskFlag := fs.String("task", "", "Task ID to complete (required)")
	meFlag := fs.String("me", defaultMe(), "Agent handle")
	agentIDFlag := fs.String("agent-id", "", "Agent ID (auto-detect from team config)")
	jsonFlag := fs.Bool("json", false, "Emit JSON output")

	usage := usageWithFlags(fs, "amq swarm complete --team <name> --task <id> --me <agent>",
		"Mark a task as completed in the shared task list.")

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	if *teamFlag == "" {
		return UsageError("--team is required")
	}
	if *taskFlag == "" {
		return UsageError("--task is required")
	}
	if err := requireMe(*meFlag); err != nil {
		return err
	}
	me, err := normalizeHandle(*meFlag)
	if err != nil {
		return err
	}

	assignee, err := resolveAgentID(*agentIDFlag, *teamFlag, me)
	if err != nil {
		return err
	}

	if err := swarm.CompleteTask(*teamFlag, *taskFlag, assignee); err != nil {
		return err
	}

	if *jsonFlag {
		return writeJSON(os.Stdout, map[string]any{
			"team":   *teamFlag,
			"task":   *taskFlag,
			"status": swarm.TaskStatusCompleted,
		})
	}

	return writeStdout("Completed task %q in team %q\n", *taskFlag, *teamFlag)
}

// resolveAgentID returns the explicit agent ID if provided, or looks up
// the agent's ID from the team config by name. Falls back to the handle
// if the team config lookup fails (team may not exist yet).
func resolveAgentID(explicit, teamName, handle string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	cfg, err := swarm.LoadTeam(teamName)
	if err != nil {
		// Team config not readable — fall back to handle
		return handle, nil
	}
	member := cfg.FindMemberByName(handle)
	if member == nil {
		// Agent not registered in team — fall back to handle
		return handle, nil
	}
	return member.AgentID, nil
}

// --- bridge ---

func runSwarmBridge(args []string) error {
	fs := flag.NewFlagSet("swarm bridge", flag.ContinueOnError)
	common := addCommonFlags(fs)
	teamFlag := fs.String("team", "", "Team name (required)")
	agentIDFlag := fs.String("agent-id", "", "Agent Teams agent_id (auto-detect from team config)")
	pollFlag := fs.Duration("poll", 3*time.Second, "Poll interval for task changes")

	usage := usageWithFlags(fs, "amq swarm bridge --team <name> --me <agent> [options]",
		"Run the swarm bridge process.",
		"",
		"Watches the Agent Teams task list for changes relevant to the",
		"specified agent and delivers notifications via AMQ.",
		"",
		"The bridge translates between Claude Code Agent Teams' shared task",
		"list and AMQ's Maildir-based messaging, enabling Codex agents to",
		"participate in the swarm.",
		"",
		"Run this alongside the Codex agent session. Press Ctrl+C to stop.")

	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	if *teamFlag == "" {
		return UsageError("--team is required")
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}

	root := resolveRoot(common.Root)

	// Auto-detect agent_id from team config
	agentID := *agentIDFlag
	if agentID == "" {
		cfg, err := swarm.LoadTeam(*teamFlag)
		if err != nil {
			return fmt.Errorf("load team config: %w (is the agent registered? try: amq swarm join --team %s --me %s)", err, *teamFlag, me)
		}
		member := cfg.FindMemberByName(me)
		if member == nil {
			return fmt.Errorf("agent %q not found in team %q (try: amq swarm join --team %s --me %s)", me, *teamFlag, *teamFlag, me)
		}
		agentID = member.AgentID
	}

	if err := writeStdout("Starting swarm bridge for team %q (agent=%s, id=%s)\n", *teamFlag, me, agentID); err != nil {
		return err
	}
	if err := writeStdout("AMQ root: %s\n", root); err != nil {
		return err
	}
	if err := writeStdout("Poll interval: %s\n", *pollFlag); err != nil {
		return err
	}
	if err := writeStdoutLine("Press Ctrl+C to stop."); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle Ctrl+C gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = writeStderr("\nBridge shutting down...\n")
		cancel()
	}()

	cfg := swarm.BridgeConfig{
		TeamName:     *teamFlag,
		AgentHandle:  me,
		AgentID:      agentID,
		AMQRoot:      root,
		PollInterval: *pollFlag,
	}

	err = swarm.RunBridge(ctx, cfg)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
