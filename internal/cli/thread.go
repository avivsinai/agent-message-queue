package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

func runThread(args []string) error {
	fs := flag.NewFlagSet("thread", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Thread id")
	agentsFlag := fs.String("agents", "", "Comma-separated agent handles (optional)")
	includeBody := fs.Bool("include-body", false, "Include body in output")
	limitFlag := fs.Int("limit", 0, "Limit number of messages (0 = no limit)")

	usage := usageWithFlags(fs, "amq thread --id <thread_id> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	threadID := strings.TrimSpace(*idFlag)
	if threadID == "" {
		return fmt.Errorf("--id is required")
	}
	if *limitFlag < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}
	root := resolveRoot(common.Root)

	agents, err := parseHandles(*agentsFlag)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		if cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json")); err == nil {
			agents, err = parseHandles(strings.Join(cfg.Agents, ","))
			if err != nil {
				return err
			}
		} else {
			var listErr error
			agents, listErr = fsq.ListAgents(root)
			if listErr != nil {
				return fmt.Errorf("list agents: %w", listErr)
			}
		}
	}
	if len(agents) == 0 {
		return fmt.Errorf("no agents found; provide --agents")
	}

	entries, err := thread.Collect(root, threadID, agents, *includeBody, func(path string, parseErr error) error {
		return writeStderr("warning: skipping corrupt message %s: %v\n", filepath.Base(path), parseErr)
	})
	if err != nil {
		return err
	}
	if *limitFlag > 0 && len(entries) > *limitFlag {
		entries = entries[len(entries)-*limitFlag:]
	}

	if common.JSON {
		return writeJSON(os.Stdout, entries)
	}

	for _, entry := range entries {
		subject := entry.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		if err := writeStdout("%s  %s  %s\n", entry.Created, entry.From, subject); err != nil {
			return err
		}
		if *includeBody {
			if err := writeStdoutLine(entry.Body); err != nil {
				return err
			}
			if err := writeStdoutLine("---"); err != nil {
				return err
			}
		}
	}
	return nil
}
