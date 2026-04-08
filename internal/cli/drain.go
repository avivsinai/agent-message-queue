package cli

import (
	"flag"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/presence"
)

type drainResult struct {
	Drained []drainItem `json:"drained"`
	Count   int         `json:"count"`
}

func runDrain(args []string) error {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	common := addCommonFlags(fs)
	limitFlag := fs.Int("limit", 20, "Max messages to drain (0 = no limit)")
	includeBodyFlag := fs.Bool("include-body", false, "Include message body in output")

	usage := usageWithFlags(fs, "amq drain --me <agent> [options]",
		"Drains new messages: reads, moves to cur, emits receipts.",
		"Designed for hook/script integration. Quiet when empty.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	if *limitFlag < 0 {
		return UsageError("--limit must be >= 0")
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me
	root := resolveRoot(common.Root)

	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	validator, err := newHeaderValidator(root, common.Strict)
	if err != nil {
		return err
	}

	items, err := collectInboxItems(root, common.Me, *includeBodyFlag, *limitFlag, validator)
	if err != nil {
		return err
	}

	// Nothing to drain
	if len(items) == 0 {
		if common.JSON {
			return writeJSON(os.Stdout, drainResult{Drained: []drainItem{}, Count: 0})
		}
		// Silent for text mode when empty (hook-friendly)
		return nil
	}

	// Best-effort presence touch.
	_ = presence.Touch(root, common.Me)

	processInboxItems(root, common.Me, items)

	if common.JSON {
		return writeJSON(os.Stdout, drainResult{Drained: items, Count: len(items)})
	}

	// Text output
	if err := writeStdout("[AMQ] %d new message(s) for %s:\n\n", len(items), common.Me); err != nil {
		return err
	}
	for _, item := range items {
		if item.ParseError != "" {
			dlqNote := ""
			if item.MovedToDLQ {
				dlqNote = " [moved to DLQ]"
			}
			if err := writeStdout("- ID: %s\n  ERROR: %s%s\n---\n", item.ID, item.ParseError, dlqNote); err != nil {
				return err
			}
			continue
		}
		subject := item.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		priority := item.Priority
		if priority == "" {
			priority = "-"
		}
		kind := item.Kind
		if kind == "" {
			kind = "-"
		}
		fromDisplay := item.From
		if item.FromProject != "" {
			fromDisplay = item.From + " (project: " + item.FromProject + ")"
		}
		if err := writeStdout("- From: %s\n  Thread: %s\n  ID: %s\n  Subject: %s\n  Priority: %s\n  Kind: %s\n  Created: %s\n",
			fromDisplay, item.Thread, item.ID, subject, priority, kind, item.Created); err != nil {
			return err
		}
		if *includeBodyFlag && item.Body != "" {
			if err := writeStdout("  Body:\n%s\n", item.Body); err != nil {
				return err
			}
		}
		if err := writeStdout("---\n"); err != nil {
			return err
		}
	}
	return nil
}

