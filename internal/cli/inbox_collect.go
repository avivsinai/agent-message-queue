package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

// drainInboxItems claims inbox/new messages before parsing them, then emits
// output items and receipts only for messages this process actually claimed.
func drainInboxItems(deliveryRoot *fsq.DeliveryRoot, root, me string, includeBody bool, limit int, validator *headerValidator) ([]inboxItem, error) {
	filenames, err := collectInboxFilenames(root, me)
	if err != nil {
		return nil, err
	}

	if validator == nil {
		validator = &headerValidator{}
	}

	items := make([]inboxItem, 0, len(filenames))
	for _, filename := range filenames {
		if limit > 0 && len(items) >= limit {
			break
		}
		if err := fsq.MoveNewToCur(deliveryRoot, me, filename); err != nil {
			if os.IsNotExist(err) {
				exists, checkErr := claimMailboxDirsExist(deliveryRoot, me)
				if checkErr != nil {
					return nil, checkErr
				}
				if exists {
					continue
				}
				return nil, NotFoundError("mailbox for %q disappeared while claiming %s at root %s", me, filename, root)
			}
			_ = writeStderr("warning: failed to claim %s: %v\n", filename, err)
			continue
		}

		item := readInboxItem(filepath.Join(fsq.AgentInboxCur(root, me), filename), filename, includeBody, validator)

		// Move parse errors to DLQ instead of cur
		if item.ParseError != "" {
			reason := item.FailureReason
			if reason == "" {
				reason = "parse_error"
			}
			if _, err := fsq.MoveCurToDLQ(deliveryRoot, me, item.Filename, item.ID, reason, item.ParseError); err != nil {
				_ = writeStderr("warning: failed to move %s to DLQ: %v\n", item.Filename, err)
			} else {
				item.MovedToDLQ = true
				emitReceipt(deliveryRoot, me, &item, receipt.StageDLQ, item.ParseError)
			}
			items = append(items, item)
			continue
		}

		item.MovedToCur = true
		emitReceipt(deliveryRoot, me, &item, receipt.StageDrained, "")
		items = append(items, item)
	}

	format.SortByTimestamp(items)
	return items, nil
}

func claimMailboxDirsExist(root *fsq.DeliveryRoot, me string) (bool, error) {
	for _, dir := range []string{filepath.Join("agents", me, "inbox", "new"), filepath.Join("agents", me, "inbox", "cur")} {
		info, err := root.Stat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		if !info.IsDir() {
			return false, nil
		}
	}
	return true, nil
}

func emitReceipt(root *fsq.DeliveryRoot, consumer string, item *inboxItem, stage, detail string) {
	sender, err := normalizeHandle(item.From)
	if err != nil {
		sender = ""
		_ = writeStderr("warning: receipt sender normalization failed for %q: %v\n", item.From, err)
	}

	r := receipt.New(item.ID, item.Thread, sender, consumer, stage, detail)
	if err := receipt.EmitDeliveryRoot(root, r); err != nil {
		_ = writeStderr("warning: failed to emit %s receipt for %s: %v\n", stage, item.ID, err)
	}
}

func collectInboxItems(root, me string, includeBody bool, limit int, validator *headerValidator) ([]inboxItem, error) {
	filenames, err := collectInboxFilenames(root, me)
	if err != nil {
		return nil, err
	}

	if validator == nil {
		validator = &headerValidator{}
	}

	items := make([]inboxItem, 0, len(filenames))
	for _, filename := range filenames {
		path := filepath.Join(fsq.AgentInboxNew(root, me), filename)
		items = append(items, readInboxItem(path, filename, includeBody, validator))
	}

	format.SortByTimestamp(items)

	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}

	return items, nil
}

func collectInboxFilenames(root, me string) ([]string, error) {
	newDir := fsq.AgentInboxNew(root, me)
	entries, err := os.ReadDir(newDir)
	if err != nil {
		return nil, err
	}

	filenames := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filename := entry.Name()
		// Skip dotfiles (like .DS_Store) and non-.md files
		if strings.HasPrefix(filename, ".") || !strings.HasSuffix(filename, ".md") {
			continue
		}
		filenames = append(filenames, filename)
	}
	sort.Strings(filenames)
	return filenames, nil
}

func readInboxItem(path, filename string, includeBody bool, validator *headerValidator) inboxItem {
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
		// Still move corrupt message to DLQ to avoid reprocessing.
	} else if err := validator.validate(header); err != nil {
		item.From = header.From
		item.Thread = header.Thread
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
		item.Priority = header.Priority
		item.Kind = header.Kind
		item.Labels = header.Labels
		item.Context = header.Context
		item.FromProject = header.FromProject
		item.ReplyProject = header.ReplyProject
		if includeBody {
			item.Body = body
		}
		if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
			item.SortKey = ts
		}
	}

	return item
}
