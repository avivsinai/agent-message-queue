package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

var validStages = map[string]bool{
	receipt.StageDrained: true,
	receipt.StageDLQ:     true,
}

func validateStage(stage string) error {
	if stage == "" {
		return nil
	}
	if !validStages[stage] {
		return fmt.Errorf("invalid stage %q (valid: drained, dlq)", stage)
	}
	return nil
}

func runReceipts(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printGroupUsage(findCommand("receipts"))
	}
	switch args[0] {
	case "list":
		return runReceiptsList(args[1:])
	case "wait":
		return runReceiptsWait(args[1:])
	default:
		return formatUnknownSubcommand("receipts", args[0])
	}
}

type receiptsListResult struct {
	Count    int               `json:"count"`
	Receipts []receiptsListRow `json:"receipts"`
}

// receiptsListRow is a written receipt, or with Owed a receipt the consumer
// owes for an outcome that already happened but has not written yet.
type receiptsListRow struct {
	receipt.Receipt
	Owed bool `json:"owed,omitempty"`
}

// listOwedReceipts returns the receipts consumer owes under root. A missing
// root owes nothing.
func listOwedReceipts(root, consumer string) ([]receipt.Receipt, error) {
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return nil, err
	}
	defer func() { _ = deliveryRoot.Close() }()
	return receipt.ListPending(deliveryRoot, consumer)
}

func sameReceiptKey(a, b receipt.Receipt) bool {
	return a.MsgID == b.MsgID && a.Consumer == b.Consumer && a.Stage == b.Stage
}

func runReceiptsList(args []string) error {
	fs := flag.NewFlagSet("receipts list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	msgID := fs.String("msg-id", "", "Filter by message ID")
	stage := fs.String("stage", "", "Filter by stage (drained, dlq)")

	usage := usageWithFlags(fs, "amq receipts list --me <agent> [--msg-id <id>] [--stage <stage>] [options]",
		"List the receipts this consumer wrote, plus the receipts it owes: a message it",
		"already consumed whose receipt write failed and was parked. Owed rows carry",
		"\"owed\": true in JSON and are marked owed in text; the next drain, read, or",
		"monitor writes them.",
	)
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
	if err := validateStage(*stage); err != nil {
		return UsageError("--stage: %v", err)
	}
	root := resolveRoot(common.Root)
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	receipts, err := receipt.List(root, me, receipt.ListFilter{
		MsgID: *msgID,
		Stage: *stage,
	})
	if err != nil {
		return err
	}
	owed, err := listOwedReceipts(root, me)
	if err != nil {
		return err
	}
	rows := make([]receiptsListRow, 0, len(receipts)+len(owed))
	for _, r := range receipts {
		rows = append(rows, receiptsListRow{Receipt: r})
	}
	for _, r := range owed {
		if (*msgID != "" && r.MsgID != *msgID) || (*stage != "" && r.Stage != *stage) {
			continue
		}
		written := false
		for _, w := range receipts {
			written = written || sameReceiptKey(w, r)
		}
		if !written {
			rows = append(rows, receiptsListRow{Receipt: r, Owed: true})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].EmittedAt < rows[j].EmittedAt })

	result := receiptsListResult{
		Count:    len(rows),
		Receipts: rows,
	}

	if common.JSON {
		return writeJSON(os.Stdout, result)
	}

	if len(rows) == 0 {
		return writeStdout("No receipts found.\n")
	}

	for _, r := range rows {
		mark := ""
		if r.Owed {
			mark = "  owed (not written yet)"
		}
		if err := writeStdout("%-12s  %-10s  %-10s  %s  %s%s\n",
			r.Stage, r.Sender, r.Consumer, r.MsgID, r.EmittedAt, mark); err != nil {
			return err
		}
	}
	return nil
}

type receiptsWaitResult struct {
	Event   string           `json:"event"`
	Receipt *receipt.Receipt `json:"receipt,omitempty"`
	// Owed marks a timeout for a message the consumer already consumed
	// whose receipt it has parked but not written yet.
	Owed bool `json:"owed,omitempty"`
}

func runReceiptsWait(args []string) error {
	fs := flag.NewFlagSet("receipts wait", flag.ContinueOnError)
	common := addCommonFlags(fs)
	msgID := fs.String("msg-id", "", "Message ID to wait for (required)")
	stage := fs.String("stage", receipt.StageDrained, "Stage to wait for (drained, dlq)")
	timeoutFlag := fs.Duration("timeout", 60*time.Second, "Maximum time to wait (0 = wait forever)")
	pollInterval := fs.Duration("poll-interval", 1*time.Second, "Polling interval")

	usage := usageWithFlags(fs, "amq receipts wait --me <agent> --msg-id <id> [--stage <stage>] [--timeout <duration>] [options]",
		"Wait for this consumer's receipt for one message. A timeout exits 4. When the",
		"message was already consumed and its receipt is owed (parked, not written yet),",
		"the timeout says so, and JSON adds \"owed\": true.",
	)
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
	if *msgID == "" {
		return UsageError("--msg-id is required")
	}
	if _, err := ensureFilename(*msgID); err != nil {
		return UsageError("--msg-id: %v", err)
	}
	if err := validateStage(*stage); err != nil {
		return UsageError("--stage: %v", err)
	}
	if *timeoutFlag < 0 {
		return UsageError("--timeout must be >= 0")
	}
	if *pollInterval <= 0 {
		return UsageError("--poll-interval must be > 0")
	}
	root := resolveRoot(common.Root)
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	r, err := receipt.WaitFor(root, *msgID, me, *stage, *timeoutFlag, *pollInterval)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		// The timeout stands either way; an unreadable pending directory only
		// loses the owed annotation.
		owedList, _ := listOwedReceipts(root, me)
		owed := false
		for _, o := range owedList {
			owed = owed || (o.MsgID == *msgID && o.Stage == *stage)
		}
		if common.JSON {
			if err := writeJSON(os.Stdout, receiptsWaitResult{Event: "timeout", Owed: owed}); err != nil {
				return err
			}
		} else if owed {
			if err := writeStdout("No %s receipt for %s (timeout): consumed; its %s receipt is owed and is written by the consumer's next drain\n", *stage, *msgID, *stage); err != nil {
				return err
			}
		} else {
			if err := writeStdout("No %s receipt for %s (timeout)\n", *stage, *msgID); err != nil {
				return err
			}
		}
		return TimeoutError("receipts wait timed out")
	}
	if err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, receiptsWaitResult{
			Event:   "matched",
			Receipt: &r,
		})
	}

	return writeStdout("Receipt: %s %s by %s at %s\n", r.Stage, r.MsgID, r.Consumer, r.EmittedAt)
}
