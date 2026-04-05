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

func runSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	common := addCommonFlags(fs)
	toFlag := fs.String("to", "", "Receiver handle (comma-separated)")
	subjectFlag := fs.String("subject", "", "Message subject")
	threadFlag := fs.String("thread", "", "Thread id (required for cross-session sends; default p2p/<a>__<b> for local)")
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

	// Cross-project flag
	projectFlag := fs.String("project", "", "Target peer project name (delivers to a peer project's inbox)")

	usage := usageWithFlags(fs, "amq send --me <agent> --to <recipients> [--project <name>] [--session <name>] [options]",
		"",
		"Cross-session example:",
		"  amq send --to codex --session auth --thread xsession/auth-review --body \"...\"",
		"",
		"Cross-project examples:",
		"  amq send --to codex --project infra-lib --body \"hello from here\"",
		"  amq send --to codex@infra-lib:collab --body \"inline syntax\"",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := rejectPositionalArgs(fs, "send"); err != nil {
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

	// Parse inline agent@project:session syntax from --to BEFORE handle validation,
	// since normalizeHandle rejects '@' and ':'.
	targetProject := strings.TrimSpace(*projectFlag)
	inlineSession := ""
	rawTo := strings.TrimSpace(*toFlag)
	if targetProject == "" && rawTo != "" && strings.Contains(rawTo, "@") {
		if handle, proj, sess, ok := parseInlineRecipient(rawTo); ok {
			rawTo = handle
			targetProject = proj
			inlineSession = sess
		}
	}

	recipients, err := splitRecipients(rawTo)
	if err != nil {
		if _, ok := err.(*ExitCodeError); ok {
			return err
		}
		return UsageError("--to: %v", err)
	}
	recipients = dedupeStrings(recipients)

	// Determine delivery root: local, cross-session, or cross-project.
	deliveryRoot := root
	targetSession := strings.TrimSpace(*sessionFlag)
	// Inline session from @project:session takes effect only when not overridden.
	if targetSession == "" && inlineSession != "" {
		targetSession = inlineSession
	}
	var replyProject string

	if targetProject != "" {
		// Cross-project delivery.
		peerBaseRoot, err := resolvePeer(root, targetProject)
		if err != nil {
			return err
		}
		if !dirExists(peerBaseRoot) {
			return fmt.Errorf("peer root for %q does not exist: %s", targetProject, peerBaseRoot)
		}

		if targetSession != "" {
			// Cross-project + explicit session.
			normalized, err := normalizeHandle(targetSession)
			if err != nil {
				return UsageError("--session: %v", err)
			}
			targetSession = normalized
			deliveryRoot = filepath.Join(peerBaseRoot, targetSession)
		} else {
			// Cross-project, no explicit session. Mirror the sender's session when
			// the source root is itself a session root.
			if classifyRoot(root) != "" {
				// Inside a session — use same session name in peer.
				targetSession = sessionName(root)
				deliveryRoot = filepath.Join(peerBaseRoot, targetSession)
			} else {
				// At base root — deliver to peer's base root directly.
				deliveryRoot = peerBaseRoot
			}
		}

		if !dirExists(deliveryRoot) {
			if targetSession != "" {
				return fmt.Errorf("session %q not found in peer %q at %s", targetSession, targetProject, deliveryRoot)
			}
			return fmt.Errorf("peer %q root does not exist at %s", targetProject, deliveryRoot)
		}
		for _, r := range recipients {
			inbox := filepath.Join(deliveryRoot, "agents", r, "inbox")
			if !dirExists(inbox) {
				if targetSession != "" {
					return fmt.Errorf("agent %q not found in peer %q session %q", r, targetProject, targetSession)
				}
				return fmt.Errorf("agent %q not found in peer %q", r, targetProject)
			}
		}
		replyProject = resolveProject(root)
	} else if targetSession != "" {
		normalized, err := normalizeHandle(targetSession)
		if err != nil {
			return UsageError("--session: %v", err)
		}
		targetSession = normalized

		// Cross-session requires AM_BASE_ROOT to be set (by coop exec) or
		// the root must be a session root (has a parent with sibling sessions).
		// This eliminates the base-root ambiguity: --session only works from
		// a session context, never from the base root directly.
		baseRoot := classifyRoot(root)
		if baseRoot == "" {
			return fmt.Errorf("--session requires a session context: run from inside 'amq coop exec --session <name>'")
		}

		deliveryRoot = filepath.Join(baseRoot, targetSession)

		// Verify the target session and agent inboxes exist.
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

	// Validate sender in source root, recipients in target root. Always.
	if targetProject != "" || targetSession != "" {
		if err := validateKnownHandles(root, common.Strict, me); err != nil {
			return err
		}
		if err := validateKnownHandles(deliveryRoot, common.Strict, recipients...); err != nil {
			return err
		}
	} else {
		allHandles := append([]string{me}, recipients...)
		if err := validateKnownHandles(root, common.Strict, allHandles...); err != nil {
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

	// Detect whether sender is inside a session (needed for reply_to and thread IDs).
	senderInSession := classifyRoot(root) != ""

	// Thread ID: auto-generated for P2P, qualified for cross-session/cross-project.
	threadID := strings.TrimSpace(*threadFlag)
	if threadID == "" {
		if len(recipients) == 1 {
			if targetProject != "" {
				// Cross-project: include project names (and session names when applicable).
				srcProject := resolveProject(root)
				if targetSession != "" && senderInSession {
					srcSession := sessionName(root)
					threadID = "p2p/" + srcProject + ":" + srcSession + ":" + common.Me + "__" + targetProject + ":" + targetSession + ":" + recipients[0]
				} else if targetSession != "" {
					// Sender at base root targeting a session.
					threadID = "p2p/" + srcProject + ":" + common.Me + "__" + targetProject + ":" + targetSession + ":" + recipients[0]
				} else {
					threadID = "p2p/" + srcProject + ":" + common.Me + "__" + targetProject + ":" + recipients[0]
				}
			} else if targetSession != "" {
				// Cross-session: include session names to avoid collisions.
				senderSession := sessionName(root)
				threadID = "p2p/" + senderSession + ":" + common.Me + "__" + targetSession + ":" + recipients[0]
			} else {
				threadID = canonicalP2P(common.Me, recipients[0])
			}
		} else {
			return UsageError("--thread is required when sending to multiple recipients")
		}
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}

	// Build reply_to for cross-session/cross-project sends.
	replyTo := ""
	if senderInSession {
		// Sender is in a session — stamp handle@session for reply routing.
		replyTo = common.Me + "@" + sessionName(root)
	} else if targetProject != "" {
		// Sender at base root, cross-project — stamp just handle.
		replyTo = common.Me
	}

	// Set from_project on cross-project sends so receivers can distinguish
	// same-handle senders from different projects.
	fromProject := ""
	if targetProject != "" {
		fromProject = replyProject
	}

	msg := format.Message{
		Header: format.Header{
			Schema:       format.CurrentSchema,
			ID:           id,
			From:         common.Me,
			To:           recipients,
			Thread:       threadID,
			Subject:      strings.TrimSpace(*subjectFlag),
			Created:      now.UTC().Format(time.RFC3339Nano),
			AckRequired:  *ackFlag,
			Refs:         splitList(*refsFlag),
			Priority:     priority,
			Kind:         kind,
			Labels:       labels,
			Context:      context,
			ReplyTo:      replyTo,
			ReplyProject: replyProject,
			FromProject:  fromProject,
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
		for _, r := range recipients {
			if _, err := fsq.DeliverToExistingInbox(deliveryRoot, r, filename, data); err != nil {
				return err
			}
		}
	} else {
		if _, err := fsq.DeliverToInboxes(deliveryRoot, recipients, filename, data); err != nil {
			return err
		}
	}

	// Best-effort presence touch.
	_ = presence.Touch(root, common.Me)

	// Copy to sender outbox/sent for audit (always in sender's root).
	outboxDir := fsq.AgentOutboxSent(root, common.Me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	session := ""
	if senderInSession {
		session = sessionName(root)
	}
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
		if targetProject != "" {
			out["cross_project"] = true
			out["source_project"] = replyProject
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
		if err := writeStderr("warning: outbox write failed: %v\n", outboxErr); err != nil {
			return err
		}
	}
	if err := writeStdout("Sent %s to %s (session: %s, root: %s)\n", id, strings.Join(recipients, ","), targetDisplay, deliveryRoot); err != nil {
		return err
	}
	return nil
}

// parseInlineRecipient parses "agent@project:session" or "agent@project" syntax.
// Returns the parsed components and true if the inline syntax was detected.
// Returns the original string unchanged and false if no @ is present.
func parseInlineRecipient(raw string) (handle, project, session string, ok bool) {
	atIdx := strings.Index(raw, "@")
	if atIdx < 0 {
		return raw, "", "", false
	}
	handle = raw[:atIdx]
	qualifier := raw[atIdx+1:]
	if qualifier == "" {
		return raw, "", "", false
	}
	if colonIdx := strings.Index(qualifier, ":"); colonIdx >= 0 {
		project = qualifier[:colonIdx]
		session = qualifier[colonIdx+1:]
	} else {
		project = qualifier
	}
	return handle, project, session, true
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
