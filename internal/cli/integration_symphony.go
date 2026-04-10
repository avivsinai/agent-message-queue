package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/integration/symphony"
)

// runIntegration dispatches the "integration" subcommand group.
// Task A owns the registry entry; this handler is temporary until Task A
// provides internal/cli/integration.go. Once that exists, this function
// should be removed and the dispatch logic consolidated there.
func runIntegration(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printIntegrationUsage()
	}
	switch args[0] {
	case "symphony":
		return runIntegrationSymphony(args[1:])
	case "kanban":
		return runIntegrationKanban(args[1:])
	case "claude":
		return runIntegrationClaude(args[1:])
	default:
		return formatUnknownSubcommand("integration", args[0])
	}
}

func printIntegrationUsage() error {
	lines := []string{
		"amq integration - Optional interoperability adapters",
		"",
		"Subcommands:",
		"  claude    Claude Code session awareness (context re-injection)",
		"  symphony  Lightweight Symphony hook adapter",
		"  kanban    Experimental Cline Kanban bridge",
		"",
		"AMQ's core transport is still the message. These adapters convert",
		"external lifecycle or task events into ordinary AMQ messages.",
		"",
		`Use "amq integration <subcommand> --help" for details.`,
	}
	return writeLines(lines)
}

// runIntegrationSymphony dispatches symphony subcommands.
func runIntegrationSymphony(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printSymphonyUsage()
	}
	switch args[0] {
	case "init":
		return runSymphonyInit(args[1:])
	case "emit":
		return runSymphonyEmit(args[1:])
	default:
		return formatUnknownSubcommand("integration symphony", args[0])
	}
}

func printSymphonyUsage() error {
	lines := []string{
		"amq integration symphony - Lightweight Symphony hook adapter",
		"",
		"Subcommands:",
		"  init  Patch WORKFLOW.md hooks with AMQ-managed fragment",
		"  emit  Emit an AMQ message for a symphony lifecycle event",
		"",
		"Examples:",
		"  amq integration symphony init --me codex",
		"  amq integration symphony init --me codex --check",
		"  amq integration symphony emit --event after_create --me codex",
		"",
		`Use "amq integration symphony <subcommand> --help" for details.`,
	}
	return writeLines(lines)
}

// --- symphony init ---

func runSymphonyInit(args []string) error {
	fs := flag.NewFlagSet("integration symphony init", flag.ContinueOnError)
	workflowFlag := fs.String("workflow", "WORKFLOW.md", "Path to WORKFLOW.md")
	meFlag := fs.String("me", defaultMe(), "Agent handle (or AM_ME)")
	rootFlag := fs.String("root", defaultRoot(), "Root directory for the queue (pinned in hooks)")
	checkFlag := fs.Bool("check", false, "Inspect without writing")
	forceFlag := fs.Bool("force", false, "Rewrite even if fragment exists")
	jsonFlag := fs.Bool("json", false, "Emit JSON output")

	usage := usageWithFlags(fs, "amq integration symphony init [--workflow <path>] --me <agent> [options]",
		"",
		"Patches WORKFLOW.md hooks section with AMQ-managed hook fragments.",
		"Use this as a small optional adapter, not a workflow control plane.",
		"",
		"The managed fragment is marked with comments:",
		"  # BEGIN AMQ MANAGED",
		"  amq integration symphony emit --event <event> --me <agent> || true",
		"  # END AMQ MANAGED",
		"",
		"Existing user hook content is preserved.",
		"Running init twice is idempotent (no duplication).",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	if err := requireMe(*meFlag); err != nil {
		return err
	}
	me, err := normalizeHandle(*meFlag)
	if err != nil {
		return UsageError("--me: %v", err)
	}

	// Resolve root for pinning in hook lines
	resolvedRoot := resolveRoot(*rootFlag)

	result, err := symphony.Init(symphony.InitOptions{
		WorkflowPath: *workflowFlag,
		Me:           me,
		Root:         resolvedRoot,
		Check:        *checkFlag,
		Force:        *forceFlag,
	})
	if err != nil {
		return err
	}

	if *jsonFlag {
		return writeJSON(os.Stdout, result)
	}

	if result.CheckOnly {
		if result.HooksFound {
			return writeStdoutLine("AMQ hooks are installed in", result.WorkflowPath)
		}
		return writeStdoutLine("AMQ hooks are NOT installed in", result.WorkflowPath)
	}

	if result.AlreadyOK {
		return writeStdoutLine("AMQ hooks already present in", result.WorkflowPath, "(use --force to rewrite)")
	}
	if result.Updated {
		return writeStdoutLine("Updated AMQ hooks in", result.WorkflowPath)
	}
	return writeStdoutLine("Installed AMQ hooks in", result.WorkflowPath)
}

// --- symphony emit ---

func runSymphonyEmit(args []string) error {
	fs := flag.NewFlagSet("integration symphony emit", flag.ContinueOnError)
	eventFlag := fs.String("event", "", "Lifecycle event: "+strings.Join(symphony.ValidEvents, ", ")+" (required)")
	meFlag := fs.String("me", defaultMe(), "Agent handle (or AM_ME)")
	rootFlag := fs.String("root", defaultRoot(), "Root directory for the queue")
	workspaceFlag := fs.String("workspace", "", "Workspace path (default: current directory)")
	identifierFlag := fs.String("identifier", "", "Workspace key (default: basename of workspace)")
	jsonFlag := fs.Bool("json", false, "Emit JSON output")

	usage := usageWithFlags(fs, "amq integration symphony emit --event <event> --me <agent> [options]",
		"",
		"Emits an AMQ message for a symphony lifecycle event.",
		"Designed to be called from WORKFLOW.md hook scripts.",
		"",
		"Events: "+strings.Join(symphony.ValidEvents, ", "),
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	if *eventFlag == "" {
		return UsageError("--event is required")
	}
	if err := requireMe(*meFlag); err != nil {
		return err
	}
	me, err := normalizeHandle(*meFlag)
	if err != nil {
		return UsageError("--me: %v", err)
	}

	root := resolveRoot(*rootFlag)
	if root == "" {
		return fmt.Errorf("AMQ root is not configured (set AM_ROOT or use --root)")
	}

	result, err := symphony.Emit(symphony.EmitOptions{
		Event:      *eventFlag,
		Me:         me,
		Root:       root,
		Workspace:  *workspaceFlag,
		Identifier: *identifierFlag,
	})
	if err != nil {
		return err
	}

	if *jsonFlag {
		return writeJSON(os.Stdout, result)
	}

	return writeStdout("Emitted %s for %s (thread=%s)\n", result.Event, result.Identifier, result.Thread)
}
