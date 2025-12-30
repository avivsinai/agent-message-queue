package cli

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type drainItem struct {
	ID          string         `json:"id"`
	From        string         `json:"from"`
	To          []string       `json:"to"`
	Thread      string         `json:"thread"`
	Subject     string         `json:"subject"`
	Created     string         `json:"created"`
	Body        string         `json:"body,omitempty"`
	AckRequired bool           `json:"ack_required"`
	MovedToCur  bool           `json:"moved_to_cur"`
	MovedToDLQ  bool           `json:"moved_to_dlq,omitempty"`
	Acked       bool           `json:"acked"`
	ParseError  string         `json:"parse_error,omitempty"`
	Priority    string         `json:"priority,omitempty"`
	Kind        string         `json:"kind,omitempty"`
	Labels      []string       `json:"labels,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
	Filename    string         `json:"-"` // actual filename on disk
	SortKey     time.Time      `json:"-"`
}

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
	if err := requireMe(common.Me); err != nil {
		return err
	}
	if *limitFlag < 0 {
		return errors.New("--limit must be >= 0")
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	newDir := fsq.AgentInboxNew(root, common.Me)
	entries, err := os.ReadDir(newDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No inbox/new directory - nothing to drain
			if common.JSON {
				return writeJSON(os.Stdout, drainResult{Drained: []drainItem{}, Count: 0})
			}
			// Silent for text mode when empty (hook-friendly)
			return nil
		}
		return err
	}

	// Collect items with timestamps for sorting
	items := make([]drainItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		// Skip dotfiles (like .DS_Store) and non-.md files
		if strings.HasPrefix(filename, ".") || !strings.HasSuffix(filename, ".md") {
			continue
		}
		path := filepath.Join(newDir, filename)

		item := drainItem{
			ID:       filename, // fallback to filename if parse fails
			Filename: filename,
		}

		// Try to parse the message
		var header format.Header
		var body string
		var parseErr error

		if *includeBodyFlag {
			msg, err := format.ReadMessageFile(path)
			if err != nil {
				parseErr = err
			} else {
				header = msg.Header
				body = msg.Body
			}
		} else {
			header, parseErr = format.ReadHeaderFile(path)
		}

		if parseErr != nil {
			item.ParseError = parseErr.Error()
			// Still move corrupt message to cur to avoid reprocessing
		} else {
			item.ID = header.ID
			item.From = header.From
			item.To = header.To
			item.Thread = header.Thread
			item.Subject = header.Subject
			item.Created = header.Created
			item.AckRequired = header.AckRequired
			item.Priority = header.Priority
			item.Kind = header.Kind
			item.Labels = header.Labels
			item.Context = header.Context
			if *includeBodyFlag {
				item.Body = body
			}
			if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
				item.SortKey = ts
			}
		}

		items = append(items, item)
	}

	// Sort by timestamp (oldest first)
	sort.Slice(items, func(i, j int) bool {
		if !items[i].SortKey.IsZero() && !items[j].SortKey.IsZero() {
			if items[i].SortKey.Equal(items[j].SortKey) {
				return items[i].ID < items[j].ID
			}
			return items[i].SortKey.Before(items[j].SortKey)
		}
		if items[i].Created == items[j].Created {
			return items[i].ID < items[j].ID
		}
		return items[i].Created < items[j].Created
	})

	// Apply limit
	if *limitFlag > 0 && len(items) > *limitFlag {
		items = items[:*limitFlag]
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
	for i := range items {
		item := &items[i]

		// Move parse errors to DLQ instead of cur
		if item.ParseError != "" {
			if _, err := fsq.MoveToDLQ(root, common.Me, item.Filename, item.ID, "parse_error", item.ParseError); err != nil {
				_ = writeStderr("warning: failed to move %s to DLQ: %v\n", item.Filename, err)
			} else {
				item.MovedToDLQ = true
			}
			continue
		}

		// Move new -> cur
		if err := fsq.MoveNewToCur(root, common.Me, item.Filename); err != nil {
			if os.IsNotExist(err) {
				// Likely moved by another drain; check if it's already in cur.
				curPath := filepath.Join(fsq.AgentInboxCur(root, common.Me), item.Filename)
				if _, statErr := os.Stat(curPath); statErr == nil {
					item.MovedToCur = true
				} else if statErr != nil && !os.IsNotExist(statErr) {
					_ = writeStderr("warning: failed to stat %s in cur: %v\n", item.Filename, statErr)
				}
			} else {
				// Log warning but continue
				_ = writeStderr("warning: failed to move %s to cur: %v\n", item.Filename, err)
			}
		} else {
			item.MovedToCur = true
		}

		// Ack if required and --ack is set
		if *ackFlag && item.AckRequired && item.ParseError == "" && item.MovedToCur {
			if err := ackMessage(root, common.Me, item); err != nil {
				_ = writeStderr("warning: failed to ack %s: %v\n", item.ID, err)
			} else {
				item.Acked = true
			}
		}
	}

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
