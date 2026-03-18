package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/resolve"
)

type announceDelivery struct {
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Status  string `json:"status"` // "delivered" or "failed"
	Error   string `json:"error,omitempty"`
}

func runAnnounce(args []string) error {
	fs := flag.NewFlagSet("announce", flag.ContinueOnError)
	common := addCommonFlags(fs)
	channelFlag := fs.String("channel", "", "Channel name (without #)")
	bodyFlag := fs.String("body", "", "Body string, @file, or empty to read stdin")
	subjectFlag := fs.String("subject", "", "Message subject")
	priorityFlag := fs.String("priority", "", "Message priority: urgent, normal, low")
	kindFlag := fs.String("kind", "", "Message kind: "+format.ValidKindsList())
	labelsFlag := fs.String("labels", "", "Comma-separated labels/tags")

	usage := usageWithFlags(fs, "amq announce --channel <name> [--body <str>] [--kind <k>] [--priority <p>]")
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

	channelName := strings.TrimSpace(*channelFlag)
	if channelName == "" {
		return UsageError("--channel is required")
	}
	channelName = strings.TrimPrefix(channelName, "#")
	if channelName == "" {
		return UsageError("channel name cannot be empty")
	}

	root := resolveRoot(common.Root)

	// Validate priority and kind
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

	body, err := readBody(*bodyFlag)
	if err != nil {
		return err
	}

	// Build resolver to resolve the channel address.
	// Use the same resolution helpers as send.go to respect AM_BASE_ROOT.
	baseRoot := resolveBaseRootForFederation(root)
	projectDir := resolveProjectDir()

	resolver := resolve.NewResolver(root, baseRoot, projectDir)

	// Parse channel address
	ep, err := resolve.ParseAddress("#" + channelName)
	if err != nil {
		return err
	}

	// Resolve channel to targets
	targets, err := resolver.Resolve(ep)
	if err != nil {
		return err
	}

	// Build the message
	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}

	recipients := make([]string, 0, len(targets))
	for _, t := range targets {
		recipients = append(recipients, t.Agent)
	}
	recipients = dedupeStrings(recipients)

	// Compute delivery scope.
	scope := computeDeliveryScope(root, targets)

	// Build resolved-to addresses for delivery metadata.
	resolvedToAddrs := make([]string, 0, len(targets))
	for _, t := range targets {
		resolvedToAddrs = append(resolvedToAddrs, formatResolvedTarget(t))
	}

	// Build origin (same pattern as send.go federation path).
	origin := buildOrigin(me, root, projectDir, false)

	// Build delivery metadata.
	channelAddr := "#" + channelName
	delivery := &format.Delivery{
		RequestedTo: []string{channelAddr},
		ResolvedTo:  resolvedToAddrs,
		Scope:       scope,
		Channel:     channelAddr,
	}

	msg := format.Message{
		Header: format.Header{
			Schema:   format.CurrentSchema,
			ID:       id,
			From:     me,
			To:       recipients,
			Thread:   "channel/" + channelName,
			Subject:  strings.TrimSpace(*subjectFlag),
			Created:  now.UTC().Format(time.RFC3339Nano),
			Priority: priority,
			Kind:     kind,
			Labels:   labels,
			Origin:   origin,
			Delivery: delivery,
		},
		Body: body,
	}

	filename := id + ".md"

	// Deliver to each target individually (may span sessions).
	deliveries := make([]announceDelivery, 0, len(targets))
	var deliveredCount int

	for i, target := range targets {
		// Set per-target fanout index in delivery metadata.
		msg.Header.Delivery.FanoutIndex = i + 1
		msg.Header.Delivery.FanoutTotal = len(targets)

		data, marshalErr := msg.Marshal()
		if marshalErr != nil {
			return marshalErr
		}

		d := announceDelivery{
			Agent:   target.Agent,
			Session: target.Session,
		}

		// Choose delivery method: DeliverToExistingInbox for foreign targets,
		// DeliverToInbox for local targets in the current session root.
		var deliverErr error
		if target.SessionRoot == root {
			_, deliverErr = fsq.DeliverToInbox(root, target.Agent, filename, data)
		} else {
			_, deliverErr = fsq.DeliverToExistingInbox(target.SessionRoot, target.Agent, filename, data)
		}

		if deliverErr != nil {
			d.Status = "failed"
			d.Error = deliverErr.Error()
		} else {
			d.Status = "delivered"
			deliveredCount++
		}
		deliveries = append(deliveries, d)
	}

	// Copy to sender outbox (use last marshaled data for audit).
	data, _ := msg.Marshal()
	outboxDir := fsq.AgentOutboxSent(root, me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	// Aggregate errors and determine exit condition BEFORE emitting any output.
	allFailed := deliveredCount == 0 && len(targets) > 0

	if common.JSON {
		result := map[string]any{
			"id":         id,
			"channel":    channelName,
			"thread":     "channel/" + channelName,
			"targets":    len(targets),
			"delivered":  deliveredCount,
			"scope":      scope,
			"deliveries": deliveries,
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		}
		if err := writeJSON(os.Stdout, result); err != nil {
			return err
		}
		if allFailed {
			return fmt.Errorf("all deliveries failed")
		}
		return nil
	}

	if outboxErr != nil {
		_ = writeStderr("warning: outbox write failed: %v\n", outboxErr)
	}
	if err := writeStdout("Announced %s to #%s (%d/%d delivered, scope: %s)\n", id, channelName, deliveredCount, len(targets), scope); err != nil {
		return err
	}
	for _, d := range deliveries {
		status := "ok"
		if d.Status == "failed" {
			status = "FAIL: " + d.Error
		}
		if err := writeStdout("  %s@%s: %s\n", d.Agent, d.Session, status); err != nil {
			return err
		}
	}

	if allFailed {
		return fmt.Errorf("all deliveries failed")
	}
	return nil
}
