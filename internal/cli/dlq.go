package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runDLQ(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printDLQUsage()
	}
	switch args[0] {
	case "list":
		return runDLQList(args[1:])
	case "read":
		return runDLQRead(args[1:])
	case "retry":
		return runDLQRetry(args[1:])
	case "purge":
		return runDLQPurge(args[1:])
	default:
		return fmt.Errorf("unknown dlq subcommand: %s", args[0])
	}
}

func printDLQUsage() error {
	lines := []string{
		"amq dlq - dead letter queue management",
		"",
		"Usage:",
		"  amq dlq <command> [options]",
		"",
		"Commands:",
		"  list   List dead-lettered messages",
		"  read   Read a DLQ message with failure info",
		"  retry  Retry a DLQ message (move back to inbox)",
		"  purge  Permanently remove DLQ messages",
	}
	for _, line := range lines {
		if err := writeStdoutLine(line); err != nil {
			return err
		}
	}
	return nil
}

// dlqListItem represents a DLQ message for listing.
type dlqListItem struct {
	ID            string    `json:"id"`
	OriginalID    string    `json:"original_id"`
	OriginalFile  string    `json:"original_file"`
	FailureReason string    `json:"failure_reason"`
	FailureDetail string    `json:"failure_detail,omitempty"`
	FailureTime   string    `json:"failure_time"`
	RetryCount    int       `json:"retry_count"`
	Box           string    `json:"box"`
	Path          string    `json:"path"`
	SortKey       time.Time `json:"-"`
}

