package cli

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runReply(args []string) error {
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Message ID to reply to (a bridged message may be addressed by its header ID even though it is stored under a transfer filename)")
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
		"",
		"--id also resolves bridged messages by their header ID (stored under a transfer filename). If the ID",
		"matches more than one stored message, the command fails with a general error (exit 1) naming both",
		"paths; a missing message exits 3.",
		"Use --wait-for drained to block until the recipient ingests the reply,",
		"mirroring amq send --wait-for.")
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
	common.warnRootOverride()
	root := resolveRoot(common.Root)
	if err := validatePinOverride(common, *ignoreSessionPinFlag, false); err != nil {
		return err
	}
	if err := guardPinnedSourceContext("reply", root, false, *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
		return err
	}

	if *idFlag == "" {
		return UsageError("--id is required")
	}

	// Validate co-op fields
	priority := strings.TrimSpace(*priorityFlag)
	kind := strings.TrimSpace(*kindFlag)
	if err := validatePriorityKind(priority, kind); err != nil {
		return err
	}

	waitFor := strings.TrimSpace(*waitForFlag)
	if waitFor != "" {
		if err := validateStage(waitFor); err != nil {
			return UsageError("--wait-for: %v", err)
		}
	}

	labels := splitList(*labelsFlag)

	context, err := parseContext(*contextFlag)
	if err != nil {
		return err
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
		if errors.Is(err, errMessageNotFound) {
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
	var sourceProject string

	if originalMsg.Header.ReplyProject != "" {
		// Cross-project reply: route via peer lookup.
		targetProject = strings.TrimSpace(originalMsg.Header.ReplyProject)
		if targetProject == "" {
			return fmt.Errorf("invalid empty reply_project in original message")
		}
		recipientNorm, sessionNorm, err := parseReplyToRoute(originalMsg.Header.ReplyTo, true)
		if err != nil {
			return fmt.Errorf("malformed cross-project reply metadata for project %q: %w", targetProject, err)
		}
		recipient = recipientNorm
		targetSession = sessionNorm

	} else if originalMsg.Header.ReplyTo != "" {
		// A same-project routing hint must include a session. Bare handles are
		// meaningful only alongside reply_project, where they identify a peer's
		// base root.
		recipientNorm, sessionNorm, err := parseReplyToRoute(originalMsg.Header.ReplyTo, false)
		if err != nil {
			return err
		}
		recipient = recipientNorm
		targetSession = sessionNorm
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

	}

	routePlan, err := planDeliveryRoute(root, targetProject, targetSession, deliveryRouteOptions{})
	if err != nil {
		return err
	}
	deliveryRoot = routePlan.DeliveryRoot
	targetSession = routePlan.TargetSession
	sourceProject = routePlan.SourceProject

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

	var localMailboxAuthorization *fsq.MailboxConfigAuthorization
	var peerConfigFS *fsq.DeliveryRoot
	var peerConfigPresent bool
	var sourceConfigFS *fsq.DeliveryRoot
	var sourceConfigPresent bool
	if targetProject != "" {
		sourceConfigSelection, err := openSourceConfigSelection(sourceFS, root, pin, *ignoreSessionPinFlag)
		if err != nil {
			return err
		}
		defer sourceConfigSelection.Close()
		sourceConfigFS = sourceConfigSelection.ConfigFS
		senderQueue := endpointQueueFor(sourceFS, sourceConfigFS)
		if err := checkEndpointRegistration(senderQueue, endpointSender, common.Strict, true, me); err != nil {
			return err
		}
		sourceConfigPresent = senderQueue.decls[0].Present

		peerConfigSelection, err := openMailboxConfigSelection(
			deliveryFS,
			deliveryRoot,
			routePlan.PeerBaseRoot,
			"",
		)
		if err != nil {
			return err
		}
		defer peerConfigSelection.Close()
		if !deliveryAgentExists(deliveryFS, recipient) {
			if targetSession != "" {
				return fmt.Errorf("agent %q not found in peer %q session %q", recipient, targetProject, targetSession)
			}
			return fmt.Errorf("agent %q not found in peer %q", recipient, targetProject)
		}
		if layoutErr := fsq.ValidateExistingMailboxLayout(deliveryFS, recipient); layoutErr != nil {
			return layoutErr
		}
		peerConfigFS = peerConfigSelection.ConfigFS
		peerQueue := endpointQueueFor(deliveryFS, peerConfigFS)
		if err := checkEndpointRegistration(peerQueue, endpointRecipient, common.Strict, true, recipient); err != nil {
			return err
		}
		peerConfigPresent = peerQueue.decls[0].Present
	} else {
		layoutErr := fsq.ValidateExistingMailboxLayout(deliveryFS, recipient)
		authorization, cleanup, err := openLocalMailboxAuthorization(
			deliveryFS,
			deliveryRoot,
			pin,
			*ignoreSessionPinFlag,
			true,
		)
		if err != nil {
			return err
		}
		defer cleanup()
		if authorization == nil {
			if layoutErr != nil {
				return layoutErr
			}
		} else {
			if err := validateAuthorizedLocalMailbox(deliveryFS, authorization, recipient); err != nil {
				return err
			}
			localMailboxAuthorization = authorization
		}
		senderQueue, closeSenderQueue, err := openLocalEndpointQueue(sourceFS, root, pin, *ignoreSessionPinFlag)
		if err != nil {
			return err
		}
		defer closeSenderQueue()
		if err := checkEndpointRegistration(senderQueue, endpointSender, common.Strict, true, me); err != nil {
			return err
		}
		recipientQueue, closeRecipientQueue, err := openLocalEndpointQueue(deliveryFS, deliveryRoot, pin, *ignoreSessionPinFlag)
		if err != nil {
			return err
		}
		defer closeRecipientQueue()
		if err := checkEndpointRegistration(recipientQueue, endpointRecipient, common.Strict, true, recipient); err != nil {
			return err
		}
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

	sourceSession := ""
	if targetProject != "" || targetSession != "" {
		sourceSession = resolveSessionName(root)
	}
	session := sessionName(root)
	targetDisplay := session
	if targetSession != "" {
		targetDisplay = targetSession
	}
	return deliverOutgoing(outgoingMessage{
		command:              "reply",
		verb:                 "Replied",
		me:                   me,
		recipients:           []string{recipient},
		thread:               originalMsg.Header.Thread,
		subject:              subject,
		refs:                 append(originalMsg.Header.Refs, originalMsg.Header.ID),
		priority:             priority,
		kind:                 kind,
		labels:               labels,
		context:              context,
		body:                 body,
		sourceSession:        sourceSession,
		sourceProject:        sourceProject,
		targetProject:        targetProject,
		targetSession:        targetSession,
		sourceFS:             sourceFS,
		deliveryFS:           deliveryFS,
		deliveryRoot:         deliveryRoot,
		strict:               common.Strict,
		localAuthorization:   localMailboxAuthorization,
		sourceConfigFS:       sourceConfigFS,
		sourceConfigPresent:  sourceConfigPresent,
		peerConfigFS:         peerConfigFS,
		peerConfigPresent:    peerConfigPresent,
		waitFor:              waitFor,
		waitTimeout:          *waitTimeoutFlag,
		json:                 common.JSON,
		displaySession:       targetDisplay,
		sourceSessionDisplay: session,
		jsonExtra: map[string]any{
			"in_reply_to":  originalMsg.Header.ID,
			"original_box": originalBox,
		},
	})
}

// parseReplyToRoute accepts exactly one reply routing shape: handle@session,
// or a bare handle when a cross-project reply may target the peer's base root.
func parseReplyToRoute(raw string, allowBase bool) (handle, session string, err error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", "", fmt.Errorf("malformed reply_to %q: value is empty", raw)
	}
	if value != raw || strings.Count(value, "@") > 1 {
		return "", "", fmt.Errorf("malformed reply_to %q: expected handle or handle@session", raw)
	}

	parts := strings.Split(value, "@")
	if len(parts) == 1 {
		if !allowBase {
			return "", "", fmt.Errorf("malformed reply_to %q: expected handle@session", raw)
		}
	} else if parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed reply_to %q: expected non-empty handle@session", raw)
	}

	handle, err = normalizeHandle(parts[0])
	if err != nil {
		return "", "", fmt.Errorf("invalid handle in reply_to %q: %w", raw, err)
	}
	if len(parts) == 1 {
		return handle, "", nil
	}
	session, err = normalizeHandle(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("invalid session in reply_to %q: %w", raw, err)
	}
	return handle, session, nil
}
