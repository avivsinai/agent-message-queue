package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

const (
	handoffSchema   = "amq/handoff/v1"
	handedOffLabel  = "handed-off"
	handoffLockName = ".handoff.lock"

	handoffSent        = "sent"
	handoffAlreadySent = "already sent"
	handoffFailed      = "failed"
	handoffNotSent     = "not sent"
	handoffTextStands  = "the earlier text stands"
)

// commitIDPattern is a full object id: SHA-1 or SHA-256, lowercase hex.
var commitIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// handoffLeg is one message of a hand-off and what the run did with it.
type handoffLeg struct {
	ID       string          `json:"id"`
	To       string          `json:"to"`
	State    string          `json:"state"`
	Error    string          `json:"error,omitempty"`
	Note     string          `json:"note,omitempty"`
	FollowUp *followUpResult `json:"follow_up,omitempty"`
	News     *followUpResult `json:"news,omitempty"`
}

type handoffReport struct {
	Schema  string     `json:"schema"`
	Request string     `json:"request"`
	Commit  string     `json:"commit"`
	Review  handoffLeg `json:"review"`
	Notice  handoffLeg `json:"notice"`
}

// handoff is one run of amq handoff after its checks: the request the
// caller owes, the reviewer, the commit, and the body as given.
type handoff struct {
	me       string
	request  string
	reviewer string // handle
	commit   string
	body     string

	reviewerLabel  string
	requesterLabel string

	report    handoffReport
	reviewOut outgoingOutcome
	reviewMsg outgoingMessage
	legErr    error
}

