package cli

import (
	"errors"
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

// outgoingMessage is a send or reply whose route, capabilities, and rosters
// are authorized. deliverOutgoing owns every step from there to the report.
type outgoingMessage struct {
	command string // names --wait-for errors: "send" or "reply"
	verb    string // leads the text report: "Sent" or "Replied"

	me         string
	recipients []string
	thread     string
	subject    string
	refs       []string
	priority   string
	kind       string
	labels     []string
	context    map[string]any
	body       string

	// sourceSession is the sender's session, empty at a base root. It decides
	// whether reply_to names handle@session or the bare handle.
	sourceSession string
	sourceProject string
	targetProject string
	targetSession string

	sourceFS     *fsq.DeliveryRoot
	deliveryFS   *fsq.DeliveryRoot
	deliveryRoot string
	strict       bool

	// localAuthorization, when set, prepares local mailboxes and must still
	// verify immediately before delivery.
	localAuthorization *fsq.MailboxConfigAuthorization
	// Cross-project deliveries re-check both rosters through these
	// capabilities immediately before delivery.
	sourceConfigFS      *fsq.DeliveryRoot
	sourceConfigPresent bool
	peerConfigFS        *fsq.DeliveryRoot
	peerConfigPresent   bool
	// explainPeerDeliveryError, when set, may replace an uncommitted peer
	// delivery error with a diagnosis; nil keeps the delivery error.
	explainPeerDeliveryError func(recipient string) error

	waitFor     string
	waitTimeout time.Duration
	// waitTimeoutHint follows the standard --wait-for timeout text.
	waitTimeoutHint string

	json bool
	// displaySession is the report's session; sourceSessionDisplay is its
	// source_session on cross-session reports.
	displaySession       string
	sourceSessionDisplay string
	jsonExtra            map[string]any
	// propagateOutboxReportError returns a failed outbox warning write.
	propagateOutboxReportError bool
}

// deliverOutgoing builds, delivers, records, and reports one authorized
// message. Local delivery verifies its mailbox authorization last;
// cross-project delivery re-checks both rosters and never creates peer
// directories; any routed delivery verifies the source base first.
func deliverOutgoing(m outgoingMessage) error {
	priority := m.priority
	if m.kind != "" && priority == "" {
		priority = format.DefaultPriority(m.kind)
	}
	routedProject := ""
	if m.targetProject != "" {
		routedProject = m.sourceProject
	}

	now := time.Now()
	id, err := format.NewMessageID(now)
	if err != nil {
		return err
	}
	msg := format.Message{
		Header: format.Header{
			Schema:       format.CurrentSchema,
			ID:           id,
			From:         m.me,
			To:           m.recipients,
			Thread:       m.thread,
			Subject:      m.subject,
			Created:      now.UTC().Format(time.RFC3339Nano),
			Refs:         m.refs,
			Priority:     priority,
			Kind:         m.kind,
			Labels:       m.labels,
			Context:      m.context,
			ReplyTo:      replyRouteFor(m.me, m.sourceSession, m.targetProject, m.targetSession),
			ReplyProject: routedProject,
			FromProject:  routedProject,
		},
		Body: m.body,
	}
	data, err := msg.Marshal()
	if err != nil {
		return err
	}

	if m.localAuthorization != nil {
		if err := prepareLocalSendMailboxes(m.deliveryFS, m.localAuthorization, m.deliveryRoot, m.recipients); err != nil {
			return err
		}
		if err := m.localAuthorization.Verify(); err != nil {
			return fmt.Errorf("destination mailbox authorization changed before delivery: %w", err)
		}
	}

	filename := id + ".md"
	if m.targetProject != "" {
		currentSourceAgents, currentSourceAgentsErr := revalidateSourceAgentsForSend(
			m.sourceConfigFS,
			m.sourceConfigPresent,
			m.strict,
		)
		if err := validateKnownHandlesFromAgents(currentSourceAgents, currentSourceAgentsErr, m.strict, m.me); err != nil {
			return err
		}
		currentPeerAgents, currentPeerAgentsErr := revalidatePeerAgentsForSend(m.peerConfigFS, m.peerConfigPresent, m.strict)
		if err := validateKnownHandlesFromAgents(currentPeerAgents, currentPeerAgentsErr, m.strict, m.recipients...); err != nil {
			return err
		}
		if err := m.sourceFS.VerifyBase(); err != nil {
			return err
		}
		for _, r := range m.recipients {
			if _, err := deliverToExistingInbox(m.deliveryFS, r, filename, data); err != nil {
				var committed *fsq.CommittedDurabilityError
				if !errors.As(err, &committed) && m.explainPeerDeliveryError != nil {
					if explained := m.explainPeerDeliveryError(r); explained != nil {
						return explained
					}
				}
				return reportDeliveryError(id, err)
			}
		}
	} else {
		if m.targetSession != "" {
			if err := m.sourceFS.VerifyBase(); err != nil {
				return err
			}
		}
		if _, err := fsq.DeliverToInboxes(m.deliveryFS, m.recipients, filename, data); err != nil {
			return reportDeliveryError(id, err)
		}
	}

	// Best-effort presence touch.
	_ = presence.TouchDeliveryRoot(m.sourceFS, m.me)

	// The sender's outbox copy is an audit record in the source root.
	outboxDir := filepath.Join("agents", m.me, "outbox", "sent")
	outboxErr := error(nil)
	if _, err := m.sourceFS.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}

	var waitResult *waitForResult
	var waitErr error
	if m.waitFor != "" {
		r, err := receipt.WaitForDeliveryRoot(m.deliveryFS, id, m.recipients[0], m.waitFor, m.waitTimeout, 1*time.Second)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			waitResult = &waitForResult{Event: "timeout", Stage: m.waitFor, Timeout: m.waitTimeout.String()}
			waitErr = TimeoutError("%s --wait-for %s timed out after %s%s", m.command, m.waitFor, m.waitTimeout, m.waitTimeoutHint)
		} else if err != nil {
			waitResult = &waitForResult{Event: "error", Stage: m.waitFor, Detail: err.Error()}
			waitErr = fmt.Errorf("%s --wait-for: %w", m.command, err)
		} else {
			waitResult = &waitForResult{Event: "matched", Stage: m.waitFor, Receipt: &r}
		}
	}

	if m.json {
		out := map[string]any{
			"id":      id,
			"thread":  m.thread,
			"to":      m.recipients,
			"subject": m.subject,
			"session": m.displaySession,
			"root":    m.deliveryRoot,
			"outbox":  outboxResult(outboxErr),
		}
		for key, value := range m.jsonExtra {
			out[key] = value
		}
		if m.targetProject != "" {
			out["cross_project"] = true
			out["source_project"] = m.sourceProject
			out["target_project"] = m.targetProject
		}
		if m.targetSession != "" {
			out["cross_session"] = true
			out["source_session"] = m.sourceSessionDisplay
			out["target_session"] = m.targetSession
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
		if err := reportOutboxError(outboxErr); err != nil && m.propagateOutboxReportError {
			return err
		}
	}
	if waitResult != nil {
		recipient := m.recipients[0]
		switch waitResult.Event {
		case "matched":
			if err := writeStdout("%s %s to %s; %s by %s at %s\n", m.verb, id, recipient, m.waitFor, recipient, waitResult.Receipt.EmittedAt); err != nil {
				return err
			}
		case "timeout":
			if err := writeStdout("%s %s to %s; timed out waiting %s for %s receipt\n", m.verb, id, recipient, m.waitTimeout, m.waitFor); err != nil {
				return err
			}
		default:
			if err := writeStdout("%s %s to %s; wait error: %s\n", m.verb, id, recipient, waitResult.Detail); err != nil {
				return err
			}
		}
		return waitErr
	}
	return writeStdout("%s %s to %s (session: %s, root: %s)\n", m.verb, id, strings.Join(m.recipients, ","), m.displaySession, m.deliveryRoot)
}

