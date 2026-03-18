package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/discover"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/resolve"
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

	usage := usageWithFlags(fs, "amq send --me <agent> --to <recipients> [options]")
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

	// Check if any recipient contains federation address characters (@, #, :).
	rawTo := strings.TrimSpace(*toFlag)
	if rawTo == "" {
		return UsageError("--to is required")
	}
	federated := hasQualifiedRecipient(rawTo)

	if federated {
		return runSendFederated(common, root, rawTo, *subjectFlag, *threadFlag,
			body, *ackFlag, *refsFlag, priority, kind, labels, context)
	}

	// --- Local send path (backward compatible, unchanged) ---
	recipients, err := splitRecipients(rawTo)
	if err != nil {
		if _, ok := err.(*ExitCodeError); ok {
			return err
		}
		return UsageError("--to: %v", err)
	}
	recipients = dedupeStrings(recipients)

	// Validate handles against config.json
	allHandles := append([]string{me}, recipients...)
	if err := validateKnownHandles(root, common.Strict, allHandles...); err != nil {
		return err
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

	session := sessionName(root)
	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"id":      id,
			"thread":  threadID,
			"to":      recipients,
			"subject": msg.Header.Subject,
			"session": session,
			"root":    root,
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
	if err := writeStdout("Sent %s to %s (session: %s, root: %s)\n", id, strings.Join(recipients, ","), session, root); err != nil {
		return err
	}
	return nil
}

// hasQualifiedRecipient returns true if ANY --to token contains @, #, or :
// characters, indicating a federated address.
func hasQualifiedRecipient(rawTo string) bool {
	return strings.ContainsAny(rawTo, "@#:")
}

// runSendFederated handles the federation send path where at least one
// recipient uses a qualified address (e.g., agent@session, #channel, agent@project:session).
func runSendFederated(common *commonFlags, root, rawTo, subject, thread,
	body string, ackRequired bool, refsFlag string, priority, kind string,
	labels []string, context map[string]any) error {

	me := common.Me

	// Parse each comma/space-separated recipient as a qualified address.
	rawParts := strings.FieldsFunc(rawTo, func(r rune) bool {
		return r == ',' || r == '\t' || r == '\n'
	})

	type parsedRecipient struct {
		raw string
		ep  resolve.Endpoint
	}
	var parsed []parsedRecipient
	seen := make(map[string]bool)

	for _, raw := range rawParts {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		ep, err := resolve.ParseAddress(raw)
		if err != nil {
			return UsageError("--to: invalid address %q: %v", raw, err)
		}
		key := ep.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		parsed = append(parsed, parsedRecipient{raw: raw, ep: ep})
	}
	if len(parsed) == 0 {
		return UsageError("--to is required")
	}

	// Collect bare-handle names for the To header (what the user asked for).
	requestedTo := make([]string, len(parsed))
	for i, p := range parsed {
		requestedTo[i] = p.raw
	}

	// Build resolver.
	baseRoot := resolveBaseRootForFederation(root)
	projectDir := resolveProjectDir()
	resolver := resolve.NewResolver(root, baseRoot, projectDir)

	// Resolve all endpoints to targets.
	type resolvedTarget struct {
		target resolve.Target
		ep     resolve.Endpoint
	}
	var allTargets []resolvedTarget
	for _, p := range parsed {
		targets, err := resolver.Resolve(p.ep)
		if err != nil {
			return fmt.Errorf("resolve %q: %w", p.raw, err)
		}
		for _, t := range targets {
			allTargets = append(allTargets, resolvedTarget{target: t, ep: p.ep})
		}
	}

	if len(allTargets) == 0 {
		return fmt.Errorf("no delivery targets resolved")
	}

	// Compute scope for the delivery metadata.
	justTargets := make([]resolve.Target, len(allTargets))
	for i, rt := range allTargets {
		justTargets[i] = rt.target
	}
	scope := computeDeliveryScope(root, justTargets)

	// Build the To header: bare agent handles of resolved targets.
	toHandles := make([]string, 0, len(allTargets))
	resolvedToAddrs := make([]string, 0, len(allTargets))
	for _, rt := range allTargets {
		toHandles = append(toHandles, rt.target.Agent)
		resolvedToAddrs = append(resolvedToAddrs, formatResolvedTarget(rt.target))
	}
	toHandles = dedupeStrings(toHandles)

	// Determine thread ID.
	threadID := strings.TrimSpace(thread)
	if threadID == "" {
		if len(toHandles) == 1 {
			threadID = canonicalP2P(me, toHandles[0])
		} else {
			return UsageError("--thread is required when sending to multiple recipients")
		}
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}

	// Build origin: identifies where this message came from.
	origin := buildOrigin(me, root, projectDir, ackRequired)

	// Build delivery metadata.
	delivery := &format.Delivery{
		RequestedTo: requestedTo,
		ResolvedTo:  resolvedToAddrs,
		Scope:       scope,
	}
	// If this was a channel send, record the channel.
	for _, p := range parsed {
		if p.ep.Kind == resolve.KindChannel {
			delivery.Channel = p.ep.String()
			break
		}
	}

	msg := format.Message{
		Header: format.Header{
			Schema:      format.CurrentSchema,
			ID:          id,
			From:        me,
			To:          toHandles,
			Thread:      threadID,
			Subject:     strings.TrimSpace(subject),
			Created:     now.UTC().Format(time.RFC3339Nano),
			AckRequired: ackRequired,
			Refs:        splitList(refsFlag),
			Priority:    priority,
			Kind:        kind,
			Labels:      labels,
			Context:     context,
			Origin:      origin,
			Delivery:    delivery,
		},
		Body: body,
	}

	filename := id + ".md"

	// Per-target delivery loop: tolerate partial failure.
	var delivered []string
	var deliveryErrors []string
	for i, rt := range allTargets {
		// Set per-target fanout index in delivery metadata.
		msg.Header.Delivery.FanoutIndex = i + 1
		msg.Header.Delivery.FanoutTotal = len(allTargets)

		data, err := msg.Marshal()
		if err != nil {
			return err
		}

		// Choose delivery method based on whether target is in the same session root.
		var deliverErr error
		if rt.target.SessionRoot == root {
			// Same session: use local delivery (MkdirAll is safe).
			_, deliverErr = fsq.DeliverToInbox(root, rt.target.Agent, filename, data)
		} else {
			// Cross-session/project: use existing-inbox delivery (no MkdirAll).
			_, deliverErr = fsq.DeliverToExistingInbox(rt.target.SessionRoot, rt.target.Agent, filename, data)
		}

		if deliverErr != nil {
			deliveryErrors = append(deliveryErrors, fmt.Sprintf("%s: %v", formatResolvedTarget(rt.target), deliverErr))
		} else {
			delivered = append(delivered, formatResolvedTarget(rt.target))
		}
	}

	// Outbox copy uses the last marshaled data (fanout fields vary, but the outbox
	// copy records the full message for audit; using the last is acceptable).
	data, _ := msg.Marshal()
	outboxDir := fsq.AgentOutboxSent(root, me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	session := sessionName(root)
	if common.JSON {
		result := map[string]any{
			"id":           id,
			"thread":       threadID,
			"to":           requestedTo,
			"resolved_to":  resolvedToAddrs,
			"delivered":    delivered,
			"subject":      msg.Header.Subject,
			"session":      session,
			"root":         root,
			"scope":        scope,
			"federated":    true,
			"fanout_total": len(allTargets),
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		}
		if len(deliveryErrors) > 0 {
			result["errors"] = deliveryErrors
		}
		return writeJSON(os.Stdout, result)
	}

	if outboxErr != nil {
		_ = writeStderr("warning: outbox write failed: %v\n", outboxErr)
	}
	for _, e := range deliveryErrors {
		_ = writeStderr("warning: delivery failed: %s\n", e)
	}

	if len(delivered) == 0 && len(deliveryErrors) > 0 {
		return fmt.Errorf("all deliveries failed")
	}

	_ = writeStdout("Sent %s to %s (session: %s, scope: %s)\n",
		id, strings.Join(delivered, ","), session, scope)

	if len(deliveryErrors) > 0 {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("%d of %d deliveries failed", len(deliveryErrors), len(allTargets))}
	}
	return nil
}