func runDLQList(args []string) error {
	fs := flag.NewFlagSet("dlq list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	newFlag := fs.Bool("new", false, "List only unread DLQ messages (dlq/new)")
	curFlag := fs.Bool("cur", false, "List only inspected DLQ messages (dlq/cur)")

	usage := usageWithFlags(fs, "amq dlq list --me <agent> [--new | --cur] [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := resolveRoot(common.Root)

	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	if *newFlag && *curFlag {
		return errors.New("use only one of --new or --cur")
	}

	var items []dlqListItem

	// Scan both or just one
	boxes := []string{"new", "cur"}
	if *newFlag {
		boxes = []string{"new"}
	} else if *curFlag {
		boxes = []string{"cur"}
	}

	for _, box := range boxes {
		var dir string
		if box == "new" {
			dir = fsq.AgentDLQNew(root, me)
		} else {
			dir = fsq.AgentDLQCur(root, me)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			env, _, err := fsq.ReadDLQEnvelope(path)
			if err != nil {
				_ = writeStderr("warning: skipping corrupt DLQ message %s: %v\n", entry.Name(), err)
				continue
			}
			item := dlqListItem{
				ID:            env.ID,
				OriginalID:    env.OriginalID,
				OriginalFile:  env.OriginalFile,
				FailureReason: env.FailureReason,
				FailureDetail: env.FailureDetail,
				FailureTime:   env.FailureTime,
				RetryCount:    env.RetryCount,
				Box:           box,
				Path:          path,
			}
			if ts, err := time.Parse(time.RFC3339, env.FailureTime); err == nil {
				item.SortKey = ts
			}
			items = append(items, item)
		}
	}

	// Sort by failure time (newest first for DLQ)
	sort.Slice(items, func(i, j int) bool {
		if !items[i].SortKey.IsZero() && !items[j].SortKey.IsZero() {
			return items[i].SortKey.After(items[j].SortKey)
		}
		return items[i].FailureTime > items[j].FailureTime
	})

	if common.JSON {
		return writeJSON(os.Stdout, items)
	}

	if len(items) == 0 {
		return writeStdoutLine("No DLQ messages.")
	}

	for _, item := range items {
		retries := ""
		if item.RetryCount > 0 {
			retries = fmt.Sprintf(" (retries: %d)", item.RetryCount)
		}
		if err := writeStdout("[%s] %s  %s  %s%s\n", item.Box, item.FailureTime, item.FailureReason, item.ID, retries); err != nil {
			return err
		}
	}
	return nil
}

// dlqReadResult represents a full DLQ message for reading.
type dlqReadResult struct {
	ID              string `json:"id"`
	OriginalID      string `json:"original_id"`
	OriginalFile    string `json:"original_file"`
	FailureReason   string `json:"failure_reason"`
	FailureDetail   string `json:"failure_detail"`
	FailureTime     string `json:"failure_time"`
	RetryCount      int    `json:"retry_count"`
	SourceDir       string `json:"source_dir"`
	Box             string `json:"box"`
	OriginalContent string `json:"original_content"`
}

func runDLQRead(args []string) error {
	fs := flag.NewFlagSet("dlq read", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "DLQ message ID to read")

	usage := usageWithFlags(fs, "amq dlq read --me <agent> --id <dlq_id> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	if *idFlag == "" {
		return errors.New("--id is required")
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := resolveRoot(common.Root)

	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return err
	}

	path, box, err := fsq.FindDLQMessage(root, me, filename)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("DLQ message not found: %s", *idFlag)
		}
		return err
	}

	env, originalContent, err := fsq.ReadDLQEnvelope(path)
	if err != nil {
		return fmt.Errorf("read DLQ message: %w", err)
	}

	// Move to cur if in new
	if box == fsq.BoxNew {
		if err := fsq.MoveDLQNewToCur(root, me, filename); err != nil {
			_ = writeStderr("warning: failed to move DLQ message to cur: %v\n", err)
		}
	}

	if common.JSON {
		result := dlqReadResult{
			ID:              env.ID,
			OriginalID:      env.OriginalID,
			OriginalFile:    env.OriginalFile,
			FailureReason:   env.FailureReason,
			FailureDetail:   env.FailureDetail,
			FailureTime:     env.FailureTime,
			RetryCount:      env.RetryCount,
			SourceDir:       env.SourceDir,
			Box:             box,
			OriginalContent: string(originalContent),
		}
		return writeJSON(os.Stdout, result)
	}

	if err := writeStdout("DLQ ID:         %s\n", env.ID); err != nil {
		return err
	}
	if err := writeStdout("Original ID:    %s\n", env.OriginalID); err != nil {
		return err
	}
	if err := writeStdout("Original File:  %s\n", env.OriginalFile); err != nil {
		return err
	}
	if err := writeStdout("Failure Reason: %s\n", env.FailureReason); err != nil {
		return err
	}
	if err := writeStdout("Failure Detail: %s\n", env.FailureDetail); err != nil {
		return err
	}
	if err := writeStdout("Failure Time:   %s\n", env.FailureTime); err != nil {
		return err
	}
	if err := writeStdout("Retry Count:    %d\n", env.RetryCount); err != nil {
		return err
	}
	if err := writeStdout("Source Dir:     %s\n", env.SourceDir); err != nil {
		return err
	}
	if err := writeStdoutLine("---"); err != nil {
		return err
	}
	return writeStdout("%s", string(originalContent))
}

func runDLQRetry(args []string) error {
	fs := flag.NewFlagSet("dlq retry", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "DLQ message ID to retry")
	allFlag := fs.Bool("all", false, "Retry all DLQ messages")
	forceFlag := fs.Bool("force", false, "Force retry even if max retries exceeded")

	usage := usageWithFlags(fs, "amq dlq retry --me <agent> --id <dlq_id> [--force] [options]",
		"Or: amq dlq retry --me <agent> --all [--force]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	if *idFlag == "" && !*allFlag {
		return errors.New("--id or --all is required")
	}
	if *idFlag != "" && *allFlag {
		return errors.New("use --id or --all, not both")
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := resolveRoot(common.Root)

	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	if *allFlag {
		return retryAllDLQ(root, me, *forceFlag, common.JSON)
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return err
	}

	if err := fsq.RetryFromDLQ(root, me, filename, *forceFlag); err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"retried": *idFlag,
		})
	}
	return writeStdout("Retried: %s\n", *idFlag)
}

