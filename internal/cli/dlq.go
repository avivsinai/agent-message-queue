package cli

import (
	"crypto/sha256"
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

var syncDLQPurgeDir = func(root *fsq.DeliveryRoot, dir string) error {
	return root.SyncDir(dir)
}

var removeDLQPurgeCandidate = func(root *fsq.DeliveryRoot, path string) error {
	return root.Remove(path)
}

var statDLQPurgeCandidate = func(root *fsq.DeliveryRoot, path string) (os.FileInfo, error) {
	return root.Stat(path)
}

var retryDLQMessage = fsq.RetryFromDLQ

var inspectDLQMessage = fsq.InspectDLQEnvelope

var readDLQRetryDir = func(root *fsq.DeliveryRoot, dir string) ([]os.DirEntry, error) {
	return root.ReadDir(dir)
}

type dlqPurgeCandidate struct {
	path     string
	identity os.FileInfo
	digest   [sha256.Size]byte
}

func runDLQ(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printGroupUsage(findCommand("dlq"))
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
		return formatUnknownSubcommand("dlq", args[0])
	}
}

// dlqListItem represents a DLQ message for listing.
type dlqListItem struct {
	ID             string    `json:"id"`
	OriginalID     string    `json:"original_id"`
	OriginalFile   string    `json:"original_file"`
	FailureReason  string    `json:"failure_reason"`
	FailureDetail  string    `json:"failure_detail,omitempty"`
	FailureTime    string    `json:"failure_time"`
	RetryCount     int       `json:"retry_count"`
	RetryState     string    `json:"retry_state"`
	RetryPending   bool      `json:"retry_pending"`
	RetryDelivered bool      `json:"retry_delivered"`
	Box            string    `json:"box"`
	Path           string    `json:"path"`
	SortKey        time.Time `json:"-"`
}

func runDLQList(args []string) error {
	fs := flag.NewFlagSet("dlq list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	newFlag := fs.Bool("new", false, "List only unread DLQ messages (dlq/new)")
	curFlag := fs.Bool("cur", false, "List only inspected DLQ messages (dlq/cur)")
	sessionFlag := fs.String("session", "", "Target session under the resolved base root")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, ignore a conflicting AM_SESSION pin")

	usage := usageWithFlags(fs, "amq dlq list --me <agent> [--session <name>] [--new | --cur] [options]")
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
		return UsageError("--me: %v", err)
	}
	common.Me = me
	root, routed, err := resolveMailboxRoot(common, *sessionFlag)
	if err != nil {
		return err
	}
	if err := validatePinOverride(common, *ignoreSessionPinFlag, routed); err != nil {
		return err
	}
	if err := guardMailboxContext("dlq list", root, routed, *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
		return err
	}

	if *newFlag && *curFlag {
		return UsageError("use only one of --new or --cur")
	}

	// The combined view represents logical envelopes, not physical box entries.
	// A same-name copy in cur is the newer authority after a recoverable
	// new-to-cur update, so it must win over stale new residue. Explicit box
	// filters intentionally retain their physical inspection semantics.
	boxes := []string{fsq.BoxCur, fsq.BoxNew}
	dedupeByFilename := true
	if *newFlag {
		boxes = []string{fsq.BoxNew}
		dedupeByFilename = false
	} else if *curFlag {
		boxes = []string{fsq.BoxCur}
		dedupeByFilename = false
	}

	deliveryIdentity, err := snapshotMailboxDeliveryRoot(root, routed, *ignoreSessionPinFlag)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, deliveryIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()

	if err := validateKnownHandlesDeliveryRoot(deliveryRoot, common.Strict, me); err != nil {
		return err
	}

	items, err := collectDLQListItemsWithPrecedence(deliveryRoot, me, boxes, dedupeByFilename)
	if err != nil {
		return err
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
		var suffixParts []string
		if item.RetryCount > 0 {
			suffixParts = append(suffixParts, fmt.Sprintf("retries: %d", item.RetryCount))
		}
		if item.RetryState != "" {
			suffixParts = append(suffixParts, "state: "+item.RetryState)
		}
		suffix := ""
		if len(suffixParts) > 0 {
			suffix = " (" + strings.Join(suffixParts, ", ") + ")"
		}
		if err := writeStdout("[%s] %s  %s  %s%s\n", item.Box, item.FailureTime, item.FailureReason, item.ID, suffix); err != nil {
			return err
		}
	}
	return nil
}