// runHandoff implements amq handoff: a review request for an exact commit to
// a reviewer, then a status to the requester of the request the caller owes.
// Each leg runs as the send or reply step that routes and authorizes it, and
// both steps authorize before either leg writes. A leg already in the
// caller's outbox is not sent again, so a rerun completes a partial run.
func runHandoff(args []string) error {
	fs := flag.NewFlagSet("handoff", flag.ContinueOnError)
	common := addCommonFlags(fs)
	idFlag := fs.String("id", "", "Id of the open request you owe")
	toFlag := fs.String("to", "", "Reviewer handle (exactly one)")
	commitFlag := fs.String("commit", "", "Full commit id of the candidate (40 or 64 lowercase hex characters)")
	bodyFlag := fs.String("body", "", "Body string, @file, or - / empty to read stdin")
	subjectFlag := fs.String("subject", "", "Review request subject (default: Review: <original subject>)")
	sessionFlag := fs.String("session", "", "Reviewer's session, as on send")
	projectFlag := fs.String("project", "", "Reviewer's peer project, as on send")
	followUpFlag := fs.Duration("follow-up", 0, "Notify me if the reviewer has not closed the review request after this duration (e.g. 30m)")
	newsWithinFlag := fs.Duration("news-within", 0, "One notice if the reviewer sends no reply about the review request within this time (e.g. 20m)")

	usage := usageWithFlags(fs, "amq handoff --me <agent> --id <request> --to <reviewer> --commit <sha> --body <text|@file> [options]",
		"Hand a candidate to a reviewer and tell the requester, in one command.",
		"The reviewer gets a review_request for the commit. The requester of --id gets a",
		"status labelled handed-off that names the reviewer, the commit and the review request.",
		"The request stays open: close it yourself when the reviewed candidate is ready.",
		"",
		"--id must be an open request that you owe. Both messages are routed and authorized",
		"before either is written. A message already in your outbox is not sent again, so",
		"running the same command again completes a hand-off that stopped part way.",
		"",
		"Example:",
		"  amq handoff --id <request> --to reviewer --commit <sha> --body \"ready for review\" --follow-up 1h")
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
	request := strings.TrimSpace(*idFlag)
	if request == "" {
		return UsageError("--id is required")
	}
	if _, err := ensureFilename(request); err != nil {
		return UsageError("--id: %v", err)
	}
	reviewers, err := splitRecipients(strings.TrimSpace(*toFlag))
	if err != nil {
		if _, ok := err.(*ExitCodeError); ok {
			return err
		}
		return UsageError("--to: %v", err)
	}
	reviewers = dedupeStrings(reviewers)
	if len(reviewers) != 1 {
		return UsageError("--to takes exactly one reviewer (got %d)", len(reviewers))
	}
	commit := strings.TrimSpace(*commitFlag)
	if !commitIDPattern.MatchString(commit) {
		return UsageError("--commit must be a full commit id: 40 or 64 lowercase hex characters")
	}
	if *followUpFlag < 0 {
		return UsageError("--follow-up must be >= 0")
	}
	if *newsWithinFlag < 0 {
		return UsageError("--news-within must be >= 0")
	}

	body, err := readBody(*bodyFlag, false)
	if err != nil {
		return err
	}

	root := absPath(resolveRoot(common.Root))
	if !dirExists(root) {
		return NotFoundError("root not found at %s", root)
	}
	scanRoot := threadOpenScanRoot(root)
	scan, red, err := reduceObligations([]thread.ScanRoot{scanRoot}, time.Now().UTC().Add(-thread.DefaultObligationMaxAge), func(string, error) error { return nil })
	if err != nil {
		return err
	}
	mine := thread.Participant{Session: scanRoot.Session, Handle: me}
	ob, err := findOwedRequest(scan.Entries, red.Obligations, request, mine)
	if err != nil {
		return err
	}
	session := strings.TrimSpace(*sessionFlag)
	project := strings.TrimSpace(*projectFlag)
	reviewer := thread.Participant{Project: project, Session: session, Handle: reviewers[0]}
	if reviewer.Session == "" {
		reviewer.Session = mine.Session
	}
	if reviewer == mine {
		return errors.New("the reviewer is you; hand off to another agent")
	}
	if reviewer == ob.Requester {
		return errors.New("the reviewer is the requester of this request; answer the request with amq reply")
	}

	h := &handoff{
		me:             me,
		request:        request,
		reviewer:       reviewer.Handle,
		commit:         commit,
		body:           body,
		reviewerLabel:  participantLabel(reviewer),
		requesterLabel: participantLabel(ob.Requester),
	}
	h.report = handoffReport{
		Schema:  handoffSchema,
		Request: request,
		Commit:  commit,
		Review:  handoffLeg{To: h.reviewerLabel, State: handoffNotSent},
		Notice:  handoffLeg{To: h.requesterLabel, State: handoffNotSent},
	}

	subject := strings.TrimSpace(*subjectFlag)
	if subject == "" {
		original := ob.Opener.Subject
		if original == "" {
			original = "(no subject)"
		}
		subject = "Review: " + original
	}
	reviewContext, err := json.Marshal(map[string]any{"handoff": map[string]any{
		"request":   request,
		"requester": h.requesterLabel,
		"reviewer":  h.reviewerLabel,
		"commit":    commit,
	}})
	if err != nil {
		return err
	}
	stepArgs := []string{"--me", me}
	if common.rootExplicit() {
		stepArgs = append(stepArgs, "--root", common.Root)
	}
	if common.Strict {
		stepArgs = append(stepArgs, "--strict")
	}
	leg1Args := append(slices.Clone(stepArgs), "--to", reviewer.Handle)
	if session != "" {
		leg1Args = append(leg1Args, "--session", session)
	}
	if project != "" {
		leg1Args = append(leg1Args, "--project", project)
	}
	leg1Args = append(leg1Args,
		"--kind", format.KindReviewRequest,
		"--subject", subject,
		"--context", string(reviewContext))
	if *followUpFlag > 0 {
		leg1Args = append(leg1Args, "--follow-up", followUpFlag.String())
	}
	if *newsWithinFlag > 0 {
		leg1Args = append(leg1Args, "--news-within", newsWithinFlag.String())
	}
	leg2Args := append(slices.Clone(stepArgs), "--id", request, "--kind", format.KindStatus, "--labels", handedOffLabel)

	ran := false
	err = runSendStep(leg1Args, nil, func(m1 outgoingMessage) error {
		return runReplyStep(leg2Args, func(m2 outgoingMessage) error {
			// The lock is taken once both steps passed their root and
			// session guards, and it covers every outbox look-up and write.
			release, err := lockHandoffs(m1.sourceFS, me)
			if err != nil {
				return err
			}
			defer release()
			ran = true
			h.handOff(m1, m2)
			return nil
		})
	})
	if !ran {
		return err
	}
	return h.print(common.JSON)
}

