package cli

import (
	"errors"
	"flag"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
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
	forTurnFlag := fs.Bool("for-turn", false, "Receive for an agent turn: urgent, then other wake-eligible, then passive mail, ordered before --limit; full bodies, a compact digest for passive mail, and the remaining counts")
	sessionFlag := fs.String("session", "", "Target session under the resolved base root")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, ignore a conflicting AM_SESSION pin")

	usage := usageWithFlags(fs, "amq drain --me <agent> [--session <name>] [options]",
		"Drains new messages: reads, moves to cur, emits receipts.",
		"Designed for hook/script integration. Quiet when empty.",
		"",
		"Writes only under <root>/agents/<me>/: inbox/new to inbox/cur (claim),",
		"receipts/, receipts-pending/, dlq/tmp/, dlq/new/, .presence.lock, and",
		"presence.json. The presence refresh is best-effort: drain ignores its error.",
		"A due follow-up timer in follow-ups/ becomes a notice through inbox/tmp/",
		"into inbox/new/ before the drain, and the timer is removed.",
		"",
		"--for-turn takes urgent mail first, then other wake-eligible mail, then",
		"passive mail, and applies --limit to that order. Wake-eligible mail prints",
		"in full; each passive message prints as a digest with a preview of at most",
		"160 characters and the amq read command for the original, which stays in",
		"inbox/cur. It ends with the counts still unread. Every claimed message,",
		"passive or not, gets its drained receipt.")
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
	root, routed, err := resolveMailboxRoot(common, *sessionFlag)
	if err != nil {
		return err
	}
	if err := validatePinOverride(common, *ignoreSessionPinFlag, routed); err != nil {
		return err
	}
	if err := guardMailboxContext("drain", root, routed, *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
		return err
	}
	noteActingHandle(root, me)
	deliveryIdentity, err := snapshotMailboxDeliveryRoot(root, routed, *ignoreSessionPinFlag)
	if err != nil {
		return err
	}
	if err := requireMailbox(root, me); err != nil {
		emitSiblingBacklogHintsIfInboxEmpty(root, me)
		return err
	}

	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	validator, err := newHeaderValidator(root, common.Strict)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, deliveryIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()

	if *forTurnFlag {
		items, rest, err := drainForTurnItems(deliveryRoot, root, common.Me, *limitFlag, validator)
		return finishDrainBatch(deliveryRoot, root, common.Me, common.JSON, items, err, func(items []inboxItem) error {
			return outputForTurn(common.JSON, common.Me, items, rest)
		})
	}
	items, err := drainInboxItems(deliveryRoot, root, common.Me, *includeBodyFlag, *limitFlag, validator)
	return finishDrainBatch(deliveryRoot, root, common.Me, common.JSON, items, err, func(items []inboxItem) error {
		return outputDrainItems(common.JSON, common.Me, *includeBodyFlag, items)
	})
}

// finishDrainBatch reports a drain batch through write, which also prints the
// empty JSON result.
func finishDrainBatch(deliveryRoot *fsq.DeliveryRoot, root, me string, jsonOutput bool, items []inboxItem, drainErr error, write func([]inboxItem) error) error {
	// Once a claim commits, its payload or failure record must be observable
	// even if a later claim fails. Emit accumulated results before returning the
	// batch error so callers can both consume the committed work and retry.
	if len(items) > 0 {
		_ = presence.TouchDeliveryRoot(deliveryRoot, me)
		if err := write(items); err != nil {
			if drainErr != nil {
				return errors.Join(drainErr, err)
			}
			return err
		}
	}

	// A failed receipt repair with nothing claimed still answers the drain:
	// the empty result is printed and the repair error decides the exit code.
	var repairErr *receipt.RepairError
	repairOnly := len(items) == 0 && errors.As(drainErr, &repairErr) && drainErr == error(repairErr)
	if drainErr != nil && !repairOnly {
		var committed *fsq.CommittedDurabilityError
		if errors.As(drainErr, &committed) {
			return drainErr
		}
		if os.IsNotExist(drainErr) {
			return NotFoundError("mailbox for %q disappeared while draining root %s", me, root)
		}
		return drainErr
	}

	if len(items) == 0 {
		emitSiblingBacklogHintsIfInboxEmpty(root, me)
		if jsonOutput {
			return joinOutcomeErr(drainErr, write([]inboxItem{}))
		}
		// Silent for text mode when empty (hook-friendly)
		return drainErr
	}
	return nil
}

func outputDrainItems(jsonOutput bool, me string, includeBody bool, items []inboxItem) error {
	if jsonOutput {
		return writeJSON(os.Stdout, drainResult{Drained: items, Count: len(items)})
	}

	// Text output
	if err := writeStdout("[AMQ] %d new message(s) for %s:\n\n", len(items), me); err != nil {
		return err
	}
	for _, item := range items {
		if err := writeDrainItemText(item, includeBody); err != nil {
			return err
		}
	}
	return nil
}

// writeDrainItemText prints one drained message as plain drain shows it.
func writeDrainItemText(item inboxItem, includeBody bool) error {
	if item.ParseError != "" {
		dlqNote := ""
		if item.MovedToDLQ {
			dlqNote = " [moved to DLQ]"
		}
		return writeStdout("- ID: %s\n  ERROR: %s%s\n---\n", item.ID, item.ParseError, dlqNote)
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
	for _, due := range []struct{ label, at string }{{"Reply by", item.ReplyBy}, {"News by", item.NewsBy}} {
		if due.at == "" {
			continue
		}
		relative := replyByRelative(due.at, time.Now())
		if relative == "" {
			if err := writeStdout("  %s: %s\n", due.label, sanitizeForTTY(due.at)); err != nil {
				return err
			}
		} else if err := writeStdout("  %s: %s (%s)\n", due.label, due.at, relative); err != nil {
			return err
		}
	}
	if recipe := item.Completion; recipe != nil {
		labels := ""
		if len(recipe.Labels) > 0 {
			labels = " --labels " + strings.Join(recipe.Labels, ",")
		}
		if err := writeStdout("  Close with: amq reply --id %s --kind %s%s --body @<file>\n", shellQuoteArg(item.ID), recipe.Kind, labels); err != nil {
			return err
		}
	}
	if includeBody && item.Body != "" {
		if err := writeStdout("  Body:\n%s\n", item.Body); err != nil {
			return err
		}
	}
	return writeStdout("---\n")
}