func collectDLQListItems(root *fsq.DeliveryRoot, me string, boxes []string) ([]dlqListItem, error) {
	return collectDLQListItemsWithPrecedence(root, me, boxes, false)
}

func collectDLQListItemsWithPrecedence(root *fsq.DeliveryRoot, me string, boxes []string, dedupeByFilename bool) ([]dlqListItem, error) {
	var items []dlqListItem
	seen := make(map[string]struct{})
	for _, box := range boxes {
		dir := filepath.Join("agents", me, "dlq", box)
		entries, err := root.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, NotFoundError(
					"mailbox for %q is missing at root %s (missing %s); check AM_ROOT or use --session <name>",
					me,
					root.Base(),
					root.DisplayPath(dir),
				)
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if dedupeByFilename {
				if _, duplicate := seen[entry.Name()]; duplicate {
					continue
				}
				// cur is scanned first for the combined view. Claim the filename
				// before parsing so a corrupt authoritative cur artifact cannot
				// accidentally revive an older new copy in the same output.
				seen[entry.Name()] = struct{}{}
			}
			path := filepath.Join(dir, entry.Name())
			env, _, err := fsq.ReadDLQEnvelope(root, path)
			if err != nil {
				_ = writeStderr("warning: skipping corrupt DLQ message %s: %v\n", entry.Name(), err)
				continue
			}
			item := dlqListItem{
				ID:             env.ID,
				OriginalID:     env.OriginalID,
				OriginalFile:   env.OriginalFile,
				FailureReason:  env.FailureReason,
				FailureDetail:  env.FailureDetail,
				FailureTime:    env.FailureTime,
				RetryCount:     env.RetryCount,
				RetryState:     env.RetryState,
				RetryPending:   env.RetryPending,
				RetryDelivered: env.RetryDelivered,
				Box:            box,
				Path:           root.DisplayPath(path),
			}
			if ts, err := time.Parse(time.RFC3339, env.FailureTime); err == nil {
				item.SortKey = ts
			}
			items = append(items, item)
		}
	}
	return items, nil
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
	RetryState      string `json:"retry_state"`
	RetryPending    bool   `json:"retry_pending"`
	RetryDelivered  bool   `json:"retry_delivered"`
	SourceDir       string `json:"source_dir"`
	Box             string `json:"box"`
	OriginalContent string `json:"original_content"`
}

func runDLQRead(args []string) error {
	fs := flag.NewFlagSet("dlq read", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "DLQ message ID to read")
	sessionFlag := fs.String("session", "", "Target session under the resolved base root")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, ignore a conflicting AM_SESSION pin")

	usage := usageWithFlags(fs, "amq dlq read --me <agent> --id <dlq_id> [--session <name>] [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	if *idFlag == "" {
		return UsageError("--id is required")
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me
	root, routed, err := resolveMailboxRoot(common, *sessionFlag)
	if err != nil {
		return err
	}
	if err := validatePinOverride(common, *ignoreSessionPinFlag, routed); err != nil {
		return err
	}
	if err := guardMailboxContext("dlq read", root, routed, *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
		return err
	}
	deliveryIdentity, err := snapshotMailboxDeliveryRoot(root, routed, *ignoreSessionPinFlag)
	if err != nil {
		return err
	}
	if err := requireMailbox(root, me); err != nil {
		return err
	}

	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, deliveryIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return UsageError("--id: %v", err)
	}

	env, originalContent, box, inspectErr := inspectDLQMessage(deliveryRoot, me, filename)
	if inspectErr != nil && env == nil {
		if os.IsNotExist(inspectErr) {
			return fmt.Errorf("DLQ message not found: %s", *idFlag)
		}
		return inspectErr
	}

	result := dlqReadResult{
		ID:              env.ID,
		OriginalID:      env.OriginalID,
		OriginalFile:    env.OriginalFile,
		FailureReason:   env.FailureReason,
		FailureDetail:   env.FailureDetail,
		FailureTime:     env.FailureTime,
		RetryCount:      env.RetryCount,
		RetryState:      env.RetryState,
		RetryPending:    env.RetryPending,
		RetryDelivered:  env.RetryDelivered,
		SourceDir:       env.SourceDir,
		Box:             box,
		OriginalContent: string(originalContent),
	}
	return errors.Join(inspectErr, outputDLQReadResult(common.JSON, result))
}

