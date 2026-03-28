package cli

import (
	"context"
	"flag"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/integration/kanban"
)

func runIntegrationKanban(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printKanbanUsage()
	}
	switch args[0] {
	case "bridge":
		return runKanbanBridge(args[1:])
	default:
		return formatUnknownSubcommand("integration kanban", args[0])
	}
}

func printKanbanUsage() error {
	lines := []string{
		"amq integration kanban - Experimental Cline Kanban bridge",
		"",
		"Subcommands:",
		"  bridge  Run websocket bridge (Kanban runtime -> AMQ messages)",
		"",
		"Warning:",
		"  Experimental adapter. Depends on a preview WebSocket surface that may change.",
		"",
		"Examples:",
		"  amq integration kanban bridge --me codex",
		"  amq integration kanban bridge --me codex --url ws://127.0.0.1:3484/api/runtime/ws",
		"  amq integration kanban bridge --me codex --workspace-id my-workspace",
		"",
		`Use "amq integration kanban <subcommand> --help" for details.`,
	}
	return writeLines(lines)
}

func runKanbanBridge(args []string) error {
	fs := flag.NewFlagSet("integration kanban bridge", flag.ContinueOnError)
	rootFlag := fs.String("root", defaultRoot(), "Root directory for the queue")
	meFlag := fs.String("me", defaultMe(), "Agent handle (or AM_ME)")
	urlFlag := fs.String("url", "ws://127.0.0.1:3484/api/runtime/ws", "Kanban runtime websocket URL")
	workspaceIDFlag := fs.String("workspace-id", "", "Workspace ID (appended as ?workspaceId=<id> query param)")
	reconnectFlag := fs.Duration("reconnect", 3*time.Second, "Reconnect delay after websocket disconnect")
	jsonFlag := fs.Bool("json", false, "Emit JSON startup info")

	usage := usageWithFlags(fs, "amq integration kanban bridge --me <agent> [options]",
		"",
		"Runs a long-lived websocket bridge from the Kanban runtime state stream",
		"to AMQ integration messages.",
		"",
		"Experimental: depends on a preview WebSocket surface and may need updates",
		"if the upstream runtime changes.",
		"",
		"The bridge emits lifecycle/handoff notifications for task session changes.",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	rootSet := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "root" {
			rootSet = true
		}
	})
	if rootSet {
		if err := guardRootOverride(*rootFlag); err != nil {
			return err
		}
	}

	if err := requireMe(*meFlag); err != nil {
		return err
	}
	me, err := normalizeHandle(*meFlag)
	if err != nil {
		return UsageError("--me: %v", err)
	}

	root := resolveRoot(*rootFlag)
	if root == "" || !dirExists(root) {
		return UsageError("AMQ root %q does not exist (run 'amq init' first)", root)
	}

	// Append workspace ID as query parameter if provided
	wsURL := *urlFlag
	if *workspaceIDFlag != "" {
		parsed, parseErr := url.Parse(wsURL)
		if parseErr != nil {
			return UsageError("invalid --url: %v", parseErr)
		}
		q := parsed.Query()
		q.Set("workspaceId", *workspaceIDFlag)
		parsed.RawQuery = q.Encode()
		wsURL = parsed.String()
	}

	cfg := kanban.BridgeConfig{
		AgentHandle:    me,
		AMQRoot:        root,
		URL:            wsURL,
		ReconnectDelay: *reconnectFlag,
	}

	if *jsonFlag {
		if err := writeJSON(os.Stdout, map[string]interface{}{
			"bridge":          "kanban",
			"agent":           me,
			"root":            root,
			"url":             cfg.URL,
			"reconnect_delay": cfg.ReconnectDelay.String(),
			"started":         true,
		}); err != nil {
			return err
		}
	} else {
		if err := writeStdout("Starting kanban bridge for %q\n", me); err != nil {
			return err
		}
		if err := writeStdout("AMQ root: %s\n", root); err != nil {
			return err
		}
		if err := writeStdout("URL: %s\n", cfg.URL); err != nil {
			return err
		}
		if err := writeStdout("Reconnect delay: %s\n", cfg.ReconnectDelay); err != nil {
			return err
		}
		if err := writeStdoutLine("Press Ctrl+C to stop."); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = writeStderr("\nKanban bridge shutting down...\n")
		cancel()
	}()

	err = kanban.RunBridge(ctx, cfg)
	if err != nil && err != context.Canceled {
		return err
	}
	return nil
}