func retryAllDLQ(root, me string, force, jsonOutput bool) error {
	dir := fsq.AgentDLQNew(root, me)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if jsonOutput {
				return writeJSON(os.Stdout, map[string]any{"retried": []string{}, "count": 0})
			}
			return writeStdoutLine("No DLQ messages to retry.")
		}
		return err
	}

	var retried []string
	var skipped []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		filename := entry.Name()
		if err := fsq.RetryFromDLQ(root, me, filename, force); err != nil {
			skipped = append(skipped, filename)
			_ = writeStderr("warning: %s: %v\n", filename, err)
		} else {
			retried = append(retried, strings.TrimSuffix(filename, ".md"))
		}
	}

	if jsonOutput {
		return writeJSON(os.Stdout, map[string]any{
			"retried": retried,
			"skipped": skipped,
			"count":   len(retried),
		})
	}

	if len(retried) == 0 {
		return writeStdoutLine("No DLQ messages retried.")
	}
	return writeStdout("Retried %d message(s).\n", len(retried))
}

func runDLQPurge(args []string) error {
	fs := flag.NewFlagSet("dlq purge", flag.ContinueOnError)
	common := addCommonFlags(fs)
	olderFlag := fs.String("older-than", "", "Duration (e.g. 24h) - only purge messages older than this")
	dryRunFlag := fs.Bool("dry-run", false, "Show what would be removed without deleting")
	yesFlag := fs.Bool("yes", false, "Skip confirmation prompt")

	usage := usageWithFlags(fs, "amq dlq purge --me <agent> [--older-than <duration>] [--dry-run] [--yes] [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := resolveRoot(common.Root)

	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	var cutoff time.Time
	if *olderFlag != "" {
		dur, err := time.ParseDuration(*olderFlag)
		if err != nil {
			return err
		}
		if dur <= 0 {
			return errors.New("--older-than must be > 0")
		}
		cutoff = time.Now().Add(-dur)
	}

	// Collect candidates from both new and cur
	var candidates []string
	for _, box := range []string{"new", "cur"} {
		var dir string
		if box == "new" {
			dir = fsq.AgentDLQNew(root, me)
		} else {
			dir = fsq.AgentDLQCur(root, me)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if !cutoff.IsZero() {
				env, _, err := fsq.ReadDLQEnvelope(path)
				if err != nil {
					// Can't parse, include anyway
					candidates = append(candidates, path)
					continue
				}
				failTime, err := time.Parse(time.RFC3339, env.FailureTime)
				if err != nil || failTime.After(cutoff) {
					continue
				}
			}
			candidates = append(candidates, path)
		}
	}

	if len(candidates) == 0 {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{"removed": 0, "candidates": []string{}})
		}
		return writeStdoutLine("No DLQ messages to purge.")
	}

	if *dryRunFlag {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{"candidates": candidates, "count": len(candidates)})
		}
		if err := writeStdout("Would remove %d DLQ message(s):\n", len(candidates)); err != nil {
			return err
		}
		for _, path := range candidates {
			if err := writeStdout("  %s\n", path); err != nil {
				return err
			}
		}
		return nil
	}

	if !*yesFlag {
		ok, err := confirmPrompt(fmt.Sprintf("Permanently delete %d DLQ message(s)?", len(candidates)))
		if err != nil {
			return err
		}
		if !ok {
			return writeStdoutLine("Aborted.")
		}
	}

	removed := 0
	for _, path := range candidates {
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			_ = writeStderr("warning: failed to remove %s: %v\n", path, err)
		} else {
			removed++
		}
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{"removed": removed})
	}
	return writeStdout("Removed %d DLQ message(s).\n", removed)
}
