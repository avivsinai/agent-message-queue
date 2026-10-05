package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/acp"
)

const cleanupUsage = `amq-acp cleanup removes old remote-events records.

Usage:
  amq-acp cleanup --older-than <duration> [--dry-run]

Removes the regular files under the amq-acp state dir's remote-events/
directory (<event>.json, <event>.mailbox.json, <event>.mailbox.lock,
<event>.cancelled) whose mtime is older than --older-than. Cleanup is
explicit: nothing removes these records on a schedule, and this command is
the only path. A *.mailbox.lock newer than the cutoff is left in place and
named in the output, because the writer may still hold it. Symlinks and
subdirectories are never touched. --dry-run prints what would be removed
without removing anything.

The state dir is AMQ_ACP_STATE_DIR when set (under AM_ROOT), else
$AM_ROOT/meta/acp, the same resolution the server uses.`

func runCleanup(args []string) int {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() { fmt.Fprint(os.Stderr, cleanupUsage) }
	olderThan := flags.String("older-than", "", "remove records older than this duration (e.g. 30m, 24h, 7d)")
	dryRun := flags.Bool("dry-run", false, "print what would be removed; remove nothing")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	d, err := time.ParseDuration(*olderThan)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "amq-acp cleanup: invalid --older-than %q (want e.g. 30m, 24h, 7d)\n", *olderThan)
		return exitUsage
	}
	cutoff := time.Now().Add(-d)

	root := strings.TrimSpace(os.Getenv(acp.EnvRoot))
	if root == "" || !filepath.IsAbs(root) {
		fmt.Fprintf(os.Stderr, "amq-acp cleanup: %s must be an absolute queue root\n", acp.EnvRoot)
		return exitContextMismatch
	}
	dir, err := acp.RemoteEventsDir(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp cleanup:", err)
		var contextErr *acp.ContextError
		if errors.As(err, &contextErr) {
			return exitContextMismatch
		}
		return exitGeneral
	}
	report, err := acp.CleanupRemoteEvents(dir, cutoff, *dryRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-acp cleanup:", err)
		return exitGeneral
	}
	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	for _, name := range report.Removed {
		fmt.Printf("amq-acp cleanup %s %s\n", verb, name)
	}
	for _, name := range report.Skipped {
		fmt.Printf("amq-acp cleanup skipped %s (lock newer than cutoff)\n", name)
	}
	if len(report.Removed) == 0 && len(report.Skipped) == 0 {
		fmt.Println("amq-acp cleanup: nothing to do")
	}
	return 0
}
