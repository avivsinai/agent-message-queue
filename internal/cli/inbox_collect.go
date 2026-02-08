package cli

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// processInboxItems moves collected inbox items to cur (or DLQ for parse errors)
// and optionally sends acknowledgments. Used by both drain and monitor commands.
func processInboxItems(root, me string, doAck bool, items []inboxItem) {
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

		// Ack if required and move succeeded
		if doAck && item.AckRequired && item.ParseError == "" && item.MovedToCur {
			if err := ackMessage(root, me, item); err != nil {
				_ = writeStderr("warning: failed to ack %s: %v\n", item.ID, err)
			} else {
				item.Acked = true
			}
		}
	}
}

func collectInboxItems(root, me string, includeBody bool, limit int, validator *headerValidator) ([]inboxItem, error) {
	newDir := fsq.AgentInboxNew(root, me)
	entries, err := os.ReadDir(newDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []inboxItem{}, nil
		}
		return nil, err
	}

	if validator == nil {
		validator = &headerValidator{}
	}

	items := make([]inboxItem, 0, len(entries))
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

		baseID := strings.TrimSuffix(filename, ".md")
		item := inboxItem{
			ID:       baseID, // fallback to filename base if parse fails
			Filename: filename,
		}

		// Try to parse the message
		var header format.Header
		var body string
		var parseErr error

		if includeBody {
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
			item.FailureReason = "parse_error"
			// Still move corrupt message to DLQ to avoid reprocessing
		} else if err := validator.validate(header); err != nil {
			item.ParseError = "invalid header: " + err.Error()
			item.FailureReason = "invalid_header"
			if safeID, ok := safeHeaderID(header.ID); ok {
				item.ID = safeID
			}
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
			if includeBody {
				item.Body = body
			}
			if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
				item.SortKey = ts
			}
		}

		items = append(items, item)
	}

	format.SortByTimestamp(items)

	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}

	return items, nil
}
