package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

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
	root := filepath.Clean(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandle(root, me, common.Strict); err != nil {
		return err
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return err
	}

	path, box, err := fsq.FindMessage(root, common.Me, filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("message not found: %s", *idFlag)
		}
		return err
	}

	if box == fsq.BoxNew {
		if err := fsq.MoveNewToCur(root, common.Me, filename); err != nil {
			return err
		}
		path = filepath.Join(fsq.AgentInboxCur(root, common.Me), filename)
	}

	msg, err := format.ReadMessageFile(path)
	if err != nil {
		return err
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
