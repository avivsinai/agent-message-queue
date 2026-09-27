package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

var moveReadMessageToDLQ = fsq.MoveToDLQ

func runRead(args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message id")
	sessionFlag := fs.String("session", "", "Target session under the resolved base root")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, ignore a conflicting AM_SESSION pin")

	usage := usageWithFlags(fs, "amq read --me <agent> --id <msg_id> [--session <name>] [options]",
		"Read a message by id.",
		"",
		"If the message is in inbox/new, AMQ only moves it to inbox/cur after parse and header validation succeed.",
		"If the message in inbox/new is corrupt or malformed, AMQ moves it to DLQ and emits a dlq receipt.",
		"",
		"--id also resolves bridged messages by their header ID: messages delivered by amq-bridge keep",
		"the header ID but are stored under a transfer filename (xfer-<host>-<transfer>.md). If the ID",
		"matches more than one stored message, the command fails with a general error (exit 1) naming",
		"both paths; a missing message exits 3.",
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
	common.Me = me
	root, routed, err := resolveMailboxRoot(common, *sessionFlag)
	if err != nil {
		return err
	}
	if err := validatePinOverride(common, *ignoreSessionPinFlag, routed); err != nil {
		return err
	}
	if err := guardMailboxContext("read", root, routed, *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
		return err
	}
	deliveryIdentity, err := snapshotMailboxDeliveryRoot(root, routed, *ignoreSessionPinFlag)
	if err != nil {
		return err
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, deliveryIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryRoot.Close() }()
	if err := requireMailboxDeliveryRoot(deliveryRoot, root, me); err != nil {
		return err
	}

	// Validate handle against config.json
	if err := validateKnownHandlesDeliveryRoot(deliveryRoot, common.Strict, me); err != nil {
		return err
	}
	validator, err := newHeaderValidatorDeliveryRoot(deliveryRoot, common.Strict)
	if err != nil {
		return err
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return UsageError("--id: %v", err)
	}

	path, box, err := findMessageDeliveryRoot(deliveryRoot, common.Me, filename, false)
	if err != nil {
		if errors.Is(err, errMessageNotFound) {
			return NotFoundError("message not found: %s", *idFlag)
		}
		return err
	}
	// Header ID lookup can resolve a bridge transfer filename. Claims and DLQ
	// transitions must operate on that stored filename, not the requested ID.
	filename = filepath.Base(path)

	// Parse first before moving to avoid stuck corrupt messages in cur
	msg, err := readMessageDeliveryRoot(deliveryRoot, path)
	if err != nil {
		// If message is corrupt and in new, move to DLQ
		readErr := fmt.Errorf("failed to parse message %s: %w", *idFlag, err)
		if box == fsq.BoxNew {
			item, transitionErr := moveReadFailureToDLQ(
				deliveryRoot,
				common.Me,
				filename,
				*idFlag,
				"parse_error",
				err.Error(),
				nil,
			)
			return errors.Join(readErr, transitionErr, outputReadFailure(common.JSON, item))
		}
		return readErr
	}
	if err := validator.validate(msg.Header); err != nil {
		readErr := fmt.Errorf("invalid message header %s: %w", *idFlag, err)
		if box == fsq.BoxNew {
			item, transitionErr := moveReadFailureToDLQ(
				deliveryRoot,
				common.Me,
				filename,
				*idFlag,
				"invalid_header",
				"invalid header: "+err.Error(),
				&msg.Header,
			)
			return errors.Join(readErr, transitionErr, outputReadFailure(common.JSON, item))
		}
		return readErr
	}

	// Move to cur only after successful parse
	var claimErr error
	if box == fsq.BoxNew {
		claimErr = claimInboxNewToCur(deliveryRoot, common.Me, filename)
		if claimErr != nil {
			var committed *fsq.CommittedDurabilityError
			if !errors.As(claimErr, &committed) {
				return claimErr
			}
		}
		emitReceipt(deliveryRoot, common.Me, &inboxItem{
			ID:     msg.Header.ID,
			From:   msg.Header.From,
			Thread: msg.Header.Thread,
		}, receipt.StageDrained, "")
	}

	if common.JSON {
		return errors.Join(claimErr, writeJSON(os.Stdout, map[string]any{
			"header": msg.Header,
			"body":   msg.Body,
		}))
	}

	return errors.Join(claimErr, writeStdout("%s", msg.Body))
}

func moveReadFailureToDLQ(root *fsq.DeliveryRoot, me, filename, fallbackID, reason, detail string, header *format.Header) (*inboxItem, error) {
	dlqPath, transitionErr := moveReadMessageToDLQ(root, me, filename, fallbackID, reason, detail)
	item := &inboxItem{
		ID:            fallbackID,
		Filename:      filename,
		ParseError:    detail,
		FailureReason: reason,
	}
	if header != nil {
		if safeID, ok := safeHeaderID(header.ID); ok {
			item.ID = safeID
		}
		item.From = header.From
		item.Thread = header.Thread
	}

	var partial *fsq.DLQTransitionError
	if errors.As(transitionErr, &partial) {
		if dlqPath == "" {
			return nil, transitionErr
		}
		return item, transitionErr
	}

	var committed *fsq.CommittedDurabilityError
	if dlqPath == "" {
		if transitionErr == nil {
			return nil, nil
		}
		if errors.As(transitionErr, &committed) {
			expectedCur := root.DisplayPath(filepath.Join("agents", me, "inbox", "cur", filename))
			if filepath.Clean(committed.FinalPath) != filepath.Clean(expectedCur) {
				return nil, transitionErr
			}
		}
		return item, transitionErr
	}
	if transitionErr != nil && !errors.As(transitionErr, &committed) {
		return nil, transitionErr
	}

	item.MovedToDLQ = true
	emitReceipt(root, me, item, receipt.StageDLQ, detail)
	return item, transitionErr
}

func outputReadFailure(jsonOutput bool, item *inboxItem) error {
	if item == nil {
		return nil
	}
	if jsonOutput {
		return writeJSON(os.Stdout, item)
	}
	dlqNote := ""
	if item.MovedToDLQ {
		dlqNote = " [moved to DLQ]"
	}
	return writeStdout("- ID: %s\n  ERROR: %s%s\n---\n", item.ID, item.ParseError, dlqNote)
}
