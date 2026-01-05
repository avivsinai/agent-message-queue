package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

type monitorResult struct {
	Event      string        `json:"event"`                 // "messages", "timeout", "empty"
	WatchEvent string        `json:"watch_event,omitempty"` // "existing", "new_message", ""
	Mode       string        `json:"mode,omitempty"`        // "drain", "peek"
	Me         string        `json:"me"`
	Count      int           `json:"count"`
	Drained    []monitorItem `json:"drained"`
}

func runMonitor(args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	common := addCommonFlags(fs)
	timeoutFlag := fs.Duration("timeout", 60*time.Second, "Max time to wait for messages (0 = wait forever)")
	pollFlag := fs.Bool("poll", false, "Use polling fallback instead of fsnotify")
	includeBodyFlag := fs.Bool("include-body", false, "Include message body in output")
	ackFlag := fs.Bool("ack", true, "Acknowledge messages that require ack")
	limitFlag := fs.Int("limit", 20, "Max messages to drain (0 = no limit)")
	peekFlag := fs.Bool("peek", false, "Peek without moving messages to cur (no ack)")

	usage := usageWithFlags(fs, "amq monitor --me <agent> [options]",
		"Combined watch+drain: waits for messages, drains them, outputs structured payload.",
		"Use --peek to watch without moving messages to cur (no ack).",
		"Ideal for co-op mode background watchers in Claude Code or Codex.")
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
	root := resolveRoot(common.Root)

	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	validator, err := newHeaderValidator(root, common.Strict)
	if err != nil {
		return err
	}

	inboxNew := fsq.AgentInboxNew(root, common.Me)
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		return err
	}

	mode := "drain"
	doAck := *ackFlag
	if *peekFlag {
		mode = "peek"
		doAck = false
	}

	// First, try to drain existing messages
	items, err := collectInboxItems(root, common.Me, *includeBodyFlag, *limitFlag, validator)
	if err != nil {
		return err
	}

	if len(items) > 0 {
		if mode == "drain" {
			drainMonitorItems(root, common.Me, doAck, items)
		}
		return outputMonitorResult(common.JSON, monitorResult{
			Event:      "messages",
			WatchEvent: "existing",
			Mode:       mode,
			Me:         common.Me,
			Count:      len(items),
			Drained:    items,
		})
	}

	// No existing messages - wait for new ones
	ctx := context.Background()
	if *timeoutFlag > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeoutFlag)
		defer cancel()
	}

	var watchEvent string
	var watchErr error

	if *pollFlag {
		watchEvent, watchErr = monitorWithPolling(ctx, inboxNew)
	} else {
		watchEvent, watchErr = monitorWithFsnotify(ctx, inboxNew)
	}

	if watchErr != nil {
		if errors.Is(watchErr, context.DeadlineExceeded) {
			if err := outputMonitorResult(common.JSON, monitorResult{
				Event:   "timeout",
				Mode:    mode,
				Me:      common.Me,
				Count:   0,
				Drained: []monitorItem{},
			}); err != nil {
				return err
			}
			return TimeoutError("monitor timed out")
		}
		return watchErr
	}

	// New message arrived - drain it
	items, err = collectInboxItems(root, common.Me, *includeBodyFlag, *limitFlag, validator)
	if err != nil {
		return err
	}
	if mode == "drain" {
		drainMonitorItems(root, common.Me, doAck, items)
	}

	result := monitorResult{
		Event:      "messages",
		WatchEvent: watchEvent,
		Mode:       mode,
		Me:         common.Me,
		Count:      len(items),
		Drained:    items,
	}

	if len(items) == 0 {
		result.Event = "empty"
	}

	return outputMonitorResult(common.JSON, result)
}

