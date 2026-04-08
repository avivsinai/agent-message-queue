package cli

import (
	"flag"
	"os"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

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

// receiptsListResult is the JSON output for receipts list.
type receiptsListResult struct {
	Count    int               `json:"count"`
	Receipts []receipt.Receipt `json:"receipts"`
}

func runReceiptsList(args []string) error {
	fs := flag.NewFlagSet("receipts list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	msgID := fs.String("msg-id", "", "Filter by message ID")
	stage := fs.String("stage", "", "Filter by stage (drained, dlq)")

	usage := usageWithFlags(fs, "amq receipts list --me <agent> [--msg-id <id>] [--stage <stage>] [options]")
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
	root := resolveRoot(common.Root)

	receipts, err := receipt.List(root, me, receipt.ListFilter{
		MsgID: *msgID,
		Stage: *stage,
	})
	if err != nil {
		return err
	}
	if receipts == nil {
		receipts = []receipt.Receipt{}
	}

	result := receiptsListResult{
		Count:    len(receipts),
		Receipts: receipts,
	}

	if common.JSON {
		return writeJSON(os.Stdout, result)
	}

	if len(receipts) == 0 {
		return writeStdout("No receipts found.\n")
	}

	for _, r := range receipts {
		if err := writeStdout("%-12s  %-10s  %-10s  %s  %s\n",
			r.Stage, r.Sender, r.Consumer, r.MsgID, r.EmittedAt); err != nil {
			return err
		}
	}
	return nil
}

// receiptsWaitResult is the JSON output for receipts wait.
type receiptsWaitResult struct {
	Event   string           `json:"event"` // "matched" or "timeout"
	Receipt *receipt.Receipt `json:"receipt,omitempty"`
}

func runReceiptsWait(args []string) error {
	fs := flag.NewFlagSet("receipts wait", flag.ContinueOnError)
	common := addCommonFlags(fs)
	msgID := fs.String("msg-id", "", "Message ID to wait for (required)")
	stage := fs.String("stage", receipt.StageDrained, "Stage to wait for (drained, dlq)")
	timeoutFlag := fs.Duration("timeout", 60*time.Second, "Maximum time to wait (0 = wait forever)")
	pollInterval := fs.Duration("poll-interval", 1*time.Second, "Polling interval")

	usage := usageWithFlags(fs, "amq receipts wait --me <agent> --msg-id <id> [--stage <stage>] [--timeout <duration>] [options]")
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
	if *timeoutFlag < 0 {
		return UsageError("--timeout must be >= 0")
	}
	root := resolveRoot(common.Root)

	deadline := time.Time{}
	if *timeoutFlag > 0 {
		deadline = time.Now().Add(*timeoutFlag)
	}

	for {
		receipts, err := receipt.List(root, me, receipt.ListFilter{
			MsgID: *msgID,
			Stage: *stage,
		})
		if err != nil {
			return err
		}
		if len(receipts) > 0 {
			r := receipts[0]
			if common.JSON {
				return writeJSON(os.Stdout, receiptsWaitResult{
					Event:   "matched",
					Receipt: &r,
				})
			}
			return writeStdout("Receipt: %s %s from %s at %s\n", r.Stage, r.MsgID, r.Consumer, r.EmittedAt)
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			if common.JSON {
				return writeJSON(os.Stdout, receiptsWaitResult{Event: "timeout"})
			}
			return writeStdout("No %s receipt for %s (timeout)\n", *stage, *msgID)
		}

		time.Sleep(*pollInterval)
	}
}