// replyRouteFor stamps reply_to only on messages that cross a session or
// project boundary; a local message replies locally and needs no hint. A
// sender in a session is reachable as handle@session; a cross-project sender
// at a base root is reachable by the bare handle alongside reply_project.
func replyRouteFor(me, sourceSession, targetProject, targetSession string) string {
	if targetProject == "" && targetSession == "" {
		return ""
	}
	if sourceSession != "" {
		return me + "@" + sourceSession
	}
	if targetProject != "" {
		return me
	}
	return ""
}

func validatePriorityKind(priority, kind string) error {
	if !format.IsValidPriority(priority) {
		return UsageError("--priority must be one of: urgent, normal, low")
	}
	if !format.IsValidKind(kind) {
		return UsageError("--kind must be one of: %s", format.ValidKindsList())
	}
	return nil
}

// openSourceConfigSelection selects the sender's roster authority for a
// cross-project delivery: the local config authority of the source root.
func openSourceConfigSelection(
	sourceFS *fsq.DeliveryRoot,
	sourceRoot string,
	pin sessionPin,
	ignoreSessionPin bool,
) (mailboxConfigSelection, error) {
	configBase, expectedBaseRootID := localMailboxConfigAuthority(sourceRoot, pin, ignoreSessionPin)
	return openMailboxConfigSelection(sourceFS, sourceRoot, configBase, expectedBaseRootID)
}
