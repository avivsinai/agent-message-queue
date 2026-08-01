package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

var deliverToExistingInbox = fsq.DeliverToExistingInbox

func runSend(args []string) error {
	return runSendWithAfterBodyRead(args, nil)
}

func runSendWithAfterBodyRead(args []string, afterBodyRead func()) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	common := addCommonFlags(fs)
	toFlag := fs.String("to", "", "Receiver handle (comma-separated)")
	subjectFlag := fs.String("subject", "", "Message subject")
	threadFlag := fs.String("thread", "", "Thread id (required for multiple recipients; default p2p/<a>__<b> for single-recipient sends)")
	bodyFlag := fs.String("body", "", "Body string, @file, or - / empty to read stdin")
	allowEmptyFlag := fs.Bool("allow-empty", false, "Allow sending a blank body (otherwise an empty body is rejected)")
	allowSelfFlag := fs.Bool("allow-self", false, "Allow an intentional same-root send to the sender's own handle")
	refsFlag := fs.String("refs", "", "Comma-separated related message ids")
	waitForFlag := fs.String("wait-for", "", "Wait for receipt stage after send (drained, dlq)")
	waitTimeoutFlag := fs.Duration("wait-timeout", 120*time.Second, "Timeout for --wait-for (0 = wait forever)")

	// Co-op mode flags
	priorityFlag := fs.String("priority", "", "Message priority: urgent, normal, low (default: normal if kind set)")
	kindFlag := fs.String("kind", "", "Message kind: "+format.ValidKindsList())
	labelsFlag := fs.String("labels", "", "Comma-separated labels/tags")
	contextFlag := fs.String("context", "", "JSON context object or @file.json")

	// Cross-session flag
	sessionFlag := fs.String("session", "", "Target session (delivers to a different session's inbox)")
	fromSessionFlag := fs.String("from-session", "", "Source session for setup-terminal cross-session sends")
	ignoreSessionPinFlag := fs.Bool("ignore-session-pin", false, "With explicit --root, ignore a conflicting AM_SESSION source pin")

	// Cross-project flag
	projectFlag := fs.String("project", "", "Target peer project name (delivers to a peer project's inbox)")

	usage := usageWithFlags(fs, "amq send --me <agent> --to <recipients> [--project <name>] [--session <name>] [--from-session <name>] [options]",
		"",
		"Cross-session example:",
		"  amq send --to codex --session auth --thread xsession/auth-review --body \"...\"",
		"  amq send --root .agent-mail --from-session cto --me alice --to bob --session qa --body \"...\"",
		"",
		"Receipt example:",
		"  amq send --to codex --body \"please review\" --wait-for drained --wait-timeout 60s",
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
	if targetProject != "" && len(recipients) > 1 {
		return UsageError("--project supports exactly one recipient; got %d. Send one message per recipient.", len(recipients))
	}

	// Validate --wait-for (basic checks; cross-root check deferred until routing is resolved)
	waitFor := strings.TrimSpace(*waitForFlag)
	if waitFor != "" {
		if err := validateStage(waitFor); err != nil {
			return UsageError("--wait-for: %v", err)
		}
		if len(recipients) != 1 {
			return UsageError("--wait-for requires exactly one recipient (got %d)", len(recipients))
		}
		if *waitTimeoutFlag < 0 {
			return UsageError("--wait-timeout must be >= 0")
		}
	}

	// Determine delivery root: local, cross-session, or cross-project.
	deliveryRoot := root
	peerBaseRoot := ""
	targetSession := strings.TrimSpace(*sessionFlag)
	// Inline session from @project:session takes effect only when not overridden.
	if targetSession == "" && inlineSession != "" {
		targetSession = inlineSession
	}
	sourceRoot := root
	sourceSession := ""
	fromSession := strings.TrimSpace(*fromSessionFlag)
	if *ignoreSessionPinFlag && !common.rootExplicit() {
		return UsageError("--ignore-session-pin requires an explicit --root")
	}
	if *ignoreSessionPinFlag && fromSession != "" {
		return UsageError("--ignore-session-pin cannot be used with --from-session")
	}

	// Cross-tree guard (issue #144): a direct, unqualified --root that crosses
	// into a different AMQ tree carries no sender-origin metadata, so the
	// recipient cannot reply — and a naive reply would loop back into the
	// replier's own tree. Refuse with guidance instead of minting an
	// unreplyable message. Replyable cross-tree messaging must declare a routing
	// dimension (--project / --session), which stamps the routing headers.
	// Fires only on positive evidence of a different home root (AM_ROOT /
	// AM_BASE_ROOT); bare-root sends with no session env set are unaffected.
	routed := targetProject != "" || targetSession != "" || fromSession != ""
	// Validate explicit session evidence before advisory cross-tree classification;
	// malformed pins must fail closed with the context-mismatch exit status.
	var pin sessionPin
	var pinErr error
	pin, pinErr = loadSessionPin()
	if pinErr != nil {
		return pinErr
	}
	if !*ignoreSessionPinFlag {
		if err := validateLegacySessionPinRoot(pin); err != nil {
			return ContextMismatchError(
				"refusing send: %s. Target routing does not authorize a mismatched source; use an explicit source route, or explicit --root with --ignore-session-pin",
				err,
			)
		}
	}
	if pin.Present && pin.IdentityPin {
		if verifyTreeIdentityToken(pin.BaseRoot, pin.BaseRootID) != TreeRelationSame {
			return ContextMismatchError("pinned base root identity is not current")
		}
		if fromSession != "" && verifyTreeIdentityToken(root, pin.RootID) != TreeRelationSame {
			return ContextMismatchError("pinned root identity is not current")
		}
		if fromSession == "" {
			if err := guardPinnedSourceContext("send", sourceRoot, targetProject != "", *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
				return err
			}
		}
	}
	if fromSession != "" {
		decision := decideSessionGuard(sessionGuardInput{
			Kind: sessionGuardSource,
			Pin:  sessionGuardPinStateFor(pin), Relation: sessionGuardTargetMismatch,
			Flags: sessionGuardFlags{FromSession: true},
		})
		if decision.Verdict != sessionGuardAllow {
			return ContextMismatchError("refusing send: source session routing was not authorized")
		}
	}
	if common.rootExplicit() && !routed {
		if src, ok := conflictingSourceRoot(root); ok {
			return UsageError("refusing send: --root %s targets a different AMQ tree than your own (%s), "+
				"but no routing dimension was given, so the recipient could not reply.\n"+
				"Use --project <peer> or --session <name> for replyable cross-tree routing, "+
				"or set the target as AM_ROOT if this send is genuinely local.", root, src)
		}
	}
	// Preserve the original lexical source guard after the advisory check. An
	// identity pin was already validated above; lexical pins still need refusal.
	if fromSession == "" && (!pin.Present || !pin.IdentityPin) {
		if err := guardPinnedSourceContext("send", sourceRoot, targetProject != "", *ignoreSessionPinFlag, common.rootExplicit()); err != nil {
			return err
		}
	}
	if !routed && !*allowSelfFlag {
		for _, recipient := range recipients {
			if recipient == me {
				return UsageError(
					"refusing send: recipient %q matches the sender and no routing dimension was given. "+
						"Use --project <peer> or --session <name> to reach another instance, "+
						"or pass --allow-self to confirm an intentional same-root self-send.",
					me,
				)
			}
		}
	}
	sourceProject := ""
	if fromSession != "" {
		if targetProject != "" {
			return UsageError("--from-session is not supported with --project")
		}
		if targetSession == "" {
			return UsageError("--from-session requires --session")
		}
		if err := validateSessionName(fromSession); err != nil {
			return UsageError("--from-session: %v", err)
		}
		if classifyRoot(root) != "" {
			return UsageError("--from-session requires --root to be the base root, not a session root")
		}
		sourceRoot, err = resolveSessionRoot(root, fromSession)
		if err != nil {
			return fmt.Errorf("source session %q not found: %w", fromSession, err)
		}
		sourceSession = fromSession
	}
	if targetProject != "" || targetSession != "" {
		routePlan, err := planDeliveryRoute(sourceRoot, targetProject, targetSession, deliveryRouteOptions{
			MirrorPeerSession: true,
		})
		if err != nil {
			return err
		}
		deliveryRoot = routePlan.DeliveryRoot
		peerBaseRoot = routePlan.PeerBaseRoot
		targetSession = routePlan.TargetSession
		sourceProject = routePlan.SourceProject
	}

	// Snapshot the physical roots at the authorization boundary. Opening the
	// capabilities below must prove it got these exact directories.
	deliveryIdentity, err := fsq.SnapshotDeliveryRoot(deliveryRoot)
	if err != nil {
		return err
	}
	sourceIdentity := deliveryIdentity
	if filepath.Clean(sourceRoot) != filepath.Clean(deliveryRoot) {
		sourceIdentity, err = fsq.SnapshotDeliveryRoot(sourceRoot)
		if err != nil {
			return err
		}
	}
	if !*allowSelfFlag && os.SameFile(sourceIdentity.FileInfo(), deliveryIdentity.FileInfo()) {
		for _, recipient := range recipients {
			if recipient == me {
				return UsageError(
					"refusing send: recipient %q resolves to the sender's physical AMQ tree. "+
						"Use --project <peer> or --session <name> to reach another instance, "+
						"or pass --allow-self to confirm an intentional same-root self-send.",
					me,
				)
			}
		}
	}
	if pin.IdentityPin && !*ignoreSessionPinFlag && fromSession == "" &&
		verifyTreeIdentityInfo(sourceIdentity.FileInfo(), pin.RootID) != TreeRelationSame {
		return ContextMismatchError("authorized source root identity changed before capability open")
	}

	deliveryFS, err := fsq.OpenDeliveryRoot(deliveryRoot, deliveryIdentity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryFS.Close() }()
	sourceFS := deliveryFS
	if filepath.Clean(sourceRoot) != filepath.Clean(deliveryRoot) {
		sourceFS, err = fsq.OpenDeliveryRoot(sourceRoot, sourceIdentity)
		if err != nil {
			return err
		}
		defer func() { _ = sourceFS.Close() }()
	}
	peerConfigBaseRoot := ""
	configBase := ""
	expectedBaseRootID := ""
	switch {
	case targetProject != "":
		configBase = peerBaseRoot
	case fromSession != "":
		configBase = root
		if pin.IdentityPin && !*ignoreSessionPinFlag {
			expectedBaseRootID = pin.BaseRootID
		}
	default:
		configBase, expectedBaseRootID = localMailboxConfigAuthority(
			deliveryRoot,
			pin,
			*ignoreSessionPinFlag,
		)
	}
	configSelection, err := openMailboxConfigSelection(
		deliveryFS,
		deliveryRoot,
		configBase,
		expectedBaseRootID,
	)
	if err != nil {
		return err
	}
	defer configSelection.Close()
	configFS := configSelection.ConfigFS
	sharedConfig := configSelection.Shared
	configAuthorityBaseRoot := configSelection.AuthorityRoot
	if targetProject != "" && sharedConfig {
		peerConfigBaseRoot = peerBaseRoot
	}
	var mailboxAuthorization *fsq.MailboxConfigAuthorization
	if targetProject == "" {
		var inventory fsq.MailboxInventory
		mailboxAuthorization, inventory, err = fsq.OpenMailboxConfigAuthorization(configFS)
		if err != nil {
			return sendMailboxAuthorizationError(deliveryRoot, inventory)
		}
		defer func() { _ = mailboxAuthorization.Close() }()
	}

	if fromSession != "" && !deliveryAgentExists(sourceFS, me) {
		return fmt.Errorf("agent %q not found in source session %q", me, fromSession)
	}
	peerRecipientConfigured := false
	peerConfigPresent := false
	if targetProject != "" {
		peerAgents, configPresent, peerAgentsErr := loadPeerAgentsForSend(configFS, common.Strict)
		if err := validateKnownHandlesFromAgents(peerAgents, peerAgentsErr, common.Strict, recipients...); err != nil {
			return err
		}
		peerConfigPresent = configPresent
		peerRecipientConfigured = handleConfigured(peerAgents, recipients[0])
		for _, recipient := range recipients {
			layoutErr := fsq.ValidateExistingMailboxLayout(deliveryFS, recipient)
			if layoutErr == nil {
				continue
			}
			return peerMailboxIncompleteError(
				targetProject,
				targetSession,
				recipient,
				layoutErr,
				deliveryRoot,
				peerConfigBaseRoot,
				peerRecipientConfigured,
			)
		}
	}

	// Validate sender in source root and recipients in target root through the
	// same capabilities that will perform delivery.
	var sourceConfigFS *fsq.DeliveryRoot
	var sourceConfigPresent bool
	if targetProject != "" || targetSession != "" {
		var sourceAgents []string
		var sourceAgentsErr error
		if targetProject == "" && sharedConfig {
			sourceAgents = withReservedHumanHandle(mailboxAuthorization.ConfiguredAgents())
		} else if targetProject == "" {
			sourceAgents, sourceAgentsErr = loadKnownAgentsDeliveryRoot(sourceFS, common.Strict)
		} else {
			sourceConfigBase, expectedSourceBaseRootID := localMailboxConfigAuthority(
				sourceRoot,
				pin,
				*ignoreSessionPinFlag,
			)
			sourceConfigSelection, sourceConfigErr := openMailboxConfigSelection(
				sourceFS,
				sourceRoot,
				sourceConfigBase,
				expectedSourceBaseRootID,
			)
			if sourceConfigErr != nil {
				return sourceConfigErr
			}
			defer sourceConfigSelection.Close()
			sourceAgents, sourceConfigPresent, sourceAgentsErr = loadPeerAgentsForSend(
				sourceConfigSelection.ConfigFS,
				common.Strict,
			)
			sourceConfigFS = sourceConfigSelection.ConfigFS
		}
		if err := validateKnownHandlesFromAgents(sourceAgents, sourceAgentsErr, common.Strict, me); err != nil {
			return err
		}
		if targetProject == "" {
			targetAgents := withReservedHumanHandle(mailboxAuthorization.ConfiguredAgents())
			if err := validateKnownHandlesFromAgents(targetAgents, nil, common.Strict, recipients...); err != nil {
				return err
			}
		}
	} else {
		allHandles := append([]string{me}, recipients...)
		agents := withReservedHumanHandle(mailboxAuthorization.ConfiguredAgents())
		if err := validateKnownHandlesFromAgents(agents, nil, common.Strict, allHandles...); err != nil {
			return err
		}
	}

	body, err := readBody(*bodyFlag, *allowEmptyFlag)
	if err != nil {
		return err
	}
	if afterBodyRead != nil {
		afterBodyRead()
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
	senderInSession := sourceSession != "" || classifyRoot(root) != ""

	// Thread ID: auto-generated for P2P, qualified for cross-session/cross-project.
	threadID := strings.TrimSpace(*threadFlag)
	if threadID == "" {
		if len(recipients) == 1 {
			if targetProject != "" {
				// Cross-project: include project names (and session names when applicable).
				if targetSession != "" && senderInSession {
					srcSession := sourceSessionName(root, sourceSession)
					threadID = "p2p/" + sourceProject + ":" + srcSession + ":" + common.Me + "__" + targetProject + ":" + targetSession + ":" + recipients[0]
				} else if targetSession != "" {
					// Sender at base root targeting a session.
					threadID = "p2p/" + sourceProject + ":" + common.Me + "__" + targetProject + ":" + targetSession + ":" + recipients[0]
				} else {
					threadID = "p2p/" + sourceProject + ":" + common.Me + "__" + targetProject + ":" + recipients[0]
				}
			} else if targetSession != "" {
				// Cross-session: include session names to avoid collisions.
				senderSession := sourceSessionName(root, sourceSession)
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

	// Build reply_to only for sends that actually cross a session or project
	// boundary. Ordinary same-session sends reply locally and need no hint —
	// stamping handle@session for them is what made a direct cross-root send
	// look replyable while looping into the replier's own tree (issue #144).
	replyTo := ""
	switch {
	case senderInSession && (targetProject != "" || targetSession != ""):
		// Cross-session or cross-project from a session — stamp handle@session.
		replyTo = common.Me + "@" + sourceSessionName(root, sourceSession)
	case targetProject != "":
		// Cross-project from a base root — stamp just the handle.
		replyTo = common.Me
	}

	// Set from_project on cross-project sends so receivers can distinguish
	// same-handle senders from different projects.
	fromProject := ""
	if targetProject != "" {
		fromProject = sourceProject
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
			Refs:         splitList(*refsFlag),
			Priority:     priority,
			Kind:         kind,
			Labels:       labels,
			Context:      context,
			ReplyTo:      replyTo,
			ReplyProject: sourceProject,
			FromProject:  fromProject,
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}
	if targetProject == "" {
		if err := prepareLocalSendMailboxes(deliveryFS, mailboxAuthorization, deliveryRoot, recipients); err != nil {
			return err
		}
		if err := mailboxAuthorization.Verify(); err != nil {
			return fmt.Errorf("destination mailbox authorization changed before delivery: %w", err)
		}
	}

	filename := id + ".md"
	if targetProject != "" {
		currentSourceAgents, currentSourceAgentsErr := revalidateSourceAgentsForSend(
			sourceConfigFS,
			sourceConfigPresent,
			common.Strict,
		)
		if err := validateKnownHandlesFromAgents(
			currentSourceAgents,
			currentSourceAgentsErr,
			common.Strict,
			me,
		); err != nil {
			return err
		}
		currentPeerAgents, currentPeerAgentsErr := revalidatePeerAgentsForSend(configFS, peerConfigPresent, common.Strict)
		if err := validateKnownHandlesFromAgents(currentPeerAgents, currentPeerAgentsErr, common.Strict, recipients...); err != nil {
			return err
		}
		if err := sourceFS.VerifyBase(); err != nil {
			return err
		}
		// Cross-project: use DeliverToExistingInbox (never creates dirs in peer).
		for _, r := range recipients {
			if _, err := deliverToExistingInbox(deliveryFS, r, filename, data); err != nil {
				var committed *fsq.CommittedDurabilityError
				if errors.As(err, &committed) {
					return reportDeliveryError(id, err)
				}
				if layoutErr := fsq.ValidateExistingMailboxLayout(deliveryFS, r); layoutErr != nil {
					currentPeerAgents, currentPeerAgentsErr := revalidatePeerAgentsForSend(configFS, peerConfigPresent, common.Strict)
					if rosterErr := validateKnownHandlesFromAgents(currentPeerAgents, currentPeerAgentsErr, common.Strict, r); rosterErr != nil {
						return rosterErr
					}
					return peerMailboxIncompleteError(
						targetProject,
						targetSession,
						r,
						layoutErr,
						deliveryRoot,
						peerConfigBaseRoot,
						handleConfigured(currentPeerAgents, r),
					)
				}
				return reportDeliveryError(id, err)
			}
		}
	} else {
		if targetSession != "" {
			if err := sourceFS.VerifyBase(); err != nil {
				return err
			}
		}
		if _, err := fsq.DeliverToInboxes(deliveryFS, recipients, filename, data); err != nil {
			return reportDeliveryError(id, err)
		}
	}

	// Best-effort presence touch.
	_ = presence.TouchDeliveryRoot(sourceFS, common.Me)

	// Copy to sender outbox/sent for audit (always in sender's root).
	outboxDir := filepath.Join("agents", common.Me, "outbox", "sent")
	outboxErr := error(nil)
	if _, err := sourceFS.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	session := ""
	if senderInSession {
		session = sourceSessionName(root, sourceSession)
	}
	targetDisplay := session
	if targetSession != "" {
		targetDisplay = targetSession
	}
	if targetDisplay == "" && common.rootExplicit() && targetProject == "" && targetSession == "" && fromSession == "" {
		targetDisplay = sessionName(deliveryRoot)
	}

	// Wait for receipt if requested (single-recipient only, validated above).
	var waitResult *waitForResult
	var waitErr error
	if waitFor != "" {
		consumer := recipients[0]
		r, err := receipt.WaitForDeliveryRoot(deliveryFS, id, consumer, waitFor, *waitTimeoutFlag, 1*time.Second)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			waitResult = &waitForResult{Event: "timeout", Stage: waitFor, Timeout: waitTimeoutFlag.String()}
			diagnosticCommand := doctorRootCommandForOS(deliveryRoot, configAuthorityBaseRoot, runtime.GOOS, "--ops")
			waitErr = TimeoutError("send --wait-for %s timed out after %s (delivery session %s, root %s); run %s to diagnose mailbox divergence", waitFor, *waitTimeoutFlag, targetDisplay, deliveryRoot, diagnosticCommand)
		} else if err != nil {
			waitResult = &waitForResult{Event: "error", Stage: waitFor, Detail: err.Error()}
			waitErr = fmt.Errorf("send --wait-for: %w", err)
		} else {
			waitResult = &waitForResult{Event: "matched", Stage: waitFor, Receipt: &r}
		}
	}

	if common.JSON {
		out := map[string]any{
			"id":          id,
			"thread":      threadID,
			"to":          recipients,
			"subject":     msg.Header.Subject,
			"session":     targetDisplay,
			"root":        deliveryRoot,
			"source_root": sourceRoot,
			"outbox":      outboxResult(outboxErr),
		}
		if targetProject != "" {
			out["cross_project"] = true
			out["source_project"] = sourceProject
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
		if err := reportOutboxError(outboxErr); err != nil {
			return err
		}
	}
	if waitResult != nil {
		switch waitResult.Event {
		case "matched":
			if err := writeStdout("Sent %s to %s; %s by %s at %s\n", id, recipients[0], waitFor, recipients[0], waitResult.Receipt.EmittedAt); err != nil {
				return err
			}
		case "timeout":
			if err := writeStdout("Sent %s to %s; timed out waiting %s for %s receipt\n", id, recipients[0], *waitTimeoutFlag, waitFor); err != nil {
				return err
			}
		default:
			if err := writeStdout("Sent %s to %s; wait error: %s\n", id, recipients[0], waitResult.Detail); err != nil {
				return err
			}
		}
		return waitErr
	} else {
		if err := writeStdout("Sent %s to %s (session: %s, root: %s)\n", id, strings.Join(recipients, ","), targetDisplay, deliveryRoot); err != nil {
			return err
		}
	}
	return nil
}

func loadPeerAgentsForSend(root *fsq.DeliveryRoot, strict bool) ([]string, bool, error) {
	configPresent := true
	agents, err := loadKnownAgentsWithRead(strict, func() ([]byte, error) {
		data, readErr := root.ReadRegularNoFollow(filepath.Join("meta", "config.json"))
		if os.IsNotExist(readErr) {
			configPresent = false
		}
		return data, readErr
	})
	return agents, configPresent, err
}

func revalidatePeerAgentsForSend(root *fsq.DeliveryRoot, configInitiallyPresent, strict bool) ([]string, error) {
	return revalidateSelectedAgentsForSend(root, configInitiallyPresent, strict, "peer")
}

func revalidateSourceAgentsForSend(root *fsq.DeliveryRoot, configInitiallyPresent, strict bool) ([]string, error) {
	return revalidateSelectedAgentsForSend(root, configInitiallyPresent, strict, "source")
}

func revalidateSelectedAgentsForSend(
	root *fsq.DeliveryRoot,
	configInitiallyPresent, strict bool,
	authority string,
) ([]string, error) {
	agents, configPresent, err := loadPeerAgentsForSend(root, strict)
	if err != nil {
		return nil, err
	}
	if !configInitiallyPresent || configPresent {
		return agents, nil
	}
	transition := fmt.Sprintf("selected %s config.json disappeared after initial validation", authority)
	if strict {
		return nil, errors.New(transition)
	}
	_ = writeStderr("warning: %s\n", transition)
	return agents, nil
}

func handleConfigured(agents []string, handle string) bool {
	for _, configured := range agents {
		if configured == handle {
			return true
		}
	}
	return false
}

func peerMailboxIncompleteError(project, session, recipient string, layoutErr error, root, baseRoot string, configured bool) error {
	peerContext := fmt.Sprintf("peer %q", project)
	if session != "" {
		peerContext += fmt.Sprintf(" session %q", session)
	}
	command := peerMailboxRepairCommandForOS(root, baseRoot, runtime.GOOS)
	runInstruction := "run"
	if runtime.GOOS == "windows" {
		runInstruction = "run in PowerShell"
	}
	if !configured {
		configRoot := baseRoot
		if configRoot == "" {
			configRoot = root
		}
		return fmt.Errorf(
			"%s mailbox for %q is incomplete: %w; add %q to agents in peer config %s, then %s: %s",
			peerContext,
			recipient,
			layoutErr,
			recipient,
			filepath.Join(configRoot, "meta", "config.json"),
			runInstruction,
			command,
		)
	}
	return fmt.Errorf(
		"%s mailbox for %q is incomplete: %w; ask the peer owner to %s: %s",
		peerContext,
		recipient,
		layoutErr,
		runInstruction,
		command,
	)
}

func peerMailboxRepairCommandForOS(root, baseRoot, goos string) string {
	return doctorMailboxRepairCommandForOS(root, baseRoot, goos)
}

func doctorMailboxRepairCommandForOS(root, baseRoot, goos string) string {
	return doctorRootCommandForOS(root, baseRoot, goos, "--fix-mailboxes")
}

func doctorRootCommandForOS(root, baseRoot, goos string, trailingArgs ...string) string {
	quote := shellQuoteArg
	if goos == "windows" {
		quote = quotePowerShellCommandArg
	}
	command := "amq doctor --root " + quote(root)
	if baseRoot != "" && filepath.Clean(baseRoot) != filepath.Clean(root) {
		command += " --base-root " + quote(baseRoot)
	}
	for _, arg := range trailingArgs {
		command += " " + arg
	}
	return command
}

func quotePowerShellCommandArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

type waitForResult struct {
	Event   string           `json:"event"`
	Stage   string           `json:"stage"`
	Timeout string           `json:"timeout,omitempty"`
	Detail  string           `json:"detail,omitempty"`
	Receipt *receipt.Receipt `json:"receipt,omitempty"`
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

func sourceSessionName(root, explicitSourceSession string) string {
	if explicitSourceSession != "" {
		return explicitSourceSession
	}
	return sessionName(root)
}
