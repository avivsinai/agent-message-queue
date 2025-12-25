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
	fs.SetOutput(os.Stdout)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message id")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return err
	}
	common.Me = me
	filename, err := ensureFilename(*idFlag)
	if err != nil {
		return err
	}
	root := filepath.Clean(common.Root)

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
	ackPayload := ack.New(header.ID, header.Thread, common.Me, header.From, time.Now())
	data, err := ackPayload.Marshal()
	if err != nil {
		return err
	}

	receiverDir := fsq.AgentAcksSent(root, common.Me)
	if _, err := fsq.WriteFileAtomic(receiverDir, header.ID+".json", data, 0o644); err != nil {
		return err
	}

	// Best-effort write to sender's received acks; sender may not exist.
	senderDir := fsq.AgentAcksReceived(root, header.From)
	if _, err := fsq.WriteFileAtomic(senderDir, header.ID+".json", data, 0o644); err != nil {
		if warnErr := writeStderr("warning: unable to write sender ack: %v\n", err); warnErr != nil {
			return warnErr
		}
	}

	if common.JSON {
		return writeJSON(os.Stdout, ackPayload)
	}
	if err := writeStdout("Acked %s\n", header.ID); err != nil {
		return err
	}
	return nil
}
