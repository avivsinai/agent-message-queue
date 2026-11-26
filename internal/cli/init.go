package cli

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/route"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	rootFlag := new(string)
	registerImplicitRootFlag(fs, rootFlag, "Root directory for the queue")
	agentsFlag := fs.String("agents", "", "Comma-separated agent handles (required)")
	forceFlag := fs.Bool("force", false, "Overwrite existing config.json if present")
	addFlag := fs.Bool("add", false, "Use with --agents: register those handles in an already-initialized root, provisioning only new mailboxes and repairing any listed mailbox that is incomplete")

	usage := usageWithFlags(fs, "amq init --root <path> --agents a,b [--force]",
		"Add handles to an initialized queue (--add is a switch used with --agents, not a handle list):",
		"  amq init --root <path> --agents c --add",
		"--add cannot be combined with --force.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *addFlag && *forceFlag {
		return UsageError("--add cannot be combined with --force")
	}

	agents, err := parseHandles(*agentsFlag)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		return UsageError("--agents is required")
	}
	agents = dedupeStrings(agents)

	// An explicit --root names the queue to create, relative to cwd. The
	// implicit default keeps the shared resolver. --add keeps the same rule:
	// an explicit relative root that is not a queue fails instead of
	// climbing to a parent queue.
	resolve := resolveRoot
	if flagWasVisited(fs, "root") {
		resolve = route.ResolveCreateRoot
	}
	root := resolve(*rootFlag)
	if *addFlag {
		return addAgentsToRoot(root, agents)
	}
	sort.Strings(agents)
	if err := fsq.EnsureRootDirs(root); err != nil {
		return err
	}

	for _, agent := range withReservedHumanHandle(agents) {
		if err := fsq.EnsureAgentDirs(root, agent); err != nil {
			return err
		}
	}

	cfgPath := filepath.Join(root, "meta", "config.json")
	cfg := config.Config{
		Version:    format.CurrentVersion,
		CreatedUTC: time.Now().UTC().Format(time.RFC3339),
		Agents:     agents,
	}
	if err := config.WriteConfig(cfgPath, cfg, *forceFlag); err != nil {
		return err
	}

	// Update .gitignore (creates if needed)
	ensureGitignore(root)

	if err := writeStdout("Initialized AMQ root at %s\n", root); err != nil {
		return err
	}
	return nil
}

// addAgentsToRoot registers agents in an existing queue in time proportional
// to the handles it provisions, not to the roster. Mailbox provisioning
// fsyncs every mailbox ancestor, so re-provisioning a large roster through
// `init --force` costs thousands of fsyncs to add one handle. A handle the
// roster already lists had its mailbox made durable before it was listed, so
// only a new handle, or a listed one whose mailbox is incomplete, is
// provisioned: under the config lock, through the pinned root (which refuses
// symlinked mailbox paths), and before the roster write names it.
func addAgentsToRoot(root string, agents []string) error {
	cfgPath := filepath.Join(root, "meta", "config.json")
	// Lstat, not a read: a FIFO at the path must not block, and a missing
	// root must not be scaffolded.
	if _, err := os.Lstat(cfgPath); errors.Is(err, fs.ErrNotExist) {
		return initAddNotFound(root, cfgPath)
	} else if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}
	added, err := config.AddAgents(root, agents, func(r *fsq.DeliveryRoot, listed func(string) bool) error {
		// The reserved human mailbox is never listed in the roster; init
		// always provisions it, so --add repairs it when incomplete.
		for _, handle := range withReservedHumanHandle(agents) {
			registered := listed(handle) || handle == reservedHumanHandle
			if registered && len(inventoryMailboxIssues(root, handle)) == 0 {
				continue
			}
			if err := r.EnsureAgentDirsSynced(handle); err != nil {
				return fmt.Errorf("provision mailbox %s: %w", handle, err)
			}
		}
		return nil
	})
	if errors.Is(err, config.ErrNoConfig) {
		return initAddNotFound(root, cfgPath)
	}
	if err != nil {
		return err
	}
	if len(added) == 0 {
		return writeStdout("Agents already registered in AMQ root at %s\n", root)
	}
	return writeStdout("Added %s to AMQ root at %s\n", strings.Join(added, ","), root)
}

func initAddNotFound(root, cfgPath string) error {
	return NotFoundError("no AMQ queue at %s (%s is missing); run amq init without --add to create it", root, cfgPath)
}
