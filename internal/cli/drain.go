package cli

import (
	"flag"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
	ackFlag := fs.Bool("ack", true, "Acknowledge messages that require ack")

	usage := usageWithFlags(fs, "amq drain --me <agent> [options]",
		"Drains new messages: reads, moves to cur, optionally acks.",
		"Designed for hook/script integration. Quiet when empty.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
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

	// Process each message: move to cur (or DLQ for parse errors), optionally ack
	processInboxItems(root, common.Me, *ackFlag, items)

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
		if err := writeStdout("- From: %s\n  Thread: %s\n  ID: %s\n  Subject: %s\n  Priority: %s\n  Kind: %s\n  Created: %s\n",
			item.From, item.Thread, item.ID, subject, priority, kind, item.Created); err != nil {
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

func ackMessage(root, me string, item *drainItem) error {
	sender, err := normalizeHandle(item.From)
	if err != nil {
		return err
	}
	msgID, err := ensureSafeBaseName(item.ID)
	if err != nil {
		return err
	}

	ackPayload := ack.New(item.ID, item.Thread, me, sender, time.Now())

	// Write to receiver's sent acks
	receiverDir := fsq.AgentAcksSent(root, me)
	receiverPath := filepath.Join(receiverDir, msgID+".json")
	needsReceiverWrite := true
	if existing, err := ack.Read(receiverPath); err == nil {
		ackPayload = existing
		needsReceiverWrite = false
	} else if !os.IsNotExist(err) {
		// Corrupt ack file - warn and rewrite
		_ = writeStderr("warning: corrupt ack file, rewriting: %v\n", err)
	}

	data, err := ackPayload.Marshal()
	if err != nil {
		return err
	}

	if needsReceiverWrite {
		if _, err := fsq.WriteFileAtomic(receiverDir, msgID+".json", data, 0o600); err != nil {
			return err
		}
	}

	// Best-effort write to sender's received acks
	senderDir := fsq.AgentAcksReceived(root, sender)
	senderPath := filepath.Join(senderDir, msgID+".json")
	if _, err := os.Stat(senderPath); err == nil {
		// Already recorded.
	} else if os.IsNotExist(err) {
		if _, err := fsq.WriteFileAtomic(senderDir, msgID+".json", data, 0o600); err != nil {
			_ = writeStderr("warning: unable to write sender ack: %v\n", err)
		}
	} else if err != nil {
		return err
	}

	return nil
}
