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
	"github.com/avivsinai/agent-message-queue/internal/resolve"
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
		"Finds the original message, sets to/thread/refs automatically.")
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

	// Read the body for the reply
	body, err := readBody(*bodyFlag)
	if err != nil {
		return err
	}

	// Determine kind for reply
	if kind == "" {
		// Auto-set kind based on original
		switch originalMsg.Header.Kind {
		case format.KindReviewRequest:
			kind = format.KindReviewResponse
		case format.KindQuestion:
			kind = format.KindAnswer
		default:
			kind = originalMsg.Header.Kind // Keep same kind
		}
	}

	// Default priority to normal if kind is set
	if kind != "" && priority == "" {
		priority = format.PriorityNormal
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

	// Check if the original message has Origin.ReplyTo set (federation reply).
	if originalMsg.Header.Origin != nil && originalMsg.Header.Origin.ReplyTo != "" {
		return runReplyFederated(common, root, me, originalMsg, originalPath,
			subject, body, *ackFlag, priority, kind, labels, context)
	}

	// --- Legacy local reply path (unchanged) ---
	// Determine recipient (original sender) with path-safety validation
	rawRecipient := originalMsg.Header.From
	if rawRecipient == me {
		// Replying to our own message - use the original recipient
		if len(originalMsg.Header.To) > 0 {
			rawRecipient = originalMsg.Header.To[0]
		} else {
			return fmt.Errorf("cannot determine recipient for reply")
		}
	}

	// Normalize and validate recipient handle to prevent path traversal
	// This is critical: untrusted header values must not become filesystem paths
	recipientNorm, err := normalizeHandle(rawRecipient)
	if err != nil {
		return fmt.Errorf("invalid recipient handle in original message: %q", rawRecipient)
	}
	if recipientNorm != rawRecipient {
		return fmt.Errorf("invalid recipient handle in original message: %q (normalized to %q)", rawRecipient, recipientNorm)
	}
	recipient := recipientNorm

	// Validate handles exist in config (if strict mode)
	if err := validateKnownHandles(root, common.Strict, me, recipient); err != nil {
		return err
	}

	// Create the reply message
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
			// Refs grows with chain length (each reply appends the parent ID).
			// This is acceptable for agent conversations which are short-lived.
			Refs:     append(originalMsg.Header.Refs, originalMsg.Header.ID),
			Priority: priority,
			Kind:     kind,
			Labels:   labels,
			Context:  context,
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"

	// Deliver to recipient
	if _, err := fsq.DeliverToInboxes(root, []string{recipient}, filename, data); err != nil {
		return err
	}

	// Copy to sender outbox/sent
	outboxDir := fsq.AgentOutboxSent(root, me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	session := sessionName(root)
	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"id":           id,
			"thread":       msg.Header.Thread,
			"to":           []string{recipient},
			"subject":      subject,
			"session":      session,
			"root":         root,
			"in_reply_to":  originalMsg.Header.ID,
			"original_box": filepath.Base(filepath.Dir(originalPath)),
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		})
	}

	if outboxErr != nil {
		_ = writeStderr("warning: outbox write failed: %v\n", outboxErr)
	}
	return writeStdout("Replied %s to %s (session: %s, root: %s)\n", id, recipient, session, root)
}

// runReplyFederated handles reply to a message that has Origin.ReplyTo set,
// routing the reply through the resolver to deliver cross-session/cross-project.
func runReplyFederated(common *commonFlags, root, me string,
	originalMsg format.Message, originalPath string,
	subject, body string, ackRequired bool,
	priority, kind string, labels []string, context map[string]any) error {

	replyTo := originalMsg.Header.Origin.ReplyTo

	ep, err := resolve.ParseAddress(replyTo)
	if err != nil {
		return fmt.Errorf("invalid reply_to address %q in original message: %w", replyTo, err)
	}

	// Build resolver.
	baseRoot := resolveBaseRootForFederation(root)
	projectDir := resolveProjectDir()
	resolver := resolve.NewResolver(root, baseRoot, projectDir)

	targets, err := resolver.Resolve(ep)
	if err != nil {
		return fmt.Errorf("resolve reply_to %q: %w", replyTo, err)
	}

	if len(targets) == 0 {
		return fmt.Errorf("reply_to %q resolved to zero targets", replyTo)
	}

	// Use the first target (reply_to should resolve to exactly one agent).
	target := targets[0]

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}

	// Build origin for the reply (reuses projectDir from resolver setup above).
	origin := buildOrigin(me, root, projectDir, ackRequired)

	// Determine scope.
	scope := "local"
	if target.Project != "" {
		scope = "cross-project"
	} else if target.SessionRoot != root {
		scope = "cross-session"
	}

	recipient := target.Agent
	resolvedAddr := formatResolvedTarget(target)

	msg := format.Message{
		Header: format.Header{
			Schema:      format.CurrentSchema,
			ID:          id,
			From:        me,
			To:          []string{recipient},
			Thread:      originalMsg.Header.Thread,
			Subject:     subject,
			Created:     now.UTC().Format(time.RFC3339Nano),
			AckRequired: ackRequired,
			Refs:        append(originalMsg.Header.Refs, originalMsg.Header.ID),
			Priority:    priority,
			Kind:        kind,
			Labels:      labels,
			Context:     context,
			Origin:      origin,
			Delivery: &format.Delivery{
				RequestedTo: []string{replyTo},
				ResolvedTo:  []string{resolvedAddr},
				Scope:       scope,
			},
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"

	// Deliver to the resolved target.
	if target.SessionRoot == root {
		_, err = fsq.DeliverToInbox(root, target.Agent, filename, data)
	} else {
		_, err = fsq.DeliverToExistingInbox(target.SessionRoot, target.Agent, filename, data)
	}
	if err != nil {
		return fmt.Errorf("deliver reply to %s: %w", resolvedAddr, err)
	}

	// Copy to sender outbox/sent.
	outboxDir := fsq.AgentOutboxSent(root, me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	session := sessionName(root)
	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"id":           id,
			"thread":       msg.Header.Thread,
			"to":           []string{recipient},
			"resolved_to":  resolvedAddr,
			"subject":      subject,
			"session":      session,
			"root":         root,
			"scope":        scope,
			"federated":    true,
			"in_reply_to":  originalMsg.Header.ID,
			"original_box": filepath.Base(filepath.Dir(originalPath)),
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		})
	}

	if outboxErr != nil {
		_ = writeStderr("warning: outbox write failed: %v\n", outboxErr)
	}
	return writeStdout("Replied %s to %s (session: %s, scope: %s)\n", id, resolvedAddr, session, scope)
}

// findMessage searches for a message by ID in the agent's inbox (new and cur).
func findMessage(root, me, msgID string) (format.Message, string, error) {
	// Normalize the message ID
	filename, err := ensureFilename(msgID)
	if err != nil {
		return format.Message{}, "", err
	}

	// Try inbox/new first
	newPath := filepath.Join(fsq.AgentInboxNew(root, me), filename)
	if msg, err := format.ReadMessageFile(newPath); err == nil {
		return msg, newPath, nil
	}

	// Try inbox/cur
	curPath := filepath.Join(fsq.AgentInboxCur(root, me), filename)
	if msg, err := format.ReadMessageFile(curPath); err == nil {
		return msg, curPath, nil
	}

	// Try outbox/sent (for replying to our own messages)
	sentPath := filepath.Join(fsq.AgentOutboxSent(root, me), filename)
	if msg, err := format.ReadMessageFile(sentPath); err == nil {
		return msg, sentPath, nil
	}

	return format.Message{}, "", fmt.Errorf("message not found: %s", msgID)
}
