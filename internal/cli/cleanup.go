package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runCleanup(args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)
	common := addCommonFlags(fs)
	olderFlag := fs.String("tmp-older-than", "", "Duration (e.g. 36h)")
	yesFlag := fs.Bool("yes", false, "Skip confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *olderFlag == "" {
		return errors.New("--tmp-older-than is required")
	}
	dur, err := time.ParseDuration(*olderFlag)
	if err != nil {
		return err
	}
	root := filepath.Clean(common.Root)
	cutoff := time.Now().Add(-dur)

	candidates, err := fsq.FindTmpFilesOlderThan(root, cutoff)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		if err := writeStdoutLine("No tmp files to remove."); err != nil {
			return err
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