func outputDLQReadResult(jsonOutput bool, result dlqReadResult) error {
	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}
	if err := writeStdout("DLQ ID:         %s\n", result.ID); err != nil {
		return err
	}
	if err := writeStdout("Original ID:    %s\n", result.OriginalID); err != nil {
		return err
	}
	if err := writeStdout("Original File:  %s\n", result.OriginalFile); err != nil {
		return err
	}
	if err := writeStdout("Failure Reason: %s\n", result.FailureReason); err != nil {
		return err
	}
	if err := writeStdout("Failure Detail: %s\n", result.FailureDetail); err != nil {
		return err
	}
	if err := writeStdout("Failure Time:   %s\n", result.FailureTime); err != nil {
		return err
	}
	if err := writeStdout("Retry Count:    %d\n", result.RetryCount); err != nil {
		return err
	}
	if err := writeStdout("Retry State:    %s\n", result.RetryState); err != nil {
		return err
	}
	if err := writeStdout("Retry Pending:  %t\n", result.RetryPending); err != nil {
		return err
	}
	if err := writeStdout("Retry Delivered: %t\n", result.RetryDelivered); err != nil {
		return err
	}
	if err := writeStdout("Source Dir:     %s\n", result.SourceDir); err != nil {
		return err
	}
	if err := writeStdoutLine("---"); err != nil {
		return err
	}
	return writeStdout("%s", result.OriginalContent)
}

