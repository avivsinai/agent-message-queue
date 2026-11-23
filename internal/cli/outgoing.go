package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/followup"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

const followUpHelp = "Notify me if the recipient has not closed this request, or for another kind of message not replied, after this duration (e.g. 30m); the recipient sees the reply time"

const newsWithinHelp = "One notice if the recipient sends you nothing about this message within this time (e.g. 20m)"

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

	// followUp, when positive, arms a follow-up timer in the sender's mailbox
	// for the one recipient.
	followUp time.Duration
	// newsWithin, when positive, arms a news timer in the sender's mailbox
	// for the one recipient.
	newsWithin time.Duration

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
	// outID, when non-nil, receives the message id deliverOutgoing minted.
	outID *string
	// silent suppresses every printer from send/reply. The embedding command
	// owns its stdout. Outbox errors still reach the caller through return.
	silent bool

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
	if m.outID != nil {
		*m.outID = id
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
	if m.followUp > 0 && !m.silent {
		msg.Header.ReplyBy = now.Add(m.followUp).UTC().Format(time.RFC3339)
	}
	if m.newsWithin > 0 && !m.silent {
		msg.Header.NewsBy = now.Add(m.newsWithin).UTC().Format(time.RFC3339)
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
		_, currentSourceAgentsErr := revalidateSourceAgentsForSend(
			m.sourceConfigFS,
			m.sourceConfigPresent,
			m.strict,
		)
		if currentSourceAgentsErr != nil {
			return currentSourceAgentsErr
		}
		if err := checkEndpointRegistration(endpointQueueFor(m.sourceFS, m.sourceConfigFS), endpointSender, m.strict, false, m.me); err != nil {
			return err
		}
		_, currentPeerAgentsErr := revalidatePeerAgentsForSend(m.peerConfigFS, m.peerConfigPresent, m.strict)
		if currentPeerAgentsErr != nil {
			return currentPeerAgentsErr
		}
		if err := checkEndpointRegistration(endpointQueueFor(m.deliveryFS, m.peerConfigFS), endpointRecipient, m.strict, false, m.recipients...); err != nil {
			return err
		}
		if err := m.sourceFS.VerifyBase(); err != nil {
			return err
		}
		for _, r := range m.recipients {
			if _, err := fsq.DeliverToExistingInbox(m.deliveryFS, r, filename, data); err != nil {
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

	// The sender's outbox copy is written after delivery: the opposite order
	// would record a send that never happened.
	// It is an audit record in the source root.
	outboxDir := filepath.Join("agents", m.me, "outbox", "sent")
	outboxErr := error(nil)
	if _, err := m.sourceFS.WriteFileAtomic(outboxDir, filename, data, 0o600); err != nil {
		outboxErr = err
	}
	if m.silent {
		// The embedding command owns its stdout/stderr. Surface outbox errors
		// (only write failures, never success toasts) through the caller.
		return outboxErr
	}

	var followUp *followUpResult
	var followUpErr error
	if m.followUp > 0 {
		followUp, followUpErr = armFollowUp(m, id, now)
	}
	var news *followUpResult
	var newsErr error
	if m.newsWithin > 0 {
		news, newsErr = armNews(m, id, now)
	}
	followUpErr = joinOutcomeErr(followUpErr, newsErr)

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
		if followUp != nil {
			out["follow_up"] = followUp
		}
		if news != nil {
			out["news"] = news
		}
		if err := writeJSON(os.Stdout, out); err != nil {
			return err
		}
		return joinOutcomeErr(waitErr, followUpErr)
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
	} else if err := writeStdout("%s %s to %s (session: %s, root: %s)\n", m.verb, id, strings.Join(m.recipients, ","), m.displaySession, m.deliveryRoot); err != nil {
		return err
	}
	if followUp != nil && followUp.Due != "" {
		line := "Follow-up due %s if %s has not replied.\n"
		if slices.Contains(thread.OpenerKinds, m.kind) {
			line = "Follow-up due %s unless %s closes it.\n"
		}
		if err := writeStdout(line, followUp.Due, m.recipients[0]); err != nil {
			return err
		}
	}
	if news != nil && news.Due != "" {
		if err := writeStdout("News notice due %s if %s has sent no reply.\n", news.Due, m.recipients[0]); err != nil {
			return err
		}
	}
	return joinOutcomeErr(waitErr, followUpErr)
}

// followUpResult reports an armed follow-up, or why a delivered message has
// none.
type followUpResult struct {
	Due      string `json:"due,omitempty"`
	After    string `json:"after,omitempty"`
	NoticeID string `json:"notice_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

// armFollowUp writes the follow-up timer of the delivered message id, sent
// at now, in the sender's mailbox: the reply it waits for lands there.
func armFollowUp(m outgoingMessage, id string, now time.Time) (*followUpResult, error) {
	timer, err := followup.New(id, m.recipients[0], m.thread, m.subject, m.kind, now, m.followUp)
	return armTimer(m, id, timer, err, m.followUp, "follow-up")
}

// armNews writes the news timer of the delivered message id, sent at now,
// in the sender's mailbox.
func armNews(m outgoingMessage, id string, now time.Time) (*followUpResult, error) {
	timer, err := followup.NewNews(id, m.recipients[0], m.thread, m.subject, m.kind, now, m.newsWithin)
	return armTimer(m, id, timer, err, m.newsWithin, "news clock")
}

// armTimer writes timer, which was built with err, and reports it; clock
// names the timer in errors.
func armTimer(m outgoingMessage, id string, timer followup.Timer, err error, after time.Duration, clock string) (*followUpResult, error) {
	if err == nil {
		timer.TargetSession, timer.TargetProject = m.targetSession, m.targetProject
		err = followup.Write(m.sourceFS, m.me, timer)
	}
	var committed *fsq.CommittedDurabilityError
	if err != nil && !errors.As(err, &committed) {
		return &followUpResult{Error: err.Error()}, fmt.Errorf("%s %s was delivered, but its %s was not armed: %w", m.command, id, clock, err)
	}
	result := &followUpResult{
		Due:      timer.Due.Format(time.RFC3339),
		After:    after.String(),
		NoticeID: timer.NoticeID,
	}
	if err != nil {
		// The timer file is in place and fires; only its durability is
		// uncertain.
		result.Error = err.Error()
		return result, fmt.Errorf("%s %s %s is armed, but not yet durable: %w", m.command, id, clock, err)
	}
	return result, nil
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
