package cli

import (
	"flag"
	"fmt"
	"os"
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
	dryRunFlag := fs.Bool("dry-run", false, "Show what would be removed without deleting")
	yesFlag := fs.Bool("yes", false, "Skip confirmation prompt")
	usage := usageWithFlags(
		fs,
		"amq cleanup [--tmp-older-than <duration>] [--wake-quarantine-older-than <duration>] [--launch-journal --root <session-root>] [--dry-run] [--yes] [options]",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *olderFlag == "" && *wakeQuarantineOlderFlag == "" && !*launchJournalFlag {
		return UsageError("one of --tmp-older-than, --wake-quarantine-older-than, or --launch-journal is required")
	}
	if *launchJournalFlag {
		if *olderFlag != "" || *wakeQuarantineOlderFlag != "" {
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
	record, err := launch.LoadJournal(deliveryRoot)
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
	if err := launch.ClearJournal(deliveryRoot, lease, record); err != nil {
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