func runDLQRetry(args []string) error {
	fs := flag.NewFlagSet("dlq retry", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "DLQ message ID to retry")
	allFlag := fs.Bool("all", false, "Retry all DLQ messages")
	forceFlag := fs.Bool("force", false, "Force retry even if max retries exceeded")
	sessionFlag := fs.String("session", "", "Target session under the resolved base root")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, ignore a conflicting AM_SESSION pin")

	usage := usageWithFlags(fs, "amq dlq retry --me <agent> --id <dlq_id> [--session <name>] [--force] [options]",
		"Or: amq dlq retry --me <agent> --all [--session <name>] [--force]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	if *idFlag == "" && !*allFlag {
		return UsageError("--id or --all is required")
	}
	if *idFlag != "" && *allFlag {
		return UsageError("use --id or --all, not both")
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me
	root, routed, err := resolveMailboxRoot(common, *sessionFlag)
	if err != nil {
		return err
	}
	if err := validatePinOverride(common, *ignoreSessionPinFlag, routed); err != nil {
		return err
	}
	if err := guardMailboxContext("dlq retry", root, routed, *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
		return err
	}
	deliveryIdentity, err := snapshotMailboxDeliveryRoot(root, routed, *ignoreSessionPinFlag)
	if err != nil {
		return err
	}
	if err := requireMailbox(root, me); err != nil {
		return err
	}

	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, deliveryIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()

	if *allFlag {
		return retryAllDLQ(deliveryRoot, me, *forceFlag, common.JSON)
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return UsageError("--id: %v", err)
	}

	retryErr := retryDLQMessage(deliveryRoot, me, filename, *forceFlag)
	alreadyDelivered := errors.Is(retryErr, fsq.ErrDLQRetryDelivered)
	if retryErr != nil && !alreadyDelivered && !committedDLQRetry(deliveryRoot, me, retryErr) {
		return retryErr
	}

	var outputErr error
	if alreadyDelivered && common.JSON {
		outputErr = writeJSON(os.Stdout, map[string]any{
			"already_delivered": *idFlag,
			"audit_finalized":   true,
		})
	} else if alreadyDelivered {
		outputErr = writeStdout("Already delivered: %s (audit finalized).\n", *idFlag)
	} else if common.JSON {
		outputErr = writeJSON(os.Stdout, map[string]any{
			"retried": *idFlag,
		})
	} else {
		outputErr = writeStdout("Retried: %s\n", *idFlag)
	}
	if alreadyDelivered {
		return outputErr
	}
	return errors.Join(retryErr, outputErr)
}

func retryAllDLQ(root *fsq.DeliveryRoot, me string, force, jsonOutput bool) error {
	type candidate struct {
		filename  string
		sourceDir string
	}
	var candidates []candidate
	var scanErr error
	seen := make(map[string]struct{})
	// cur is authoritative if a crash left an old same-name envelope in new.
	// Scanning it first keeps both retry selection and any diagnostic path tied
	// to the state that RetryFromDLQ will actually operate on.
	for _, box := range []string{fsq.BoxCur, fsq.BoxNew} {
		dir := filepath.Join("agents", me, "dlq", box)
		entries, err := readDLQRetryDir(root, dir)
		if err != nil {
			if !os.IsNotExist(err) {
				scanErr = errors.Join(scanErr, fmt.Errorf(
					"read DLQ directory %s: %w",
					root.DisplayPath(dir),
					err,
				))
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			filename := entry.Name()
			if _, duplicate := seen[filename]; duplicate {
				continue
			}
			seen[filename] = struct{}{}
			candidates = append(candidates, candidate{
				filename:  filename,
				sourceDir: dir,
			})
		}
	}

	var retried []string
	var alreadyDelivered []string
	var skipped []string
	var retryErr error
	for _, candidate := range candidates {
		err := retryDLQMessage(root, me, candidate.filename, force)
		if err == nil || committedDLQRetry(root, me, err) {
			retried = append(retried, strings.TrimSuffix(candidate.filename, ".md"))
		} else if errors.Is(err, fsq.ErrDLQRetryDelivered) {
			// Bulk retry is convergent: a persisted delivered audit has already
			// achieved the requested outcome, so a rerun is a success rather than
			// a permanent skipped/error item. Single --id follows the same
			// convergent success rule and reports the idempotent outcome.
			alreadyDelivered = append(alreadyDelivered, strings.TrimSuffix(candidate.filename, ".md"))
		} else {
			skipped = append(skipped, candidate.filename)
		}
		if err != nil && !errors.Is(err, fsq.ErrDLQRetryDelivered) {
			retryErr = errors.Join(retryErr, fmt.Errorf(
				"retry DLQ message %s: %w",
				root.DisplayPath(filepath.Join(candidate.sourceDir, candidate.filename)),
				err,
			))
		}
	}

	var outputErr error
	if jsonOutput {
		outputErr = writeJSON(os.Stdout, map[string]any{
			"retried":           retried,
			"already_delivered": alreadyDelivered,
			"skipped":           skipped,
			"count":             len(retried),
		})
	} else if len(retried) == 0 && len(alreadyDelivered) == 0 {
		outputErr = writeStdoutLine("No DLQ messages retried.")
	} else {
		if len(retried) > 0 {
			outputErr = errors.Join(outputErr, writeStdout("Retried %d message(s).\n", len(retried)))
		}
		if len(alreadyDelivered) > 0 {
			outputErr = errors.Join(outputErr, writeStdout("Already delivered: %d message(s).\n", len(alreadyDelivered)))
		}
	}
	return errors.Join(scanErr, retryErr, outputErr)
}

func committedDLQRetry(root *fsq.DeliveryRoot, me string, err error) bool {
	var committed *fsq.CommittedDurabilityError
	if !errors.As(err, &committed) || committed.Recipient != me {
		return false
	}
	wantDir := root.DisplayPath(filepath.Join("agents", me, "inbox", "new"))
	return filepath.Clean(filepath.Dir(committed.FinalPath)) == filepath.Clean(wantDir)
}

func runDLQPurge(args []string) error {
	fs := flag.NewFlagSet("dlq purge", flag.ContinueOnError)
	common := addCommonFlags(fs)
	olderFlag := fs.String("older-than", "", "Duration (e.g. 24h) - only purge messages older than this")
	dryRunFlag := fs.Bool("dry-run", false, "Show what would be removed without deleting")
	yesFlag := fs.Bool("yes", false, "Skip confirmation prompt")
	sessionFlag := fs.String("session", "", "Target session under the resolved base root")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, ignore a conflicting AM_SESSION pin")

	usage := usageWithFlags(fs, "amq dlq purge --me <agent> [--session <name>] [--older-than <duration>] [--dry-run] [--yes] [options]")
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
		return UsageError("--me: %v", err)
	}
	common.Me = me
	root, routed, err := resolveMailboxRoot(common, *sessionFlag)
	if err != nil {
		return err
	}
	if err := validatePinOverride(common, *ignoreSessionPinFlag, routed); err != nil {
		return err
	}
	if err := guardMailboxContext("dlq purge", root, routed, *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
		return err
	}
	deliveryIdentity, err := snapshotMailboxDeliveryRoot(root, routed, *ignoreSessionPinFlag)
	if err != nil {
		return err
	}
	if err := requireMailbox(root, me); err != nil {
		return err
	}

	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, deliveryIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()

	var cutoff time.Time
	if *olderFlag != "" {
		dur, err := time.ParseDuration(*olderFlag)
		if err != nil {
			return UsageError("--older-than: %v", err)
		}
		if dur <= 0 {
			return UsageError("--older-than must be > 0")
		}
		cutoff = time.Now().Add(-dur)
	}

	// Collect candidates from both new and cur
	var candidates []dlqPurgeCandidate
	var selectionErr error
	for _, box := range []string{"new", "cur"} {
		var dir string
		if box == "new" {
			dir = filepath.Join("agents", me, "dlq", "new")
		} else {
			dir = filepath.Join("agents", me, "dlq", "cur")
		}
		entries, err := deliveryRoot.ReadDir(dir)
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
			candidate, selected, err := selectDLQPurgeCandidate(deliveryRoot, path, cutoff)
			if err != nil {
				selectionErr = errors.Join(selectionErr, err)
				continue
			}
			if selected {
				candidates = append(candidates, candidate)
			}
		}
	}

	if len(candidates) == 0 {
		if common.JSON {
			return errors.Join(selectionErr, writeJSON(os.Stdout, map[string]any{"removed": 0, "candidates": []string{}}))
		}
		return errors.Join(selectionErr, writeStdoutLine("No DLQ messages to purge."))
	}

	if *dryRunFlag {
		if common.JSON {
			displayCandidates := make([]string, len(candidates))
			for i, candidate := range candidates {
				displayCandidates[i] = deliveryRoot.DisplayPath(candidate.path)
			}
			return errors.Join(selectionErr, writeJSON(os.Stdout, map[string]any{"candidates": displayCandidates, "count": len(candidates)}))
		}
		if err := writeStdout("Would remove %d DLQ message(s):\n", len(candidates)); err != nil {
			return errors.Join(selectionErr, err)
		}
		for _, candidate := range candidates {
			if err := writeStdout("  %s\n", deliveryRoot.DisplayPath(candidate.path)); err != nil {
				return errors.Join(selectionErr, err)
			}
		}
		return selectionErr
	}

	if !*yesFlag {
		ok, err := confirmPrompt(fmt.Sprintf("Permanently delete %d DLQ message(s)?", len(candidates)))
		if err != nil {
			return errors.Join(selectionErr, err)
		}
		if !ok {
			return errors.Join(selectionErr, writeStdoutLine("Aborted."))
		}
	}

	removed := 0
	var removeErr error
	for _, candidate := range candidates {
		removedCandidate, err := removeSelectedDLQPurgeCandidate(deliveryRoot, me, candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			removeErr = errors.Join(removeErr, fmt.Errorf(
				"remove DLQ message %s: %w",
				deliveryRoot.DisplayPath(candidate.path),
				err,
			))
		} else if removedCandidate {
			removed++
		}
	}
	var syncErr error
	for _, dir := range []string{filepath.Join("agents", me, "dlq", "new"), filepath.Join("agents", me, "dlq", "cur")} {
		if err := syncDLQPurgeDir(deliveryRoot, dir); err != nil && !os.IsNotExist(err) {
			syncErr = errors.Join(syncErr, fmt.Errorf("sync purged DLQ directory %s: %w", deliveryRoot.DisplayPath(dir), err))
		}
	}

	var outputErr error
	if common.JSON {
		outputErr = writeJSON(os.Stdout, map[string]any{"removed": removed})
	} else {
		outputErr = writeStdout("Removed %d DLQ message(s).\n", removed)
	}
	return errors.Join(selectionErr, removeErr, syncErr, outputErr)
}

