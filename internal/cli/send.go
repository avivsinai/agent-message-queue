package cli

import (
	"flag"
	"fmt"
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

	// Co-op mode flags
	priorityFlag := fs.String("priority", "", "Message priority: urgent, normal, low (default: normal if kind set)")
	kindFlag := fs.String("kind", "", "Message kind: "+format.ValidKindsList())
	labelsFlag := fs.String("labels", "", "Comma-separated labels/tags")
	contextFlag := fs.String("context", "", "JSON context object or @file.json")

	// Cross-session flag
	sessionFlag := fs.String("session", "", "Target session (delivers to a different session's inbox)")

	usage := usageWithFlags(fs, "amq send --me <agent> --to <recipients> [--session <name>] [options]",
		"",
		"Cross-session example:",
		"  amq send --to codex --session auth --body \"Heads up, auth module changed\"",
	)
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
	recipients, err := splitRecipients(*toFlag)
	if err != nil {
		if _, ok := err.(*ExitCodeError); ok {
			return err
		}
		return UsageError("--to: %v", err)
	}
	recipients = dedupeStrings(recipients)

	// Determine delivery root: current session or a target session.
	deliveryRoot := root
	targetSession := strings.TrimSpace(*sessionFlag)
	if targetSession != "" {
		normalized, err := normalizeHandle(targetSession)
		if err != nil {
			return UsageError("--session: %v", err)
		}
		targetSession = normalized

		// Resolve the target session root from the base root.
		baseRoot := resolveBaseRootForSend(root)
		deliveryRoot = filepath.Join(baseRoot, targetSession)

		// Verify the target session exists (never auto-create foreign mailboxes).
		if !dirExists(deliveryRoot) {
			return fmt.Errorf("session %q not found at %s", targetSession, deliveryRoot)
		}
		for _, r := range recipients {
			inbox := filepath.Join(deliveryRoot, "agents", r, "inbox")
			if !dirExists(inbox) {
				return fmt.Errorf("agent %q not found in session %q", r, targetSession)
			}
		}
	}

	// Validate handles against config.json in the DELIVERY root.
	allHandles := append([]string{me}, recipients...)
	if err := validateKnownHandles(deliveryRoot, common.Strict, allHandles...); err != nil {
		// For cross-session, also try validating sender against the source root.
		if targetSession != "" {
			if err2 := validateKnownHandles(root, common.Strict, me); err2 != nil {
				return err
			}
			// Sender is known in source; recipients validated in target.
			if err3 := validateKnownHandles(deliveryRoot, common.Strict, recipients...); err3 != nil {
				return err3
			}
		} else {
			return err
		}
	}

	body, err := readBody(*bodyFlag)
	if err != nil {
		return err
	}

	// Validate and process co-op mode fields
	priority := strings.TrimSpace(*priorityFlag)
	kind := strings.TrimSpace(*kindFlag)
	if !format.IsValidPriority(priority) {
		return UsageError("--priority must be one of: urgent, normal, low")
	}
	if !format.IsValidKind(kind) {
		return UsageError("--kind must be one of: %s", format.ValidKindsList())
	}
	// Default priority to "normal" if kind is set but priority is not
	if kind != "" && priority == "" {
		priority = format.PriorityNormal
	}

	labels := splitList(*labelsFlag)

	var context map[string]any
	if *contextFlag != "" {
		var err error
		context, err = parseContext(*contextFlag)
		if err != nil {
			return err
		}
	}

	threadID := strings.TrimSpace(*threadFlag)
	if threadID == "" {
		if len(recipients) == 1 {
			threadID = canonicalP2P(common.Me, recipients[0])
		} else {
			return UsageError("--thread is required when sending to multiple recipients")
		}
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}

	// Build reply_to for cross-session sends so replies route back.
	replyTo := ""
	if targetSession != "" {
		senderSession := sessionName(root)
		replyTo = common.Me + "@" + senderSession
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
			Priority:    priority,
			Kind:        kind,
			Labels:      labels,
			Context:     context,
			ReplyTo:     replyTo,
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"
	// Deliver to each recipient in the delivery root.
	if _, err := fsq.DeliverToInboxes(deliveryRoot, recipients, filename, data); err != nil {
		return err
	}

	// Copy to sender outbox/sent for audit (always in sender's root).
	outboxDir := fsq.AgentOutboxSent(root, common.Me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	session := sessionName(root)
	targetDisplay := session
	if targetSession != "" {
		targetDisplay = targetSession
	}
	if common.JSON {
		out := map[string]any{
			"id":      id,
			"thread":  threadID,
			"to":      recipients,
			"subject": msg.Header.Subject,
			"session": targetDisplay,
			"root":    deliveryRoot,
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		}
		if targetSession != "" {
			out["cross_session"] = true
			out["source_session"] = session
			out["target_session"] = targetSession
		}
		return writeJSON(os.Stdout, out)
	}
	if outboxErr != nil {
		if err := writeStderr("warning: outbox write failed: %v\n", outboxErr); err != nil {
			return err
		}
	}
	if err := writeStdout("Sent %s to %s (session: %s, root: %s)\n", id, strings.Join(recipients, ","), targetDisplay, deliveryRoot); err != nil {
		return err
	}
	return nil
}

// resolveBaseRootForSend derives the base root (parent of sessions) from the
// current root. It checks AM_BASE_ROOT first, then tries two heuristics:
// 1. If root itself contains session-like subdirs (agents/ dirs), root IS a session → parent is base
// 2. Otherwise, root might BE the base root → use it directly
func resolveBaseRootForSend(root string) string {
	// Check env var first (set by coop exec).
	if base := strings.TrimSpace(os.Getenv(envBaseRoot)); base != "" {
		return base
	}

	// Heuristic: if root has an "agents/" dir, it's a session root.
	// The base root is its parent.
	if dirExists(filepath.Join(root, "agents")) {
		return filepath.Dir(root)
	}

	// Otherwise root might be the base root itself.
	return root
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
