package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
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
	launchJournalFlag := fs.Bool(
		"launch-journal",
		false,
		"Remove one exact stuck managed-launch recovery journal",
	)
	mailboxFlag := fs.String(
		"mailbox",
		"",
		"Remove one agent mailbox under --root when it holds no unread mail, dead letters, extension data, or live wake",
	)
	historyOlderFlag := fs.String(
		"history-older-than",
		"",
		"Remove read messages, sent copies and receipts older than this duration (e.g. 720h)",
	)
	historyMaxFlag := fs.String(
		"history-max-size",
		"",
		"Remove the oldest read messages, sent copies and receipts until each mailbox's history fits this size (e.g. 100MB)",
	)
	dryRunFlag := fs.Bool("dry-run", false, "Show what would be removed without deleting")
	yesFlag := fs.Bool("yes", false, "Skip confirmation prompt")
	usage := usageWithFlags(
		fs,
		"amq cleanup [--tmp-older-than <duration>] [--wake-quarantine-older-than <duration>] [--history-older-than <duration>] [--history-max-size <size>] [--launch-journal --root <session-root>] [--mailbox <handle> --root <root>] [--dry-run] [--yes] [options]",
		"--history-older-than and --history-max-size remove history from every agent",
		"mailbox under the root: read messages (inbox/cur), sent copies (outbox/sent)",
		"and receipts, oldest first by the creation stamp in the message id. A size",
		"(B, KB, MB or GB) bounds each mailbox's history by file size. Unread mail,",
		"dead letters, extension data, and every copy of a message that",
		"'amq thread open --max-age 0' lists as open are never removed, and nothing",
		"is removed unless you run this command.",
		"--mailbox uses --root as given and removes <root>/agents/<handle>, with its",
		"read history (inbox/cur), outbox, and receipts. It refuses (exit 6) while",
		"inbox/tmp, inbox/new, any dlq leaf, or extensions/ holds an entry, or while a",
		"live or uncertain wake holds the handle. One lock (meta/removing.lock)",
		"serializes removals under a root; a second run exits 6 (busy). A dry run,",
		"and a run that finds nothing to remove, write nothing. The mailbox moves to",
		"meta/removing/<handle>-<nonce>, is checked, checked again after 250 ms, and",
		"is put back if anything arrived. A delivery that stalls longer than that",
		"between opening the inbox and publishing can still fail or be removed with",
		"the mailbox. A later run removes a clean tree an interrupted run left there,",
		"and restores one that holds mail to agents/<handle> so it can be drained.",
		"meta/config.json is not changed: a later local send to the handle recreates",
		"the mailbox and delivers. Remove the handle from the config to make send warn",
		"(or refuse with --strict).",
		"JSON: absent = no live mailbox when the run started; removed = this run",
		"deleted the live mailbox; restored = this run moved a leftover back to",
		"agents/<handle>, which exists at the end; recovered = leftovers deleted;",
		"preserved = where a tree was kept; refusal = why the run exited 6.",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	mailboxSelected := flagWasVisited(fs, "mailbox")
	historySelected := *historyOlderFlag != "" || *historyMaxFlag != ""
	if *olderFlag == "" && *wakeQuarantineOlderFlag == "" && !historySelected && !*launchJournalFlag && !mailboxSelected {
		return UsageError("one of --tmp-older-than, --wake-quarantine-older-than, --history-older-than, --history-max-size, --launch-journal, or --mailbox is required")
	}
	if mailboxSelected {
		if *olderFlag != "" || *wakeQuarantineOlderFlag != "" || historySelected || *launchJournalFlag {
			return UsageError("--mailbox cannot be combined with other cleanup selectors")
		}
		if !common.rootExplicit() {
			return UsageError("--mailbox requires an explicit --root <root>")
		}
		handle := *mailboxFlag
		if err := fsq.ValidateHandle(handle); err != nil {
			return UsageError("--mailbox: %v", err)
		}
		if handle == reservedHumanHandle {
			return UsageError("--mailbox %s is reserved for the human operator", handle)
		}
		if handle == strings.TrimSpace(common.Me) {
			return UsageError("--mailbox %s is your own handle (--me/AM_ME)", handle)
		}
		return cleanupMailbox(common.Root, handle, common.JSON, *dryRunFlag, *yesFlag)
	}
	if *launchJournalFlag {
		if *olderFlag != "" || *wakeQuarantineOlderFlag != "" || historySelected {
			return UsageError("--launch-journal cannot be combined with other cleanup selectors")
		}
		if !common.rootExplicit() {
			return UsageError("--launch-journal requires an explicit --root <session-root>")
		}
		return cleanupLaunchJournal(common.Root, common.JSON, *dryRunFlag, *yesFlag)
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
	var historyDuration time.Duration
	if *historyOlderFlag != "" {
		var err error
		historyDuration, err = time.ParseDuration(*historyOlderFlag)
		if err != nil {
			return UsageError("--history-older-than: %v", err)
		}
		if historyDuration <= 0 {
			return UsageError("--history-older-than must be > 0")
		}
	}
	var historyMax int64
	if *historyMaxFlag != "" {
		var err error
		if historyMax, err = parseByteSize(*historyMaxFlag); err != nil {
			return UsageError("--history-max-size: %v", err)
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
	var historyCandidates []fsq.HistoryFile
	var historyBytes int64
	if historySelected {
		var cutoff time.Time
		if historyDuration > 0 {
			cutoff = now.Add(-historyDuration)
		}
		open, err := openObligationIDs(absPath(root))
		if err != nil {
			return err
		}
		if historyCandidates, err = fsq.FindHistoryFiles(root, cutoff, historyMax, open); err != nil {
			return err
		}
	}
	candidates := append([]string{}, tmpCandidates...)
	for _, candidate := range wakeQuarantineCandidates {
		candidates = append(candidates, candidate.Path)
	}
	for _, candidate := range historyCandidates {
		candidates = append(candidates, candidate.Path)
		historyBytes += candidate.Size
	}
	label := "cleanup artifact(s)"
	if *wakeQuarantineOlderFlag == "" && !historySelected {
		label = "tmp file(s)"
	}
	if len(candidates) == 0 {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{
				"removed":    0,
				"candidates": []string{},
				"count":      0,
			})
		}
		if label != "tmp file(s)" {
			return writeStdoutLine("No cleanup artifacts to remove.")
		}
		return writeStdoutLine("No tmp files to remove.")
	}

	if *dryRunFlag {
		if common.JSON {
			return writeJSON(os.Stdout, map[string]any{
				"candidates":    candidates,
				"count":         len(candidates),
				"history_bytes": historyBytes,
			})
		}
		if err := writeStdout("Would remove %d %s%s.\n", len(candidates), label, historyNote(historyBytes)); err != nil {
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
	for _, candidate := range historyCandidates {
		if err := os.Remove(candidate.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		removed++
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"removed":       removed,
			"history_bytes": historyBytes,
		})
	}
	if err := writeStdout("Removed %d %s%s.\n", removed, label, historyNote(historyBytes)); err != nil {
		return err
	}
	return nil
}

type launchJournalCleanupReport struct {
	Path             string                    `json:"path"`
	Phase            launch.JournalPhase       `json:"phase"`
	Backend          string                    `json:"backend"`
	Profile          string                    `json:"profile"`
	LaunchNonce      string                    `json:"launch_nonce"`
	CreatedAt        time.Time                 `json:"created_at"`
	KnownResources   []launch.ResourceIdentity `json:"known_resources"`
	ResourceEvidence string                    `json:"resource_evidence"`
}

const launchJournalRelativePath = "meta/launch/journal.json"

func loadLaunchJournalForCleanup(root *fsq.DeliveryRoot) (launch.LaunchJournal, []byte, error) {
	file, info, err := root.OpenRegularNoFollow(launchJournalRelativePath)
	if err != nil {
		return launch.LaunchJournal{}, nil, err
	}
	defer func() { _ = file.Close() }()
	if info.Mode().Perm() != 0o600 {
		return launch.LaunchJournal{}, nil, fmt.Errorf("launch journal permissions are %04o, want 0600", info.Mode().Perm())
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return launch.LaunchJournal{}, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record launch.LaunchJournal
	if err := decoder.Decode(&record); err != nil {
		return launch.LaunchJournal{}, nil, fmt.Errorf("decode launch journal: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return launch.LaunchJournal{}, nil, fmt.Errorf("decode launch journal: multiple JSON values")
		}
		return launch.LaunchJournal{}, nil, fmt.Errorf("decode launch journal: %w", err)
	}
	return record, data, nil
}

func cleanupLaunchJournal(root string, jsonOutput, dryRun, yes bool) error {
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()
	record, raw, err := loadLaunchJournalForCleanup(deliveryRoot)
	if err != nil {
		return err
	}
	report := launchJournalCleanupReport{
		Path: launch.JournalPath(deliveryRoot.Base()), Phase: record.Phase,
		Backend: record.Backend, Profile: record.Profile, LaunchNonce: record.LaunchNonce,
		CreatedAt: record.CreatedAt, KnownResources: []launch.ResourceIdentity{},
		ResourceEvidence: "journal is non-authoritative; backend resources may still exist",
	}
	if record.Binding != nil {
		report.KnownResources = append(report.KnownResources, record.Binding.Resources.Resources...)
	}
	if dryRun {
		if jsonOutput {
			return writeJSON(os.Stdout, map[string]any{"removed": false, "launch_journal": report})
		}
		return writeLaunchJournalCleanupReport("Would remove", report)
	}
	if !yes {
		if err := writeLaunchJournalCleanupReport("Will remove", report); err != nil {
			return err
		}
		confirmed, err := confirmPrompt("Remove this recovery journal and leave any backend resources untouched?")
		if err != nil {
			return err
		}
		if !confirmed {
			return writeStdoutLine("Aborted.")
		}
	}
	lease, err := launch.AcquireLease(deliveryRoot, record.LaunchNonce)
	if err != nil {
		return err
	}
	defer func() { _ = lease.Release() }()
	if err := launch.ClearJournalRaw(deliveryRoot, lease, raw); err != nil {
		return err
	}
	if jsonOutput {
		return writeJSON(os.Stdout, map[string]any{"removed": true, "launch_journal": report})
	}
	return writeLaunchJournalCleanupReport("Removed", report)
}

func writeLaunchJournalCleanupReport(action string, report launchJournalCleanupReport) error {
	if err := writeStdout("%s launch journal %s\n", action, report.Path); err != nil {
		return err
	}
	if err := writeStdout("Backend: %s; profile: %s; nonce: %s; phase: %s\n", report.Backend, report.Profile, report.LaunchNonce, report.Phase); err != nil {
		return err
	}
	if len(report.KnownResources) == 0 {
		return writeStdoutLine("Known resources: none recorded; live resources may still exist.")
	}
	for _, resource := range report.KnownResources {
		if err := writeStdout("Known resource: %s (agent: %s)\n", resource.OpaqueID, resource.Agent); err != nil {
			return err
		}
	}
	return nil
}

func historyNote(bytes int64) string {
	if bytes == 0 {
		return ""
	}
	return fmt.Sprintf(", %d bytes of mailbox history", bytes)
}

// parseByteSize reads a positive size such as "100MB", "64KB" or "4096B".
func parseByteSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	shift := 0
	for _, u := range []struct {
		suffix string
		shift  int
	}{{"GB", 30}, {"MB", 20}, {"KB", 10}, {"B", 0}} {
		if num, ok := strings.CutSuffix(t, u.suffix); ok {
			t, shift = strings.TrimSpace(num), u.shift
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)>>shift {
		return 0, fmt.Errorf("%q is not a positive size such as 100MB", s)
	}
	return n << shift, nil
}
