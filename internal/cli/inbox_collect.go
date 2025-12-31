package cli

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

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
