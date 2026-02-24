package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func runPresence(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		printPresenceUsage()
		return nil
	}
	switch args[0] {
	case "set":
		return runPresenceSet(args[1:])
	case "list":
		return runPresenceList(args[1:])
	default:
		return fmt.Errorf("unknown presence command: %s", args[0])
	}
}

func runPresenceSet(args []string) error {
	fs := flag.NewFlagSet("presence set", flag.ContinueOnError)
	common := addCommonFlags(fs)
	statusFlag := fs.String("status", "", "Status string")
	noteFlag := fs.String("note", "", "Optional note")
	usage := usageWithFlags(fs, "amq presence set --me <agent> --status <status> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me
	root := resolveRoot(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	status := strings.TrimSpace(*statusFlag)
	if status == "" {
		return UsageError("--status is required")
	}
	p := presence.New(common.Me, status, strings.TrimSpace(*noteFlag), time.Now())
	if err := presence.Write(root, p); err != nil {
		return err
	}
	if common.JSON {
		return writeJSON(os.Stdout, p)
	}
	if err := writeStdout("Presence updated for %s\n", common.Me); err != nil {
		return err
	}
	return nil
}

func runPresenceList(args []string) error {
	fs := flag.NewFlagSet("presence list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	usage := usageWithFlags(fs, "amq presence list [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	root := resolveRoot(common.Root)

	var agents []string
	if cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json")); err == nil {
		agents = cfg.Agents
	} else {
		var listErr error
		agents, listErr = fsq.ListAgents(root)
		if listErr != nil && !os.IsNotExist(listErr) {
			return listErr
		}
	}

	items := make([]presence.Presence, 0, len(agents))
	for _, raw := range agents {
		agent, err := normalizeHandle(raw)
		if err != nil {
			if err := writeStderr("warning: skipping invalid handle %s: %v\n", raw, err); err != nil {
				return err
			}
			continue
		}
		p, err := presence.Read(root, agent)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		items = append(items, p)
	}

	if common.JSON {
		return writeJSON(os.Stdout, items)
	}
	if len(items) == 0 {
		if err := writeStdoutLine("No presence data."); err != nil {
			return err
		}
		return nil
	}
	for _, item := range items {
		if err := writeStdout("%s  %s  %s\n", item.Handle, item.Status, item.LastSeen); err != nil {
			return err
		}
		if item.Note != "" {
			if err := writeStdout("  %s\n", item.Note); err != nil {
				return err
			}
		}
	}
	return nil
}

func printPresenceUsage() {
	_ = writeStdoutLine("amq presence <command> [options]")
	_ = writeStdoutLine("")
	_ = writeStdoutLine("Commands:")
	_ = writeStdoutLine("  set   Update presence")
	_ = writeStdoutLine("  list  List presence")
}
