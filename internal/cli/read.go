package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runRead(args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message id")

	usage := usageWithFlags(fs, "amq read --me <agent> --id <msg_id> [options]")
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
	root := resolveRoot(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return err
	}

	path, box, err := fsq.FindMessage(root, common.Me, filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NotFoundError("message not found: %s", *idFlag)
		}
		return err
	}

	// Parse first before moving to avoid stuck corrupt messages in cur
	msg, err := format.ReadMessageFile(path)
	if err != nil {
		// If message is corrupt and in new, move to DLQ
		if box == fsq.BoxNew {
			if _, dlqErr := fsq.MoveToDLQ(root, common.Me, filename, *idFlag, "parse_error", err.Error()); dlqErr != nil {
				_ = writeStderr("warning: failed to move corrupt message to DLQ: %v\n", dlqErr)
			}
		}
		return fmt.Errorf("failed to parse message %s: %w", *idFlag, err)
	}

	// Move to cur only after successful parse
	if box == fsq.BoxNew {
		if err := fsq.MoveNewToCur(root, common.Me, filename); err != nil {
			return err
		}
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"header": msg.Header,
			"body":   msg.Body,
		})
	}

	if err := writeStdout("%s", msg.Body); err != nil {
		return err
	}
	return nil
}