func selectDLQPurgeCandidate(root *fsq.DeliveryRoot, path string, cutoff time.Time) (candidate dlqPurgeCandidate, selected bool, err error) {
	identity, err := statDLQPurgeCandidate(root, path)
	if err != nil {
		if cutoff.IsZero() {
			return candidate, false, fmt.Errorf("inspect DLQ message %s for purge: %w", root.DisplayPath(path), err)
		}
		return candidate, false, fmt.Errorf("inspect DLQ message %s for --older-than: %w", root.DisplayPath(path), err)
	}
	raw, err := root.ReadRegularNoFollow(path)
	if err != nil {
		return candidate, false, fmt.Errorf("read DLQ message %s for purge: %w", root.DisplayPath(path), err)
	}
	candidate = dlqPurgeCandidate{
		path:     path,
		identity: identity,
		digest:   sha256.Sum256(raw),
	}
	if cutoff.IsZero() {
		return candidate, true, nil
	}
	env, _, err := fsq.ReadDLQEnvelope(root, path)
	if err != nil {
		return candidate, false, fmt.Errorf("inspect DLQ message %s for --older-than: %w", root.DisplayPath(path), err)
	}
	failureTime, err := time.Parse(time.RFC3339, env.FailureTime)
	if err != nil {
		return candidate, false, fmt.Errorf("inspect DLQ message %s failure_time for --older-than: %w", root.DisplayPath(path), err)
	}
	return candidate, !failureTime.After(cutoff), nil
}

func removeSelectedDLQPurgeCandidate(root *fsq.DeliveryRoot, me string, candidate dlqPurgeCandidate) (removed bool, err error) {
	filename := filepath.Base(candidate.path)
	err = root.WithDLQEnvelopeLock(me, filename, func(batch *fsq.DeliveryRoot) error {
		current, statErr := statDLQPurgeCandidate(batch, candidate.path)
		if statErr != nil {
			return statErr
		}
		if !os.SameFile(candidate.identity, current) {
			return nil
		}
		raw, readErr := batch.ReadRegularNoFollow(candidate.path)
		if readErr != nil {
			return readErr
		}
		if sha256.Sum256(raw) != candidate.digest {
			return nil
		}
		if removeErr := removeDLQPurgeCandidate(batch, candidate.path); removeErr != nil {
			return removeErr
		}
		removed = true
		return nil
	})
	return removed, err
}
