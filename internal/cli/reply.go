package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

func runReply(args []string) error {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message ID to reply to")
	bodyFlag := fs.String("body", "", "Body string, @file, or - / empty to read stdin")
	allowEmptyFlag := fs.Bool("allow-empty", false, "Allow sending a blank body (otherwise an empty body is rejected)")
	subjectFlag := fs.String("subject", "", "Override subject (default: Re: <original>)")

	// Co-op mode flags
	priorityFlag := fs.String("priority", "", "Message priority: urgent, normal, low")
	kindFlag := fs.String("kind", "", fmt.Sprintf("Message kind: %s (default: same as original, review_response for review_request, answer for question)", format.ValidKindsList()))
	labelsFlag := fs.String("labels", "", "Comma-separated labels/tags")
	contextFlag := fs.String("context", "", "JSON context object or @file.json")
	waitForFlag := fs.String("wait-for", "", "Wait for receipt stage after reply (e.g., drained)")
	waitTimeoutFlag := fs.Duration("wait-timeout", 120*time.Second, "Timeout for --wait-for")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, ignore a conflicting AM_SESSION source pin")

	usage := usageWithFlags(fs, "amq reply --me <agent> --id <msg_id> [options]",
		"Reply to a message with automatic thread/refs handling.",
		"Finds the original message, sets to/thread/refs automatically.",
		"Cross-session replies are routed via reply_to header.",
		"To follow up on a sent cross-session message, use amq send --session instead.",
		"Use --wait-for drained to block until the recipient ingests the reply,",
		"mirroring amq send --wait-for.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := rejectPositionalArgs(fs, "reply"); err != nil {
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
	common.warnRootOverride()
	root := resolveRoot(common.Root)
	if err := validatePinOverride(common, *ignoreSessionPinFlag, false); err != nil {
		return err
	}
	if err := guardPinnedSourceContext("reply", root, *ignoreSessionPinFlag); err != nil {
		return err
	}

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

	waitFor := strings.TrimSpace(*waitForFlag)
	if waitFor != "" {
		if err := validateStage(waitFor); err != nil {
			return UsageError("--wait-for: %v", err)
		}
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

	// Pin the source tree before reading the original. Its contents determine
	// the reply recipient and routing destination.
	sourceIdentity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return err
	}
	pin, err := loadSessionPin()
	if err != nil {
		return err
	}
	if pin.IdentityPin && !*ignoreSessionPinFlag &&
		verifyTreeIdentityInfo(sourceIdentity.FileInfo(), pin.RootID) != TreeRelationSame {
		return ContextMismatchError("authorized source root identity changed before capability open")
	}
	sourceFS, err := fsq.OpenDeliveryRoot(root, sourceIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = sourceFS.Close() }()
	originalFilename, err := ensureFilename(*idFlag)
	if err != nil {
		return UsageError("--id: %v", err)
	}
	originalPath, originalBox, err := findMessageDeliveryRoot(sourceFS, me, originalFilename, true)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NotFoundError("message not found: %s", *idFlag)
		}
		return err
	}
	originalMsg, err := readMessageDeliveryRoot(sourceFS, originalPath)
	if err != nil {
		return fmt.Errorf("read message %s: %w", *idFlag, err)
	}

	// Disallow reply on sent cross-session/cross-project messages.
	// If the message was found in outbox/sent and has a reply_to, the user
	// is trying to follow up on their own send. This is under-specified
	// (no target metadata in outbox copy).
	if originalBox == "sent" && originalMsg.Header.ReplyTo != "" {
		return fmt.Errorf("cannot reply to a sent cross-session/cross-project message (reply_to points back to you); use amq send --session/--project for follow-ups")
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

	}

	deliveryFS := sourceFS
	if filepath.Clean(root) != filepath.Clean(deliveryRoot) {
		deliveryIdentity, err := fsq.SnapshotDeliveryRoot(deliveryRoot)
		if err != nil {
			return err
		}
		deliveryFS, err = fsq.OpenDeliveryRoot(deliveryRoot, deliveryIdentity)
		if err != nil {
			return err
		}
		defer func() { _ = deliveryFS.Close() }()
	}
	if !deliveryInboxExists(deliveryFS, recipient) {
		switch {
		case targetProject != "" && targetSession != "":
			return fmt.Errorf("agent %q not found in peer %q session %q", recipient, targetProject, targetSession)
		case targetProject != "":
			return fmt.Errorf("agent %q not found in peer %q", recipient, targetProject)
		case targetSession != "":
			return fmt.Errorf("agent %q not found in session %q", recipient, targetSession)
		default:
			return fmt.Errorf("agent %q not found in current session (no reply_to in original message — sender may not be in a session)", recipient)
		}
	}

	// Validate recipient through the same capability used for delivery.
	if err := validateKnownHandlesDeliveryRoot(deliveryFS, common.Strict, recipient); err != nil {
		return err
	}

	body, err := readBody(*bodyFlag, *allowEmptyFlag)
	if err != nil {
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
			Schema:   format.CurrentSchema,
			ID:       id,
			From:     me,
			To:       []string{recipient},
			Thread:   originalMsg.Header.Thread,
			Subject:  subject,
			Created:  now.UTC().Format(time.RFC3339Nano),
			Refs:     append(originalMsg.Header.Refs, originalMsg.Header.ID),
			Priority: priority,
			Kind:     kind,
			Labels:   labels,
			Context:  context,
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
		if _, err := fsq.DeliverToExistingInbox(deliveryFS, recipient, filename, data); err != nil {
			return reportDeliveryError(id, err)
		}
	} else {
		if _, err := fsq.DeliverToInboxes(deliveryFS, []string{recipient}, filename, data); err != nil {
			return reportDeliveryError(id, err)
		}
	}

	// Best-effort presence touch.
	_ = presence.TouchDeliveryRoot(sourceFS, me)

	outboxDir := filepath.Join("agents", me, "outbox", "sent")
	outboxErr := error(nil)
	if _, err := sourceFS.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	// Wait for receipt if requested (mirrors amq send --wait-for).
	var waitResult *waitForResult
	var waitErr error
	if waitFor != "" {
		r, err := receipt.WaitForDeliveryRoot(deliveryFS, id, recipient, waitFor, *waitTimeoutFlag, 1*time.Second)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			waitResult = &waitForResult{Event: "timeout", Stage: waitFor, Timeout: waitTimeoutFlag.String()}
			waitErr = TimeoutError("reply --wait-for %s timed out after %s", waitFor, *waitTimeoutFlag)
		} else if err != nil {
			waitResult = &waitForResult{Event: "error", Stage: waitFor, Detail: err.Error()}
			waitErr = fmt.Errorf("reply --wait-for: %w", err)
		} else {
			waitResult = &waitForResult{Event: "matched", Stage: waitFor, Receipt: &r}
		}
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
			"outbox":       outboxResult(outboxErr),
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
		if waitResult != nil {
			out["wait"] = waitResult
		}
		if err := writeJSON(os.Stdout, out); err != nil {
			return err
		}
		return waitErr
	}

	if outboxErr != nil {
		_ = reportOutboxError(outboxErr)
	}
	if waitResult != nil {
		switch waitResult.Event {
		case "matched":
			if err := writeStdout("Replied %s to %s; %s by %s at %s\n", id, recipient, waitFor, recipient, waitResult.Receipt.EmittedAt); err != nil {
				return err
			}
		case "timeout":
			if err := writeStdout("Replied %s to %s; timed out waiting %s for %s receipt\n", id, recipient, *waitTimeoutFlag, waitFor); err != nil {
				return err
			}
		default:
			if err := writeStdout("Replied %s to %s; wait error: %s\n", id, recipient, waitResult.Detail); err != nil {
				return err
			}
		}
		return waitErr
	}
	return writeStdout("Replied %s to %s (session: %s, root: %s)\n", id, recipient, targetDisplay, deliveryRoot)
}
