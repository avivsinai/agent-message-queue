package cli

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	rootFlag := fs.String("root", defaultRoot(), "Root directory for the queue")
	agentsFlag := fs.String("agents", "", "Comma-separated agent handles (required)")
	forceFlag := fs.Bool("force", false, "Overwrite existing config.json if present")

	if err := fs.Parse(args); err != nil {
		return err
	}

	agents, err := parseHandles(*agentsFlag)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		return errors.New("--agents is required")
	}

	root := filepath.Clean(*rootFlag)
	if err := fsq.EnsureRootDirs(root); err != nil {
		return err
	}

	for _, agent := range agents {
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

	if err := writeStdout("Initialized AMQ root at %s\n", root); err != nil {
		return err
	}
	return nil
}
