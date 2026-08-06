package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

var claimInboxNewToCur = fsq.MoveNewToCur
var moveInboxCurToDLQ = fsq.MoveCurToDLQ
var moveClaimedInboxCurToDLQ = fsq.MoveClaimedCurToDLQ

const receiptSenderUnavailableDetail = "sender unavailable because message metadata could not be parsed"

// drainInboxItems claims inbox/new messages before parsing them, then emits
// output items and receipts only for messages this process actually claimed.
func drainInboxItems(deliveryRoot *fsq.DeliveryRoot, root, me string, includeBody bool, limit int, validator *headerValidator) ([]inboxItem, error) {
	return drainInboxItemsWithClaimHook(deliveryRoot, root, me, includeBody, limit, validator, nil)
}

func drainInboxItemsWithClaimHook(
	deliveryRoot *fsq.DeliveryRoot,
	root, me string,
	includeBody bool,
	limit int,
	validator *headerValidator,
	afterClaim func(string) error,
) ([]inboxItem, error) {
	var items []inboxItem
	err := deliveryRoot.WithPinnedBatch(func(batch *fsq.DeliveryRoot) error {
		var err error
		items, err = drainInboxItemsPinned(batch, root, me, includeBody, limit, validator, afterClaim)
		return err
	})
	return items, err
}

func drainInboxItemsPinned(
	deliveryRoot *fsq.DeliveryRoot,
	root, me string,
	includeBody bool,
	limit int,
	validator *headerValidator,
	afterClaim func(string) error,
) ([]inboxItem, error) {
	filenames, err := collectInboxFilenames(deliveryRoot, me)
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
		claimErr := claimInboxNewToCur(deliveryRoot, me, filename)
		var committedClaim *fsq.CommittedDurabilityError
		claimCommitted := errors.As(claimErr, &committedClaim)
		if claimErr != nil && !claimCommitted {
			if os.IsNotExist(claimErr) {
				exists, checkErr := claimMailboxDirsExist(deliveryRoot, me)
				if checkErr != nil {
					return finishInboxBatch(items, checkErr)
				}
				if exists {
					continue
				}
				return finishInboxBatch(items, NotFoundError("mailbox for %q disappeared while claiming %s at root %s", me, filename, root))
			}
			return finishInboxBatch(items, claimErr)
		}
		if afterClaim != nil {
			if err := afterClaim(filename); err != nil {
				items = append(items, failedClaimedInboxItem(filename, "processing_error", err))
				if claimCommitted {
					return finishInboxBatch(items, errors.Join(claimErr, err))
				}
				return finishInboxBatch(items, err)
			}
		}

		item, err := readInboxItem(
			deliveryRoot,
			filepath.Join("agents", me, "inbox", "cur", filename),
			filename,
			includeBody,
			validator,
		)
		if err != nil {
			// The claim already committed. Preserve that outcome in the batch
			// instead of returning an error that would hide this and all earlier
			// claimed messages from the caller. The existing parse-failure path
			// attempts DLQ placement and reports the retained claim if that
			// placement cannot read the artifact either.
			item.ParseError = err.Error()
			item.FailureReason = "parse_error"
		}

		// Move parse errors to DLQ instead of cur
		if item.ParseError != "" {
			reason := item.FailureReason
			if reason == "" {
				reason = "parse_error"
			}
			var dlqPath string
			var dlqErr error
			if claimCommitted {
				dlqPath, dlqErr = moveClaimedInboxCurToDLQ(
					deliveryRoot,
					me,
					item.Filename,
					item.ID,
					reason,
					item.ParseError,
					claimErr,
				)
			} else {
				dlqPath, dlqErr = moveInboxCurToDLQ(
					deliveryRoot,
					me,
					item.Filename,
					item.ID,
					reason,
					item.ParseError,
				)
			}
			if dlqErr == nil {
				item.MovedToDLQ = true
				emitReceipt(deliveryRoot, me, &item, receipt.StageDLQ, item.ParseError)
				items = append(items, item)
				continue
			} else {
				var partial *fsq.DLQTransitionError
				var committed *fsq.CommittedDurabilityError
				switch {
				case errors.As(dlqErr, &partial):
					// The DLQ envelope is visible but the claimed source remains
					// in cur. Surface the item before returning a typed error so
					// callers cannot mistake the duplicate state for completion.
					items = append(items, item)
					return finishInboxBatch(items, dlqErr)
				case dlqPath != "" && errors.As(dlqErr, &committed):
					// The source transition completed, but its durability is
					// indeterminate. Report the completed logical outcome and
					// still fail the command so operators do not retry blindly.
					item.MovedToDLQ = true
					emitReceipt(deliveryRoot, me, &item, receipt.StageDLQ, item.ParseError)
					items = append(items, item)
					return finishInboxBatch(items, dlqErr)
				default:
					// No DLQ envelope is visible and the claimed source remains
					// in cur. Preserve the item, but fail the batch instead of
					// reducing a transport failure to a warning-only success.
					items = append(items, item)
					return finishInboxBatch(items, dlqErr)
				}
			}
		}

		item.MovedToCur = true
		emitReceipt(deliveryRoot, me, &item, receipt.StageDrained, "")
		items = append(items, item)
		if claimCommitted {
			return finishInboxBatch(items, claimErr)
		}
	}

	return finishInboxBatch(items, nil)
}

