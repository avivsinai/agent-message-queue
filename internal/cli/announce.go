package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/discover"
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

	// Build resolver to resolve the channel address
	baseRoot := filepath.Dir(root)
	projectDir := ""
	cwd, cwdErr := os.Getwd()
	if cwdErr == nil {
		proj, projErr := discover.DiscoverProject(cwd)
		if projErr == nil {
			baseRoot = proj.BaseRoot
			projectDir = proj.Dir
		}
	}

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

	msg := format.Message{
		Header: format.Header{
			Schema:  format.CurrentSchema,
			ID:      id,
			From:    me,
			To:      recipients,
			Thread:  "channel/" + channelName,
			Subject: strings.TrimSpace(*subjectFlag),
			Created: now.UTC().Format(time.RFC3339Nano),
			Priority: priority,
			Kind:     kind,
			Labels:   labels,
		},
		Body: body,
	}

	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	filename := id + ".md"

	// Deliver to each target individually (may span sessions)
	deliveries := make([]announceDelivery, 0, len(targets))
	var deliveredCount int

	for _, target := range targets {
		d := announceDelivery{
			Agent:   target.Agent,
			Session: target.Session,
		}
		_, deliverErr := fsq.DeliverToInbox(target.SessionRoot, target.Agent, filename, data)
		if deliverErr != nil {
			d.Status = "failed"
			d.Error = deliverErr.Error()
		} else {
			d.Status = "delivered"
			deliveredCount++
		}
		deliveries = append(deliveries, d)
	}

	// Copy to sender outbox
	outboxDir := fsq.AgentOutboxSent(root, me)
	outboxErr := error(nil)
	if _, err := fsq.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"id":         id,
			"channel":    channelName,
			"thread":     "channel/" + channelName,
			"targets":    len(targets),
			"delivered":  deliveredCount,
			"deliveries": deliveries,
			"outbox": map[string]any{
				"written": outboxErr == nil,
				"error":   errString(outboxErr),
			},
		})
	}

	if outboxErr != nil {
		_ = writeStderr("warning: outbox write failed: %v\n", outboxErr)
	}
	if err := writeStdout("Announced %s to #%s (%d/%d delivered)\n", id, channelName, deliveredCount, len(targets)); err != nil {
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
	return nil
}