func drainMonitorItems(root, me string, doAck bool, items []monitorItem) {
	// Process each message: move to cur (or DLQ for parse errors), optionally ack
	for i := range items {
		item := &items[i]

		// Move parse errors to DLQ instead of cur
		if item.ParseError != "" {
			reason := item.FailureReason
			if reason == "" {
				reason = "parse_error"
			}
			if _, err := fsq.MoveToDLQ(root, me, item.Filename, item.ID, reason, item.ParseError); err != nil {
				_ = writeStderr("warning: failed to move %s to DLQ: %v\n", item.Filename, err)
			} else {
				item.MovedToDLQ = true
			}
			continue
		}

		// Move new -> cur
		if err := fsq.MoveNewToCur(root, me, item.Filename); err != nil {
			if os.IsNotExist(err) {
				// Likely moved by another drain; check if it's already in cur.
				curPath := filepath.Join(fsq.AgentInboxCur(root, me), item.Filename)
				if _, statErr := os.Stat(curPath); statErr == nil {
					item.MovedToCur = true
				} else if statErr != nil && !os.IsNotExist(statErr) {
					_ = writeStderr("warning: failed to stat %s in cur: %v\n", item.Filename, statErr)
				}
			} else {
				_ = writeStderr("warning: failed to move %s to cur: %v\n", item.Filename, err)
			}
		} else {
			item.MovedToCur = true
		}

		// Ack if required and move succeeded (gate acking on successful move)
		if doAck && item.AckRequired && item.ParseError == "" && item.MovedToCur {
			if err := ackMessage(root, me, item); err != nil {
				_ = writeStderr("warning: failed to ack %s: %v\n", item.ID, err)
			} else {
				item.Acked = true
			}
		}
	}
}

func monitorWithFsnotify(ctx context.Context, inboxNew string) (string, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return monitorWithPolling(ctx, inboxNew)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(inboxNew); err != nil {
		return monitorWithPolling(ctx, inboxNew)
	}

	// Check for existing messages AFTER setting up watcher to avoid race condition
	// (messages arriving between drain and watcher setup would be missed otherwise)
	hasMessages, err := hasMessageFiles(inboxNew)
	if err != nil {
		return "", err
	}
	if hasMessages {
		return "existing", nil
	}

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case event, ok := <-watcher.Events:
			if !ok {
				return "", errors.New("watcher closed")
			}
			if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				time.Sleep(10 * time.Millisecond)
				return "new_message", nil
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return "", errors.New("watcher closed")
			}
			return "", err
		}
	}
}

func monitorWithPolling(ctx context.Context, inboxNew string) (string, error) {
	// Check immediately first to avoid missing messages that arrived before polling started
	hasMessages, err := hasMessageFiles(inboxNew)
	if err != nil {
		return "", err
	}
	if hasMessages {
		return "existing", nil
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			hasMessages, err := hasMessageFiles(inboxNew)
			if err != nil {
				return "", err
			}
			if hasMessages {
				return "new_message", nil
			}
		}
	}
}

// hasMessageFiles checks if inbox/new contains any message files (.md, non-dotfile)
func hasMessageFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip dotfiles (like .DS_Store) and require .md suffix
		if strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(name, ".md") {
			return true, nil
		}
	}
	return false, nil
}

func outputMonitorResult(jsonOutput bool, result monitorResult) error {
	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}

	mode := result.Mode
	if mode == "" {
		mode = "drain"
	}

	switch result.Event {
	case "timeout":
		return writeStdoutLine("No new messages (timeout)")
	case "empty":
		if mode == "peek" {
			return writeStdoutLine("No messages available")
		}
		return writeStdoutLine("No messages to drain")
	case "messages":
		header := "[AMQ] %d message(s) for %s:\n\n"
		if mode == "peek" {
			header = "[AMQ] %d message(s) available for %s (peek):\n\n"
		}
		if err := writeStdout(header, result.Count, result.Me); err != nil {
			return err
		}
		for _, item := range result.Drained {
			// Handle corrupt/unparseable messages like drain does
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
			if err := writeStdout("- From: %s\n  ID: %s\n  Subject: %s\n  Priority: %s\n  Kind: %s\n  Thread: %s\n",
				item.From, item.ID, subject, priority, kind, item.Thread); err != nil {
				return err
			}
			if item.Body != "" {
				if err := writeStdout("  Body:\n%s\n", item.Body); err != nil {
					return err
				}
			}
			if err := writeStdout("---\n"); err != nil {
				return err
			}
		}
	}
	return nil
}
