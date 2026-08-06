package cli

import (
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
	wakeQuarantineOlderFlag := fs.String(
		"wake-quarantine-older-than",
		"",
		"Remove preserved wake quarantine artifacts older than this duration",
	)
	dryRunFlag := fs.Bool("dry-run", false, "Show what would be removed without deleting")
	yesFlag := fs.Bool("yes", false, "Skip confirmation prompt")
	usage := usageWithFlags(
		fs,
		"amq cleanup [--tmp-older-than <duration>] [--wake-quarantine-older-than <duration>] [--dry-run] [--yes] [options]",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *olderFlag == "" && *wakeQuarantineOlderFlag == "" {
		return UsageError("one of --tmp-older-than or --wake-quarantine-older-than is required")
	}
	var tmpDuration time.Duration
	if *olderFlag != "" {
		var err error
		tmpDuration, err = time.ParseDuration(*olderFlag)
		if err != nil {
			return UsageError("--tmp-older-than: %v", err)
		}
		if tmpDuration <= 0 {
			return UsageError("--tmp-older-than must be > 0")
		}
	}
	var wakeQuarantineDuration time.Duration
	if *wakeQuarantineOlderFlag != "" {
		var err error
		wakeQuarantineDuration, err = time.ParseDuration(*wakeQuarantineOlderFlag)
		if err != nil {
			return UsageError("--wake-quarantine-older-than: %v", err)
		}
		if wakeQuarantineDuration <= 0 {
			return UsageError("--wake-quarantine-older-than must be > 0")
		}
	}
	root := resolveRoot(common.Root)
	now := time.Now()

	var tmpCandidates []string
	if *olderFlag != "" {
		var err error
		tmpCandidates, err = fsq.FindTmpFilesOlderThan(root, now.Add(-tmpDuration))
		if err != nil {
			return err
		}
	}
	var wakeQuarantineCandidates []wakeQuarantineCleanupCandidate
	if *wakeQuarantineOlderFlag != "" {
		var err error
		wakeQuarantineCandidates, err = findWakeQuarantineOlderThan(
			root,
			now.Add(-wakeQuarantineDuration),
		)
		if err != nil {
			return err
		}
	}
	candidates := append([]string{}, tmpCandidates...)
	for _, candidate := range wakeQuarantineCandidates {
		candidates = append(candidates, candidate.Path)
	}
	if len(candidates) == 0 {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{
				"removed":    0,
				"candidates": []string{},
				"count":      0,
			})
		}
		if *wakeQuarantineOlderFlag != "" {
			return writeStdoutLine("No cleanup artifacts to remove.")
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
		label := "cleanup artifact(s)"
		if len(wakeQuarantineCandidates) == 0 {
			label = "tmp file(s)"
		}
		if err := writeStdout("Would remove %d %s.\n", len(candidates), label); err != nil {
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
		ok, err := confirmPrompt(fmt.Sprintf("Delete %d cleanup artifact(s)?", len(candidates)))
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
	for _, path := range tmpCandidates {
		if err := os.Remove(path); err != nil {
			return err
		}
		removed++
	}
	for _, candidate := range wakeQuarantineCandidates {
		if err := removeWakeQuarantineCandidate(root, candidate); err != nil {
			return err
		}
		removed++
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"removed": removed,
		})
	}
	label := "cleanup artifact(s)"
	if len(wakeQuarantineCandidates) == 0 {
		label = "tmp file(s)"
	}
	if err := writeStdout("Removed %d %s.\n", removed, label); err != nil {
		return err
	}
	return nil
}
