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
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

func runReply(args []string) error {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message ID to reply to")
	bodyFlag := fs.String("body", "", "Body string, @file, or empty to read stdin")
	subjectFlag := fs.String("subject", "", "Override subject (default: Re: <original>)")
	ackFlag := fs.Bool("ack", false, "Request ack for the reply")

	// Co-op mode flags
	priorityFlag := fs.String("priority", "", "Message priority: urgent, normal, low")
	kindFlag := fs.String("kind", "", fmt.Sprintf("Message kind: %s (default: same as original, review_response for review_request, answer for question)", format.ValidKindsList()))
	labelsFlag := fs.String("labels", "", "Comma-separated labels/tags")
	contextFlag := fs.String("context", "", "JSON context object or @file.json")

	usage := usageWithFlags(fs, "amq reply --me <agent> --id <msg_id> [options]",
		"Reply to a message with automatic thread/refs handling.",
		"Finds the original message, sets to/thread/refs automatically.",
		"Cross-session replies are routed via reply_to header.",
		"To follow up on a sent cross-session message, use amq send --session instead.")
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
	root := resolveRoot(common.Root)

	if *idFlag == "" {
		return UsageError("--id is required")
	}

	// Validate co-op fields
	priority := strings.TrimSpace(*priorityFlag)
	kind := strings.TrimSpace(*kindFlag)
	if !format.IsValidPriority(priority) {
		return UsageError("--priority must be one of: urgent, normal, low")
	}
	if !format.IsValidKind(kind) {
		return UsageError("--kind must be one of: %s", format.ValidKindsList())
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

	// Find the original message
	originalMsg, originalPath, err := findMessage(root, me, *idFlag)
	if err != nil {
		return err
	}

	// Disallow reply on sent cross-session/cross-project messages.
	// If the message was found in outbox/sent and has a reply_to, the user
	// is trying to follow up on their own send. This is under-specified
	// (no target metadata in outbox copy).
	originalBox := filepath.Base(filepath.Dir(originalPath))
	if originalBox == "sent" && originalMsg.Header.ReplyTo != "" {
		return fmt.Errorf("cannot reply to a sent cross-session/cross-project message (reply_to points back to you); use amq send --session/--project for follow-ups")
	}

	body, err := readBody(*bodyFlag)
	if err != nil {
		return err
	}

	// Determine recipient and delivery root.
	var recipient string
	var deliveryRoot string
	var targetSession string
	var targetProject string

	if originalMsg.Header.ReplyProject != "" && originalMsg.Header.ReplyTo != "" {
		// Cross-project reply: route via peer lookup.
		targetProject = originalMsg.Header.ReplyProject

		// reply_to is either "handle@session" (cross-project+session) or "handle" (base root).
		parts := strings.SplitN(originalMsg.Header.ReplyTo, "@", 2)
		recipientNorm, err := normalizeHandle(parts[0])
		if err != nil {
			return fmt.Errorf("invalid handle in reply_to %q: %v", originalMsg.Header.ReplyTo, err)
		}
		recipient = recipientNorm

		peerBaseRoot, err := resolvePeer(root, targetProject)
		if err != nil {
			return err
		}

		if len(parts) == 2 && parts[1] != "" {
			// Cross-project + session: deliver to peer's session.
			sessionNorm, err := normalizeHandle(parts[1])
			if err != nil {
				return fmt.Errorf("invalid session in reply_to %q: %v", originalMsg.Header.ReplyTo, err)
			}
			targetSession = sessionNorm
			deliveryRoot = filepath.Join(peerBaseRoot, targetSession)
		} else {
			// Cross-project, base root: deliver to peer's base root.
			deliveryRoot = peerBaseRoot
		}

		if !dirExists(deliveryRoot) {
			if targetSession != "" {
				return fmt.Errorf("session %q not found in peer %q at %s", targetSession, targetProject, deliveryRoot)
			}
			return fmt.Errorf("peer %q root does not exist at %s", targetProject, deliveryRoot)
		}
		inbox := filepath.Join(deliveryRoot, "agents", recipient, "inbox")
		if !dirExists(inbox) {
			if targetSession != "" {
				return fmt.Errorf("agent %q not found in peer %q session %q", recipient, targetProject, targetSession)
			}
			return fmt.Errorf("agent %q not found in peer %q", recipient, targetProject)
		}
	} else if originalMsg.Header.ReplyTo != "" {
		// Cross-session reply (no cross-project). Strict reply_to parsing.
		parts := strings.SplitN(originalMsg.Header.ReplyTo, "@", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("malformed reply_to %q: expected handle@session", originalMsg.Header.ReplyTo)
		}
		recipientNorm, err := normalizeHandle(parts[0])
		if err != nil {
			return fmt.Errorf("invalid handle in reply_to %q: %v", originalMsg.Header.ReplyTo, err)
		}
		sessionNorm, err := normalizeHandle(parts[1])
		if err != nil {
			return fmt.Errorf("invalid session in reply_to %q: %v", originalMsg.Header.ReplyTo, err)
		}
		recipient = recipientNorm
		targetSession = sessionNorm

		// Use classifyRoot for consistent root resolution.
		baseRoot := classifyRoot(root)
		if baseRoot == "" {
			return fmt.Errorf("cannot route cross-session reply: run from inside 'amq coop exec --session <name>'")
		}
		deliveryRoot = filepath.Join(baseRoot, targetSession)
		if !dirExists(deliveryRoot) {
			return fmt.Errorf("reply_to session %q not found at %s", targetSession, deliveryRoot)
		}
		// Verify recipient inbox exists in target session.
		inbox := filepath.Join(deliveryRoot, "agents", recipient, "inbox")
		if !dirExists(inbox) {
			return fmt.Errorf("agent %q not found in session %q", recipient, targetSession)
		}
	} else {
		// Local reply: send to original sender in current session.
		rawRecipient := originalMsg.Header.From
		if rawRecipient == me {
			if len(originalMsg.Header.To) > 0 {
				rawRecipient = originalMsg.Header.To[0]
			} else {
				return fmt.Errorf("cannot determine recipient for reply")
			}
		}
		recipientNorm, err := normalizeHandle(rawRecipient)
		if err != nil {
			return fmt.Errorf("invalid recipient handle in original message: %q", rawRecipient)
		}
		if recipientNorm != rawRecipient {
			return fmt.Errorf("invalid recipient handle in original message: %q (normalized to %q)", rawRecipient, recipientNorm)
		}
		recipient = recipientNorm
		deliveryRoot = root

		// Verify the recipient's inbox exists locally.
		inbox := filepath.Join(deliveryRoot, "agents", recipient, "inbox")
		if !dirExists(inbox) {
			return fmt.Errorf("agent %q not found in current session (no reply_to in original message — sender may not be in a session)", recipient)
		}
	}

	// Validate recipient in delivery root.
	if err := validateKnownHandles(deliveryRoot, common.Strict, recipient); err != nil {
		return err
	}

	// Determine subject
	subject := strings.TrimSpace(*subjectFlag)
	if subject == "" {
		origSubject := originalMsg.Header.Subject
		if origSubject == "" {
			subject = "Re: (no subject)"
		} else if !strings.HasPrefix(strings.ToLower(origSubject), "re:") {
			subject = "Re: " + origSubject
		} else {
			subject = origSubject
		}
	}

	// Determine kind for reply
	if kind == "" {
		switch originalMsg.Header.Kind {
		case format.KindReviewRequest:
			kind = format.KindReviewResponse
		case format.KindQuestion:
			kind = format.KindAnswer
		default:
			kind = originalMsg.Header.Kind
		}
	}
	if kind != "" && priority == "" {
		priority = format.PriorityNormal
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
			From:        me,
			To:          []string{recipient},
			Thread:      originalMsg.Header.Thread,
			Subject:     subject,
			Created:     now.UTC().Format(time.RFC3339Nano),
			AckRequired: *ackFlag,
			Refs:        append(originalMsg.Header.Refs, originalMsg.Header.ID),
			Priority:    priority,
			Kind:        kind,
			Labels:      labels,
			Context:     context,
			// Restamp ReplyTo/ReplyProject so the recipient can reply back.
			ReplyTo: func() string {
				if targetSession != "" {
					return me + "@" + sessionName(root)
				}
				if targetProject != "" {
					return me // base-root cross-project: just handle
				}
				return ""
			}(),
			ReplyProject: func() string {
				if targetProject != "" {
					return resolveProject(root)
				}
				return ""
			}(),
			FromProject: func() string {
				if targetProject != "" {
					return resolveProject(root)
				}
				return ""
			}(),
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"
	if targetProject != "" {
		// Cross-project: use DeliverToExistingInbox (never creates dirs in peer).
		if _, err := fsq.DeliverToExistingInbox(deliveryRoot, recipient, filename, data); err != nil {
			return err
		}
	} else {
		if _, err := fsq.DeliverToInboxes(deliveryRoot, []string{recipient}, filename, data); err != nil {
			return err
		}
	}

	// Best-effort presence touch.
	_ = presence.Touch(root, me)

	outboxDir := fsq.AgentOutboxSent(root, me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	// Fix 5: Report target session in reply output (consistent with send).
	session := sessionName(root)
	targetDisplay := session
	if targetSession != "" {
		targetDisplay = targetSession
	}
	if common.JSON {
		out := map[string]any{
			"id":           id,
			"thread":       msg.Header.Thread,
			"to":           []string{recipient},
			"subject":      subject,
			"session":      targetDisplay,
			"root":         deliveryRoot,
			"in_reply_to":  originalMsg.Header.ID,
			"original_box": originalBox,
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		}
		if targetProject != "" {
			out["cross_project"] = true
			out["source_project"] = resolveProject(root)
			out["target_project"] = targetProject
		}
		if targetSession != "" {
			out["cross_session"] = true
			out["source_session"] = session
			out["target_session"] = targetSession
		}
		return writeJSON(os.Stdout, out)
	}

	if outboxErr != nil {
		_ = writeStderr("warning: outbox write failed: %v\n", outboxErr)
	}
	return writeStdout("Replied %s to %s (session: %s, root: %s)\n", id, recipient, targetDisplay, deliveryRoot)
}

// findMessage searches for a message by ID in the agent's inbox (new and cur).
func findMessage(root, me, msgID string) (format.Message, string, error) {
	filename, err := ensureFilename(msgID)
	if err != nil {
		return format.Message{}, "", err
	}

	newPath := filepath.Join(fsq.AgentInboxNew(root, me), filename)
	if msg, err := format.ReadMessageFile(newPath); err == nil {
		return msg, newPath, nil
	}

	curPath := filepath.Join(fsq.AgentInboxCur(root, me), filename)
	if msg, err := format.ReadMessageFile(curPath); err == nil {
		return msg, curPath, nil
	}

	sentPath := filepath.Join(fsq.AgentOutboxSent(root, me), filename)
	if msg, err := format.ReadMessageFile(sentPath); err == nil {
		return msg, sentPath, nil
	}

	return format.Message{}, "", fmt.Errorf("message not found: %s", msgID)
}
