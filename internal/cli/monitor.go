package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/fsnotify/fsnotify"
)

type monitorItem struct {
	ID          string         `json:"id"`
	From        string         `json:"from"`
	To          []string       `json:"to"`
	Thread      string         `json:"thread"`
	Subject     string         `json:"subject"`
	Created     string         `json:"created"`
	Body        string         `json:"body,omitempty"`
	AckRequired bool           `json:"ack_required"`
	Priority    string         `json:"priority,omitempty"`
	Kind        string         `json:"kind,omitempty"`
	Labels      []string       `json:"labels,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
	Acked       bool           `json:"acked"`
	Filename    string         `json:"-"`
	SortKey     time.Time      `json:"-"`
}

type monitorResult struct {
	Event      string        `json:"event"`                 // "messages", "timeout", "empty"
	WatchEvent string        `json:"watch_event,omitempty"` // "existing", "new_message", ""
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
	onceFlag := fs.Bool("once", false, "Exit after first non-empty drain (ideal for background watchers)")

	usage := usageWithFlags(fs, "amq monitor --me <agent> [options]",
		"Combined watch+drain: waits for messages, drains them, outputs structured payload.",
		"Ideal for co-op mode background watchers in Claude Code or Codex.")
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
		return err
	}
	common.Me = me
	root := filepath.Clean(common.Root)

	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	inboxNew := fsq.AgentInboxNew(root, common.Me)
	if err := os.MkdirAll(inboxNew, 0o700); err != nil {
		return err
	}

	// First, try to drain existing messages
	items, err := drainMessages(root, common.Me, *includeBodyFlag, *ackFlag, *limitFlag)
	if err != nil {
		return err
	}

	if len(items) > 0 {
		return outputMonitorResult(common.JSON, monitorResult{
			Event:      "messages",
			WatchEvent: "existing",
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
			return outputMonitorResult(common.JSON, monitorResult{
				Event:   "timeout",
				Me:      common.Me,
				Count:   0,
				Drained: []monitorItem{},
			})
		}
		return watchErr
	}

	// New message arrived - drain it
	items, err = drainMessages(root, common.Me, *includeBodyFlag, *ackFlag, *limitFlag)
	if err != nil {
		return err
	}

	result := monitorResult{
		Event:      "messages",
		WatchEvent: watchEvent,
		Me:         common.Me,
		Count:      len(items),
		Drained:    items,
	}

	if len(items) == 0 {
		result.Event = "empty"
	}

	if err := outputMonitorResult(common.JSON, result); err != nil {
		return err
	}

	// If --once is set, we're done. Otherwise, in a real implementation
	// we could loop, but for simplicity we exit after one cycle.
	// The caller (CC/Codex) should respawn the monitor.
	if *onceFlag {
		return nil
	}

	return nil
}

func drainMessages(root, me string, includeBody, doAck bool, limit int) ([]monitorItem, error) {
	newDir := fsq.AgentInboxNew(root, me)
	entries, err := os.ReadDir(newDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []monitorItem{}, nil
		}
		return nil, err
	}

	items := make([]monitorItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		path := filepath.Join(newDir, filename)

		item := monitorItem{
			ID:       filename,
			Filename: filename,
		}

		var header format.Header
		var body string

		if includeBody {
			msg, err := format.ReadMessageFile(path)
			if err != nil {
				continue // Skip corrupt messages
			}
			header = msg.Header
			body = msg.Body
		} else {
			var err error
			header, err = format.ReadHeaderFile(path)
			if err != nil {
				continue // Skip corrupt messages
			}
		}

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
		if includeBody {
			item.Body = body
		}
		if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
			item.SortKey = ts
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
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}

	// Process each message: move to cur, optionally ack
	for i := range items {
		item := &items[i]

		// Move new -> cur
		if err := fsq.MoveNewToCur(root, me, item.Filename); err != nil {
			if !os.IsNotExist(err) {
				_ = writeStderr("warning: failed to move %s to cur: %v\n", item.Filename, err)
			}
		}

		// Ack if required
		if doAck && item.AckRequired {
			if err := monitorAckMessage(root, me, item); err != nil {
				_ = writeStderr("warning: failed to ack %s: %v\n", item.ID, err)
			} else {
				item.Acked = true
			}
		}
	}

	return items, nil
}

func monitorAckMessage(root, me string, item *monitorItem) error {
	sender, err := normalizeHandle(item.From)
	if err != nil {
		return err
	}
	msgID, err := ensureSafeBaseName(item.ID)
	if err != nil {
		return err
	}

	ackPayload := ack.New(item.ID, item.Thread, me, sender, time.Now())

	receiverDir := fsq.AgentAcksSent(root, me)
	receiverPath := filepath.Join(receiverDir, msgID+".json")
	if existing, err := ack.Read(receiverPath); err == nil {
		ackPayload = existing
	}

	data, err := ackPayload.Marshal()
	if err != nil {
		return err
	}

	if _, err := fsq.WriteFileAtomic(receiverDir, msgID+".json", data, 0o600); err != nil {
		return err
	}

	// Best-effort write to sender's received acks
	senderDir := fsq.AgentAcksReceived(root, sender)
	_, _ = fsq.WriteFileAtomic(senderDir, msgID+".json", data, 0o600) // ignore error

	return nil
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
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			entries, err := os.ReadDir(inboxNew)
			if err != nil && !os.IsNotExist(err) {
				return "", err
			}
			for _, entry := range entries {
				if !entry.IsDir() {
					return "new_message", nil
				}
			}
		}
	}
}

func outputMonitorResult(jsonOutput bool, result monitorResult) error {
	if jsonOutput {
		return writeJSON(os.Stdout, result)
	}

	switch result.Event {
	case "timeout":
		return writeStdoutLine("No new messages (timeout)")
	case "empty":
		return writeStdoutLine("No messages to drain")
	case "messages":
		if err := writeStdout("[AMQ] %d message(s) for %s:\n\n", result.Count, result.Me); err != nil {
			return err
		}
		for _, item := range result.Drained {
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