// resolveBaseRootForFederation returns the base root directory for federation.
// It checks AM_BASE_ROOT first (set by coop exec), then derives from the
// session root (parent directory), then falls back to resolveBaseRoot().
func resolveBaseRootForFederation(sessionRoot string) string {
	if base := strings.TrimSpace(os.Getenv(envBaseRoot)); base != "" {
		return base
	}
	// Session root is typically base/session — parent is the base root.
	parent := filepath.Dir(sessionRoot)
	if parent != "" && parent != "." && parent != sessionRoot {
		return parent
	}
	return resolveBaseRoot()
}

// resolveProjectDir returns the current project directory for federation.
// It checks AM_PROJECT env to find the project, then falls back to cwd.
func resolveProjectDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// computeDeliveryScope determines the federation scope based on the delivery targets.
func computeDeliveryScope(senderRoot string, targets []resolve.Target) string {
	for _, t := range targets {
		if t.Project != "" {
			return "cross-project"
		}
		if t.SessionRoot != senderRoot {
			return "cross-session"
		}
	}
	return "local"
}

// formatResolvedTarget returns a human-readable string for a resolved target.
func formatResolvedTarget(t resolve.Target) string {
	if t.Project != "" {
		return t.Agent + "@" + t.Project + ":" + t.Session
	}
	if t.Session != "" {
		return t.Agent + "@" + t.Session
	}
	return t.Agent
}

// buildOrigin constructs an Origin for a federated message.
// It resolves the project slug with the following precedence:
//  1. AM_PROJECT env var (fastest, set by coop exec)
//  2. discover.DiscoverProject(projectDir) using the .amqrc Slug/ProjectID
//
// If neither yields a project, the Origin omits the project qualifier.
func buildOrigin(me, root, projectDir string, ackRequired bool) *format.Origin {
	session := sessionName(root)
	origin := &format.Origin{
		Agent:   me,
		Session: session,
		ReplyTo: me + "@" + session,
	}

	// Try AM_PROJECT first (set by coop exec).
	proj := strings.TrimSpace(os.Getenv(envProject))

	// Fallback: discover project from the working directory.
	if proj == "" && projectDir != "" {
		if discovered, err := discover.DiscoverProject(projectDir); err == nil && discovered.Slug != "" {
			proj = discovered.Slug
			if discovered.ProjectID != "" {
				origin.ProjectID = discovered.ProjectID
			}
		}
	}

	if proj != "" {
		origin.Project = proj
		origin.ReplyTo = me + "@" + proj + ":" + session
	}

	if ackRequired {
		origin.AckTo = origin.ReplyTo
	}
	return origin
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
