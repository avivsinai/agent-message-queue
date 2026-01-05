package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	common := addCommonFlags(fs)
	olderFlag := fs.String("tmp-older-than", "", "Duration (e.g. 36h)")
	dryRunFlag := fs.Bool("dry-run", false, "Show what would be removed without deleting")
	yesFlag := fs.Bool("yes", false, "Skip confirmation prompt")
	usage := usageWithFlags(fs, "amq cleanup --tmp-older-than <duration> [--dry-run] [--yes] [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *olderFlag == "" {
		return errors.New("--tmp-older-than is required")
	}
	dur, err := time.ParseDuration(*olderFlag)
	if err != nil {
		return err
	}
	if dur <= 0 {
		return errors.New("--tmp-older-than must be > 0")
	}
	root := resolveRoot(common.Root)
	cutoff := time.Now().Add(-dur)

	candidates, err := fsq.FindTmpFilesOlderThan(root, cutoff)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{
				"removed":    0,
				"candidates": []string{},
				"count":      0,
			})
		}
		return writeStdoutLine("No tmp files to remove.")
	}

	if *dryRunFlag {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{
				"candidates": candidates,
				"count":      len(candidates),
			})
		}
		if err := writeStdout("Would remove %d tmp file(s).\n", len(candidates)); err != nil {
			return err
		}
		for _, path := range candidates {
			if err := writeStdout("%s\n", path); err != nil {
				return err
			}
		}
		return nil
	}

	if !*yesFlag {
		ok, err := confirmPrompt(fmt.Sprintf("Delete %d tmp file(s)?", len(candidates)))
		if err != nil {
			return err
		}
		if !ok {
			if err := writeStdoutLine("Aborted."); err != nil {
				return err
			}
			return nil
		}
	}

	removed := 0
	for _, path := range candidates {
		if err := os.Remove(path); err != nil {
			return err
		}
		removed++
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"removed": removed,
		})
	}
	if err := writeStdout("Removed %d tmp file(s).\n", removed); err != nil {
		return err
	}
	return nil
}