// lockHandoffs takes the caller's hand-off lock, which serializes every
// hand-off of one agent, waiting while another run holds it.
func lockHandoffs(root *fsq.DeliveryRoot, me string) (func(), error) {
	if !lock.AdvisoryLockAvailable() {
		return nil, errors.New("a hand-off needs advisory file locks, which this platform lacks")
	}
	file, err := root.OpenLockFile(filepath.Join("agents", me), handoffLockName, 0o600)
	if err != nil {
		return nil, fmt.Errorf("hand-off lock: %w", err)
	}
	if err := lock.Lock(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("hand-off lock: %w", err)
	}
	return func() {
		_ = lock.Unlock(file)
		_ = file.Close()
	}, nil
}

// findOwedRequest returns the open obligation of the opener id that mine
// owes, or the refusal that says why there is none.
func findOwedRequest(entries []thread.Entry, obligations []*thread.Obligation, id string, mine thread.Participant) (*thread.Obligation, error) {
	closed, requested := false, false
	for _, ob := range obligations {
		if ob.Opener.ID != id {
			continue
		}
		switch {
		case ob.Owner == mine && ob.Open():
			return ob, nil
		case ob.Owner == mine && ob.Closed:
			closed = true
		case ob.Requester == mine:
			requested = true
		}
	}
	switch {
	case closed:
		return nil, fmt.Errorf("request %s is closed; a hand-off is for an open request you owe", id)
	case requested:
		return nil, errors.New("you sent this request; a hand-off is for a request you owe")
	}
	for _, entry := range entries {
		if entry.ID == id && !thread.OpensObligation(entry.Kind, entry.Labels) {
			return nil, fmt.Errorf("message %s is not a request (kind %q); a hand-off is for a request you owe", id, entry.Kind)
		}
	}
	return nil, fmt.Errorf("request %s not found among your open requests", id)
}

// handOff writes the legs the caller's outbox does not hold yet: the review
// request, then, once it is sent or already sent, the notice.
func (h *handoff) handOff(m1, m2 outgoingMessage) {
	m1.body = h.body
	h.reviewMsg = m1
	review := &h.report.Review
	if found, ok := h.findSent(m1.sourceFS, h.isReview); ok {
		review.ID, review.State = found.ID, handoffAlreadySent
		review.Note = h.earlierText(m1.sourceFS, found, m1.body)
	} else {
		o, err := sendOutgoing(m1)
		review.ID = o.ID
		if err != nil {
			review.State, review.Error = handoffFailed, err.Error()
			h.legErr = err
			return
		}
		h.reviewOut = o
		review.State, review.FollowUp, review.News = handoffSent, o.FollowUp, o.News
		if o.OutboxErr != nil {
			review.Error = "outbox: " + o.OutboxErr.Error()
		}
	}

	m2.body = fmt.Sprintf("Handed to %s for review: %s. Review request %s.\n\n%s", h.reviewerLabel, h.commit, review.ID, h.body)
	m2.context = map[string]any{"handoff": map[string]any{
		"review":   review.ID,
		"reviewer": h.reviewerLabel,
		"commit":   h.commit,
	}}
	notice := &h.report.Notice
	if found, ok := h.findSent(m2.sourceFS, h.isNotice); ok {
		notice.ID, notice.State = found.ID, handoffAlreadySent
		notice.Note = h.earlierText(m2.sourceFS, found, m2.body)
		return
	}
	o, err := sendOutgoing(m2)
	notice.ID = o.ID
	if err != nil {
		notice.State, notice.Error = handoffFailed, err.Error()
		h.legErr = err
		return
	}
	notice.State = handoffSent
	if o.OutboxErr != nil {
		notice.Error = "outbox: " + o.OutboxErr.Error()
	}
}

