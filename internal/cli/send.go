package cli

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	common := addCommonFlags(fs)
	toFlag := fs.String("to", "", "Receiver handle (comma-separated)")
	subjectFlag := fs.String("subject", "", "Message subject")
	threadFlag := fs.String("thread", "", "Thread id (optional; default p2p/<a>__<b>)")
	bodyFlag := fs.String("body", "", "Body string, @file, or empty to read stdin")
	ackFlag := fs.Bool("ack", false, "Request ack")
	refsFlag := fs.String("refs", "", "Comma-separated related message ids")

	usage := usageWithFlags(fs, "amq send --me <agent> --to <recipients> [options]")
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
	recipients, err := splitRecipients(*toFlag)
	if err != nil {
		return err
	}
	recipients = dedupeStrings(recipients)

	// Validate handles against config.json
	allHandles := append([]string{me}, recipients...)
	if err := validateKnownHandles(root, allHandles, common.Strict); err != nil {
		return err
	}

	body, err := readBody(*bodyFlag)
	if err != nil {
		return err
	}

	threadID := strings.TrimSpace(*threadFlag)
	if threadID == "" {
		if len(recipients) == 1 {
			threadID = canonicalP2P(common.Me, recipients[0])
		} else {
			return errors.New("--thread is required when sending to multiple recipients")
		}
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}

	msg := format.Message{
		Header: format.Header{
			Schema:      format.CurrentSchema,
			ID:          id,
			From:        common.Me,
			To:          recipients,
			Thread:      threadID,
			Subject:     strings.TrimSpace(*subjectFlag),
			Created:     now.UTC().Format(time.RFC3339Nano),
			AckRequired: *ackFlag,
			Refs:        splitList(*refsFlag),
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"
	// Deliver to each recipient.
	if _, err := fsq.DeliverToInboxes(root, recipients, filename, data); err != nil {
		return err
	}

	// Copy to sender outbox/sent for audit.
	outboxDir := fsq.AgentOutboxSent(root, common.Me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"id":      id,
			"thread":  threadID,
			"to":      recipients,
			"subject": msg.Header.Subject,
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		})
	}
	if outboxErr != nil {
		if err := writeStderr("warning: outbox write failed: %v\n", outboxErr); err != nil {
			return err
		}
	}
	if err := writeStdout("Sent %s to %s\n", id, strings.Join(recipients, ",")); err != nil {
		return err
	}
	return nil
}

func canonicalP2P(a, b string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return "p2p/" + a + "__" + b
	}
	if a < b {
		return "p2p/" + a + "__" + b
	}
	return "p2p/" + b + "__" + a
}
