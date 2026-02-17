package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/ack"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runAck(args []string) error {
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message id")

	usage := usageWithFlags(fs, "amq ack --me <agent> --id <msg_id> [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me
	root := resolveRoot(common.Root)

	// Validate handle against config.json
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}

	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return UsageError("--id: %v", err)
	}

	path, _, err := fsq.FindMessage(root, common.Me, filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("message not found: %s", *idFlag)
		}
		return err
	}

	header, err := format.ReadHeaderFile(path)
	if err != nil {
		return err
	}
	sender, err := normalizeHandle(header.From)
	if err != nil || sender != header.From {
		return fmt.Errorf("invalid sender handle in message: %s", header.From)
	}
	msgID, err := ensureSafeBaseName(header.ID)
	if err != nil || msgID != header.ID {
		return fmt.Errorf("invalid message id in message: %s", header.ID)
	}
	ackPayload := ack.New(header.ID, header.Thread, common.Me, sender, time.Now())
	receiverDir := fsq.AgentAcksSent(root, common.Me)
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

	// Best-effort write to sender's received acks; sender may not exist.
	senderDir := fsq.AgentAcksReceived(root, sender)
	senderPath := filepath.Join(senderDir, msgID+".json")
	if _, err := os.Stat(senderPath); err == nil {
		// Already recorded.
	} else if os.IsNotExist(err) {
		if _, err := fsq.WriteFileAtomic(senderDir, msgID+".json", data, 0o600); err != nil {
			if warnErr := writeStderr("warning: unable to write sender ack: %v\n", err); warnErr != nil {
				return warnErr
			}
		}
	} else if err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, ackPayload)
	}
	if err := writeStdout("Acked %s\n", header.ID); err != nil {
		return err
	}
	return nil
}