// isReview matches the review request of this hand-off.
func (h *handoff) isReview(hdr format.Header) bool {
	ctx := handoffContext(hdr)
	return hdr.Kind == format.KindReviewRequest &&
		slices.Equal(hdr.To, []string{h.reviewer}) &&
		ctx["request"] == h.request && ctx["reviewer"] == h.reviewerLabel && ctx["commit"] == h.commit
}

// isNotice matches the notice of this hand-off to the requester.
func (h *handoff) isNotice(hdr format.Header) bool {
	return hdr.Kind == format.KindStatus &&
		slices.Contains(hdr.Labels, handedOffLabel) &&
		slices.Contains(hdr.Refs, h.request) &&
		handoffContext(hdr)["review"] == h.report.Review.ID
}

func handoffContext(hdr format.Header) map[string]any {
	ctx, _ := hdr.Context["handoff"].(map[string]any)
	return ctx
}

// sentMessage is a message found in the caller's outbox/sent.
type sentMessage struct {
	ID   string
	Name string
}

// findSent returns the oldest message in the caller's outbox/sent whose
// header matches, reading headers only.
func (h *handoff) findSent(root *fsq.DeliveryRoot, match func(format.Header) bool) (sentMessage, bool) {
	dir := filepath.Join("agents", h.me, "outbox", "sent")
	entries, err := root.ReadDir(dir)
	if err != nil {
		return sentMessage{}, false
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		name := filepath.Join(dir, entry.Name())
		hdr, err := format.ReadHeaderFileRoot(root, name)
		if err != nil || !match(hdr) {
			continue
		}
		return sentMessage{ID: hdr.ID, Name: name}, true
	}
	return sentMessage{}, false
}

// earlierText returns the note for a leg found already sent whose stored
// body is not the body this run would send.
func (h *handoff) earlierText(root *fsq.DeliveryRoot, found sentMessage, body string) string {
	msg, err := format.ReadMessageFileRoot(root, found.Name)
	if err == nil {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if msg.Body == body {
			return ""
		}
	}
	return handoffTextStands
}

// print reports both legs and returns the error of a failed leg or of a
// clock that is not armed.
func (h *handoff) print(asJSON bool) error {
	review, notice := h.report.Review, h.report.Notice
	var errs []error
	if h.legErr != nil {
		errs = append(errs, h.legErr)
	}
	if h.reviewOut.ClockErr != nil {
		errs = append(errs, h.reviewOut.ClockErr)
	}
	var outErr error
	if len(errs) > 0 {
		outErr = WithExitCode(1, errors.Join(errs...))
	}
	if asJSON {
		if err := writeJSON(os.Stdout, h.report); err != nil {
			return err
		}
		return outErr
	}
	if err := writeStdout("%s\n", legLine("Review request", review)); err != nil {
		return err
	}
	if review.State == handoffSent {
		if err := printClockLines(h.reviewMsg, h.reviewOut); err != nil {
			return err
		}
	}
	if err := writeStdout("%s\n", legLine("Notice", notice)); err != nil {
		return err
	}
	if done(review) && !done(notice) {
		if err := writeStdout("run the same command again to complete the hand-off\n"); err != nil {
			return err
		}
	}
	return outErr
}

func done(leg handoffLeg) bool {
	return leg.State == handoffSent || leg.State == handoffAlreadySent
}

// legLine is one line of the text report: "<what> <id> to <who>: <state>".
func legLine(what string, leg handoffLeg) string {
	line := what
	if leg.ID != "" {
		line += " " + leg.ID
	}
	line += " to " + leg.To + ": " + leg.State
	if leg.Error != "" {
		line += ": " + leg.Error
	}
	if leg.Note != "" {
		line += "; " + leg.Note
	}
	return line
}