func finishInboxBatch(items []inboxItem, err error) ([]inboxItem, error) {
	format.SortByTimestamp(items)
	return items, err
}

func failedClaimedInboxItem(filename, reason string, err error) inboxItem {
	return inboxItem{
		ID:            strings.TrimSuffix(filename, ".md"),
		Filename:      filename,
		ParseError:    err.Error(),
		FailureReason: reason,
	}
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
	sender := ""
	if strings.TrimSpace(item.From) != "" {
		var err error
		sender, err = normalizeHandle(item.From)
		if err != nil {
			sender = ""
			_ = writeStderr("warning: receipt sender normalization failed for %q: %v\n", item.From, err)
		}
	} else if stage == receipt.StageDLQ {
		if strings.TrimSpace(detail) == "" {
			detail = receiptSenderUnavailableDetail
		} else {
			detail += "; " + receiptSenderUnavailableDetail
		}
	}

	r := receipt.New(item.ID, item.Thread, sender, consumer, stage, detail)
	if err := receipt.EmitDeliveryRoot(root, r); err != nil {
		_ = writeStderr("warning: failed to emit %s receipt for %s: %v\n", stage, item.ID, err)
	}
}

func collectInboxItems(
	deliveryRoot *fsq.DeliveryRoot,
	me string,
	includeBody bool,
	limit int,
	validator *headerValidator,
	revalidateContext func() error,
) ([]inboxItem, error) {
	var items []inboxItem
	err := deliveryRoot.WithPinnedBatch(func(batch *fsq.DeliveryRoot) error {
		var err error
		items, err = collectInboxItemsPinned(batch, me, includeBody, limit, validator, revalidateContext)
		return err
	})
	return items, err
}

func collectInboxItemsPinned(
	deliveryRoot *fsq.DeliveryRoot,
	me string,
	includeBody bool,
	limit int,
	validator *headerValidator,
	revalidateContext func() error,
) ([]inboxItem, error) {
	filenames, err := collectInboxFilenames(deliveryRoot, me)
	if err != nil {
		return nil, err
	}

	if validator == nil {
		validator = &headerValidator{}
	}

	items := make([]inboxItem, 0, len(filenames))
	for _, filename := range filenames {
		if revalidateContext != nil {
			if err := revalidateContext(); err != nil {
				return nil, err
			}
		}
		item, err := readInboxItem(
			deliveryRoot,
			filepath.Join("agents", me, "inbox", "new", filename),
			filename,
			includeBody,
			validator,
		)
		if err != nil {
			// Another consumer may claim the message after this peek enumerates
			// inbox/new. That is a normal race, not a monitor failure.
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		items = append(items, item)
	}

	format.SortByTimestamp(items)

	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}

	return items, nil
}

func collectInboxFilenames(root *fsq.DeliveryRoot, me string) ([]string, error) {
	entries, err := root.ReadDir(filepath.Join("agents", me, "inbox", "new"))
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

func readInboxItem(
	root *fsq.DeliveryRoot,
	path, filename string,
	includeBody bool,
	validator *headerValidator,
) (inboxItem, error) {
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
		info, err := root.Stat(path)
		if err != nil {
			return item, err
		}
		if info.Size() > format.MaxMessageSize {
			parseErr = fmt.Errorf("%w: %d bytes", format.ErrMessageTooLarge, info.Size())
		} else if data, err := root.ReadRegularNoFollow(path); err != nil {
			return item, err
		} else if len(data) > format.MaxMessageSize {
			parseErr = fmt.Errorf("%w: %d bytes", format.ErrMessageTooLarge, len(data))
		} else if msg, err := format.ParseMessage(data); err != nil {
			parseErr = err
		} else {
			header = msg.Header
			body = msg.Body
		}
	} else {
		file, _, err := root.OpenRegularNoFollow(path)
		if err != nil {
			return item, err
		}
		header, parseErr = format.ReadHeader(file)
		if err := file.Close(); err != nil && parseErr == nil {
			return item, err
		}
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

	return item, nil
}
