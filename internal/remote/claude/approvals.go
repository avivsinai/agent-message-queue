package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// The attachment side of DM approvals (bead 611.42.3). The poller binds
// each PermissionRequest the hook raised to the run whose prompt it
// belongs to and shows the oldest open one to the endpoint. One file
// decides how each approval ended: the first create-new of
// resolved/<iid>.json. The hook prints a decision only after it won that
// create-new with answered. The poller tries it with answered_elsewhere
// when the bound call's tool_result appears or the hook died, and with
// run_ended when the run ends. Whoever loses adopts the file on disk, so
// memory, events and disk always name the same outcome.

// approval is one PermissionRequest bound to a run.
type approval struct {
	id       string
	toolName string
	preview  string
	hash     string
	hookPID  int
	deadline time.Time
	// openedAt is when the hook raised it (unix ms): a tool_result stamped
	// earlier belongs to an earlier call.
	openedAt int64
	// toolUseID is the one call this approval is bound to; uncertain means
	// two or more calls matched, so approve is not offered and the first
	// tool_result among them closes it.
	toolUseID string
	uncertain bool
	// seenSeq is the poll pass that found the request; only a pass of the
	// same or a later number read the transcript after it was found.
	seenSeq uint64
	// sessionID is the session whose directory holds this approval's files.
	// Its evidence is read and arbitrated there only, also after the
	// registry moved to another session.
	sessionID string
	// approvable is the hook's word that it can apply a proven allow for
	// this call; pinned that the installed hook and the serving share name
	// the same owner. Both decide only whether the DM offers allow.
	approvable, pinned bool
}

// offersAllow reports whether the DM offers allow for ap: the hook can
// apply one, the owner is pinned, and the approval is bound to its one
// tool_use. Otherwise the approval is reject only.
func (ap *approval) offersAllow() bool {
	return ap.approvable && ap.pinned && ap.toolUseID != "" && !ap.uncertain
}

// openRef names one open approval and the session its files live in.
type openRef struct{ id, sessionID string }

// toolCall is one tool_use of a run's turn and when its tool_result
// appeared (unix ms, 0 while none has).
type toolCall struct {
	id, name, hash string
	resultTS       int64
	resulted       bool
	boundTo        string
}

// maxRunToolCalls bounds the calls one run keeps; the oldest answered ones
// go first.
const maxRunToolCalls = 1024

// approvalDisk is one poll's read of the approval files, taken outside a.mu.
type approvalDisk struct {
	requests map[string]approvalRequest
	resolved map[string]approvalResolved
	// pinned is allowPinned for the session, read when a request is new.
	pinned bool
}

// readApprovalDisk reads every request not yet known and the resolved file
// of each open approval and each new request, so a resolution saved while
// the endpoint was down is adopted when its request is found again. known
// holds the ids already bound.
func (a *Attachment) readApprovalDisk(sessionID string, known map[string]bool, open []openRef) approvalDisk {
	d := approvalDisk{requests: map[string]approvalRequest{}, resolved: map[string]approvalResolved{}}
	dir := approveDir(a.home, sessionID)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return d
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "requests")); err == nil {
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || !e.Type().IsRegular() || known[id] || !interactionIDRe.MatchString(id) {
				continue
			}
			var r approvalRequest
			if readApprovalJSON(filepath.Join(dir, "requests", e.Name()), &r) != nil {
				continue // partial or foreign: read again next tick
			}
			if r.Protocol != approvalProtocol || r.InteractionID != id || r.SessionID != sessionID {
				continue
			}
			d.requests[id] = r
			open = append(open, openRef{id: id, sessionID: sessionID})
		}
	}
	for _, o := range open {
		if r, ok := readResolved(a.home, o.sessionID, o.id); ok {
			d.resolved[o.id] = r
		}
	}
	if len(d.requests) > 0 {
		d.pinned = allowPinned(a.home, sessionID)
	}
	return d
}

// readResolved reads resolved/<iid>.json.
func readResolved(home, sessionID, id string) (approvalResolved, bool) {
	var r approvalResolved
	err := readApprovalJSON(filepath.Join(approveDir(home, sessionID), "resolved", id+".json"), &r)
	return r, err == nil && r.InteractionID == id
}

// arbitrateLocked tries to record want as how the approval ended, create-new,
// and returns the record that stands: want when this write won, the file
// when another writer won first. false means neither is known. The file is
// tiny, so the write runs under a.mu: the outcome in memory and in the
// events must be the one on disk.
func (a *Attachment) arbitrateLocked(sessionID string, want approvalResolved) (approvalResolved, bool) {
	if writeResolved(a.home, sessionID, want) == nil {
		return want, true
	}
	return readResolved(a.home, sessionID, want.InteractionID)
}

// finalOutcomeLocked turns the winning resolved record into how the
// approval ended. A hook claim is the hook's exclusive right to answer, not
// proof that it answered: it is answered only with the hook's delivery
// record saying the deny was written whole. A record saying the write
// failed, or a hook gone with no record (it stopped before or after its
// write, or could not record it), is delivery_unknown: nobody can tell
// whether the deny reached Claude, and no terminal answer is claimed. While
// the claiming hook lives with no record, the outcome is not final; the
// run's end settles it (endApprovalsLocked). Caller holds a.mu.
func (a *Attachment) finalOutcomeLocked(sessionID string, ap *approval, r approvalResolved) (protocol.Resolution, bool) {
	if r.Outcome != outcomeHookClaim {
		return protocol.Resolution{InteractionID: r.InteractionID, Outcome: r.Outcome, Option: r.Option}, true
	}
	dead := hookDead(ap.hookPID) // before the delivery read: a live hook writes it before it exits
	unknown := protocol.Resolution{InteractionID: ap.id, Outcome: protocol.ResolutionDeliveryUnknown, Option: r.Option}
	if d, ok := readDelivery(a.home, sessionID, ap.id); ok {
		if d.Written {
			return protocol.Resolution{InteractionID: ap.id, Outcome: protocol.ResolutionAnswered, Option: r.Option}, true
		}
		return unknown, true
	}
	if dead {
		return unknown, true
	}
	return protocol.Resolution{}, false
}

// approvalIDsLocked lists the ids bound to any run and the open ones.
func (a *Attachment) approvalIDsLocked() (map[string]bool, []openRef) {
	known := map[string]bool{}
	var open []openRef
	for _, rec := range a.runs {
		for id := range rec.approvals {
			known[id] = true
		}
		for _, ap := range rec.open {
			open = append(open, openRef{id: ap.id, sessionID: ap.sessionID})
		}
	}
	return known, open
}

// runByPromptLocked returns the non-terminal run that owns promptID.
func (a *Attachment) runByPromptLocked(promptID string) *runRecord {
	if promptID == "" {
		return nil
	}
	for _, rec := range a.runs {
		if !rec.terminal && rec.promptID == promptID {
			return rec
		}
	}
	return nil
}

// applyToolEntryLocked records the tool_use and tool_result blocks of one
// transcript line against the run of the prompt the turn belongs to.
func (a *Attachment) applyToolEntryLocked(e transcriptEntry) {
	if !a.cfg.Approve {
		return
	}
	if e.PromptID != "" && len(e.ToolResults) > 0 {
		a.turnPrompt = e.PromptID
	}
	if len(e.ToolUses) > 0 && e.Type == "assistant" {
		if rec := a.runByPromptLocked(a.turnPrompt); rec != nil {
			for _, u := range e.ToolUses {
				rec.addCall(u)
			}
		}
	}
	for _, id := range e.ToolResults {
		rec := a.runByPromptLocked(e.PromptID)
		if rec == nil {
			rec = a.runByPromptLocked(a.turnPrompt)
		}
		if rec == nil {
			continue
		}
		for _, c := range rec.calls {
			if c.id == id && !c.resulted {
				c.resulted, c.resultTS = true, e.TS
			}
		}
	}
}

func (rec *runRecord) addCall(u toolUseRef) {
	for _, c := range rec.calls {
		if c.id == u.ID {
			return
		}
	}
	if len(rec.calls) >= maxRunToolCalls {
		for i, c := range rec.calls {
			if c.resulted && c.boundTo == "" {
				rec.calls = append(rec.calls[:i], rec.calls[i+1:]...)
				break
			}
		}
		if len(rec.calls) >= maxRunToolCalls {
			return
		}
	}
	rec.calls = append(rec.calls, &toolCall{id: u.ID, name: u.Name, hash: u.Hash})
}

// candidates are the calls an approval can be bound to: the same tool and
// input hash, not bound to another approval, and with no tool_result from
// before the approval opened.
func (rec *runRecord) candidates(ap *approval) []*toolCall {
	var out []*toolCall
	for _, c := range rec.calls {
		if c.name != ap.toolName || c.hash != ap.hash || (c.boundTo != "" && c.boundTo != ap.id) {
			continue
		}
		if c.resulted && c.resultTS > 0 && c.resultTS < ap.openedAt {
			continue
		}
		out = append(out, c)
	}
	return out
}

// applyApprovalsLocked applies one poll's approval files and the
// transcript's tool calls: new requests open on their run, a resolved file
// closes its approval, and the poller arbitrates when the terminal
// answered. Binding needs the whole transcript: caughtUp reports that this
// poll's read reached the end of the file, so no candidate call can still
// be unread. Caller holds a.mu.
func (a *Attachment) applyApprovalsLocked(sessionID string, d approvalDisk, caughtUp bool, seq uint64, events []core.NativeEvent) []core.NativeEvent {
	if !a.cfg.Approve {
		return events
	}
	ids := make([]string, 0, len(d.requests))
	for id := range d.requests {
		ids = append(ids, id)
	}
	// Oldest first, so the endpoint shows approvals in the order raised.
	sort.Slice(ids, func(i, j int) bool {
		oi, oj := d.requests[ids[i]].OpenedAt, d.requests[ids[j]].OpenedAt
		if oi != oj {
			return oi < oj
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		r := d.requests[id]
		rec := a.runByPromptLocked(r.PromptID)
		if rec == nil || rec.approvals[id] != nil {
			continue
		}
		deadline, derr := time.Parse(time.RFC3339Nano, r.Deadline)
		opened, oerr := time.Parse(time.RFC3339Nano, r.OpenedAt)
		if derr != nil || oerr != nil || r.ActionHash == "" {
			continue
		}
		preview, _ := protocol.TruncateText(r.Preview, protocol.MaxApprovalPreview)
		ap := &approval{id: id, toolName: r.ToolName, preview: preview, hash: r.ActionHash,
			hookPID: r.HookPID, deadline: deadline, openedAt: opened.UnixMilli(), seenSeq: seq, sessionID: r.SessionID,
			approvable: r.Approvable && preview == r.Preview, pinned: d.pinned}
		if rec.approvals == nil {
			rec.approvals = map[string]*approval{}
		}
		rec.approvals[id] = ap
		rec.open = append(rec.open, ap)
		if caughtUp && seq >= ap.seenSeq {
			a.bindCallLocked(rec, ap)
		}
		if len(rec.open) == 1 {
			events = append(events, rec.questionEvent(ap))
		}
	}
	for _, rec := range a.runs {
		for _, ap := range append([]*approval(nil), rec.open...) {
			if r, ok := d.resolved[ap.id]; ok {
				if res, final := a.finalOutcomeLocked(ap.sessionID, ap, r); final {
					events = rec.closeApproval(ap.id, res, events)
				}
				continue // a hook claim still writing its decision: wait
			}
			if caughtUp && seq >= ap.seenSeq {
				offered := ap.offersAllow()
				a.bindCallLocked(rec, ap)
				if ap.offersAllow() != offered && rec.open[0] == ap {
					events = append(events, rec.questionEvent(ap)) // bound late: allow is offered now
				}
			}
			if !a.terminalAnswered(rec, ap) && !hookDead(ap.hookPID) {
				continue
			}
			// The terminal decided, or the hook is gone. An answer file on
			// disk is not delivery: only the hook's own answered record,
			// if it won the create-new first, stands over this one.
			if r, ok := a.arbitrateLocked(ap.sessionID, approvalResolved{InteractionID: ap.id, Outcome: protocol.ResolutionElsewhere}); ok {
				if res, final := a.finalOutcomeLocked(ap.sessionID, ap, r); final {
					events = rec.closeApproval(ap.id, res, events)
				}
			}
		}
	}
	return events
}

// bindCallLocked binds an unbound, certain approval to its one candidate
// call, or marks it uncertain when two or more match.
func (a *Attachment) bindCallLocked(rec *runRecord, ap *approval) {
	if ap.toolUseID != "" || ap.uncertain {
		return
	}
	cands := rec.candidates(ap)
	switch {
	case len(cands) == 1:
		ap.toolUseID = cands[0].id
		cands[0].boundTo = ap.id
	case len(cands) > 1:
		ap.uncertain = true
	}
}

// terminalAnswered reports whether the call the approval stands for has a
// tool_result from after it opened: the terminal let it run or refused it.
func (a *Attachment) terminalAnswered(rec *runRecord, ap *approval) bool {
	for _, c := range rec.calls {
		switch {
		case ap.toolUseID != "":
			if c.id == ap.toolUseID && c.resulted {
				return true
			}
		case ap.uncertain:
			if c.boundTo == "" && c.name == ap.toolName && c.hash == ap.hash && c.resulted && (c.resultTS == 0 || c.resultTS >= ap.openedAt) {
				return true
			}
		}
	}
	return false
}

// hookDead reports whether the hook process is gone.
func hookDead(pid int) bool {
	alive, err := pidAlive(pid)
	return err == nil && !alive
}

// questionEvent publishes ap as the run's pending interaction.
func (rec *runRecord) questionEvent(ap *approval) core.NativeEvent {
	return core.NativeEvent{Type: core.EventQuestion, Key: rec.key, RunID: rec.msgID, Interaction: projectApproval(ap)}
}

// projectApproval is the endpoint's view of one open approval. It offers
// allow only when ap.offersAllow (bead 611.42.4); otherwise it is reject
// only, and the terminal allows.
func projectApproval(ap *approval) *protocol.Interaction {
	in := &protocol.Interaction{InteractionID: ap.id, Kind: "approval", Prompt: ap.preview, Options: []string{optionDeny},
		RemoteAnswer: true, RejectOption: optionDeny}
	if ap.offersAllow() {
		in.Options, in.ApproveOption = []string{optionAllow, optionDeny}, optionAllow
	}
	return in
}

// pendingApproval is the run's head approval, or nil.
func (rec *runRecord) pendingApproval() *protocol.Interaction {
	if len(rec.open) == 0 {
		return nil
	}
	return projectApproval(rec.open[0])
}

// closeApproval records how one open approval ended and, when it is the one
// the endpoint shows, publishes the resolution and the next one.
func (rec *runRecord) closeApproval(id string, res protocol.Resolution, events []core.NativeEvent) []core.NativeEvent {
	idx := -1
	for i, ap := range rec.open {
		if ap.id == id {
			idx = i
		}
	}
	if idx < 0 {
		return events
	}
	switch res.Outcome {
	case protocol.ResolutionAnswered, protocol.ResolutionElsewhere, protocol.ResolutionRunEnded, protocol.ResolutionDeliveryUnknown:
	default:
		res = protocol.Resolution{InteractionID: id, Outcome: protocol.ResolutionElsewhere}
	}
	rec.open = append(rec.open[:idx], rec.open[idx+1:]...)
	if rec.outcomes == nil {
		rec.outcomes = map[string]protocol.Resolution{}
	}
	rec.outcomes[id] = res
	if idx != 0 {
		return events // queued: the endpoint never showed it
	}
	events = append(events, core.NativeEvent{
		Type: core.EventQuestionResolved, Key: rec.key, RunID: rec.msgID,
		Interaction: &protocol.Interaction{InteractionID: id},
		Remote:      res.Outcome == protocol.ResolutionAnswered, Outcome: res.Outcome, Option: res.Option,
	})
	if len(rec.open) > 0 {
		events = append(events, rec.questionEvent(rec.open[0]))
	}
	return events
}

// endApprovalsLocked closes every open approval of a run that ended,
// queued ones first. Each tries run_ended as its resolved file, which also
// tells a lingering hook to stop, and closes with whatever outcome stands on
// disk. A hook's claim with no delivery record yet is settled now as
// delivery_unknown, by taking the delivery record itself: a record the hook
// writes later loses, so the files and the kept outcome agree. When nothing
// can be recorded the run's end still closes the approval as run_ended.
// Caller holds a.mu.
func (a *Attachment) endApprovalsLocked(rec *runRecord, events []core.NativeEvent) []core.NativeEvent {
	for len(rec.open) > 0 {
		ap := rec.open[len(rec.open)-1]
		res := protocol.Resolution{InteractionID: ap.id, Outcome: protocol.ResolutionRunEnded}
		if sid := ap.sessionID; sid != "" {
			if got, ok := a.arbitrateLocked(sid, approvalResolved{InteractionID: ap.id, Outcome: protocol.ResolutionRunEnded}); ok {
				final := false
				if res, final = a.finalOutcomeLocked(sid, ap, got); !final {
					_ = writeDelivery(a.home, sid, ap.id, false)
					res, final = a.finalOutcomeLocked(sid, ap, got)
					if !final {
						res = protocol.Resolution{InteractionID: ap.id, Outcome: protocol.ResolutionDeliveryUnknown, Option: got.Option}
					}
				}
			}
		}
		events = rec.closeApproval(ap.id, res, events)
	}
	return events
}

// Respond implements core.Attachment for DM approvals: a deny. An allow
// needs evidence, so it comes through RespondWithEvidence.
func (a *Attachment) Respond(key requests.Key, epoch, interactionID, option string) (protocol.Code, error) {
	return a.RespondWithEvidence(key, epoch, interactionID, option, nil)
}

// RespondWithEvidence implements core.EvidenceResponder. A deny is an answer
// on its own. An allow is an answer only for an approval that offers it,
// with evidence, which the answer file carries to the hook unread: the hook
// verifies it against its pinned owner and applies nothing else. An answer
// already on disk with the same identity is delivered. A fresh answer is
// written only for the run's head approval, while its hook is alive,
// before its deadline, and before anything resolved it. The hook applies
// the answer only to the call with the same action hash. The endpoint owns
// first-answer-wins.
func (a *Attachment) RespondWithEvidence(key requests.Key, _, interactionID, option string, evidence json.RawMessage) (protocol.Code, error) {
	switch {
	case option == optionDeny:
		evidence = nil
	case option == optionAllow && len(evidence) > 0 && len(evidence) <= maxEvidenceBytes && json.Valid(evidence):
	default:
		return protocol.CodeInvalid, nil
	}
	a.mu.Lock()
	rec := a.runs[key]
	var ap *approval
	if rec != nil {
		ap = rec.approvals[interactionID]
	}
	sessionID := ""
	if ap != nil {
		sessionID = ap.sessionID
	}
	if ap == nil || sessionID == "" || !interactionIDRe.MatchString(interactionID) {
		a.mu.Unlock()
		return protocol.CodeAlreadyResolved, nil
	}
	ans := approvalAnswer{InteractionID: interactionID, ActionHash: ap.hash, Option: option, At: protocol.FormatTime(a.now()), Evidence: evidence}
	head := len(rec.open) > 0 && rec.open[0] == ap
	hookPID, deadline, allowOffered := ap.hookPID, ap.deadline, ap.offersAllow()
	a.mu.Unlock()

	dir := approveDir(a.home, sessionID)
	if done, prior := answerOnDisk(filepath.Join(dir, "answers", interactionID+".json"), ans); prior {
		if done {
			return "", nil
		}
		return protocol.CodeAlreadyResolved, nil
	}
	switch {
	case option == optionAllow && !allowOffered:
		return protocol.CodeInvalid, nil
	case resolvedExists(filepath.Join(dir, "resolved", interactionID+".json")), !head, hookDead(hookPID):
		return protocol.CodeAlreadyResolved, nil
	case !a.now().Before(deadline):
		return protocol.CodeExpired, nil
	}
	adir, err := ensureApproveSubdir(a.home, sessionID, "answers")
	if err != nil {
		return "", err
	}
	if err := createNewJSON(adir, interactionID+".json", ans); err != nil {
		if !errors.Is(err, errFileExists) {
			return "", err
		}
		// Another writer published first: the same answer is delivered,
		// any other answer on disk stands as the first.
		if done, _ := answerOnDisk(filepath.Join(adir, interactionID+".json"), ans); done {
			return "", nil
		}
		return protocol.CodeAlreadyResolved, nil
	}
	return "", nil
}

// answerOnDisk compares the answer file with ans: prior reports a file is
// there, done that it holds the same answer.
func answerOnDisk(path string, ans approvalAnswer) (done, prior bool) {
	var got approvalAnswer
	err := readApprovalJSON(path, &got)
	if errors.Is(err, os.ErrNotExist) {
		return false, false
	}
	if err != nil {
		return false, true
	}
	return got.InteractionID == ans.InteractionID && got.ActionHash == ans.ActionHash && got.Option == ans.Option, true
}

// ResolvedInteraction implements core.InteractionResolver: how one approval
// of the key's run ended, as its resolved file recorded it.
func (a *Attachment) ResolvedInteraction(key requests.Key, _, interactionID string) (protocol.Resolution, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if rec, ok := a.runs[key]; ok {
		res, done := rec.outcomes[interactionID]
		return res, done
	}
	res, done := a.releasedOutcomes[key][interactionID]
	return res, done
}

// SetNow replaces the attachment's clock, which dates answers and decides
// approval expiry. For tests; production uses the wall clock.
func (a *Attachment) SetNow(now func() time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.clock = now
}

// removeApprovalFiles drops a released run's prompt marker and approval
// files.
func removeApprovalFiles(home, sessionID, promptID string, ids []string) {
	if !sessionIDRe.MatchString(sessionID) || !noFollowSupported {
		return
	}
	if promptID != "" {
		removeRunMarker(home, sessionID, promptID)
	}
	dir := approveDir(home, sessionID)
	for _, id := range ids {
		if !interactionIDRe.MatchString(id) {
			continue
		}
		for _, sub := range []string{"requests", "answers", "resolved", "delivery"} {
			removeRegular(filepath.Join(dir, sub, id+".json"))
		}
	}
}

// claimPromptLocked follows the prompt of a new turn and, when the turn is
// an AMQ run's own delivery, records the run's prompt id and queues its
// runs/<prompt_id> marker. Caller holds a.mu and has applied e.
func (a *Attachment) claimPromptLocked(e transcriptEntry, sessionID string) {
	a.turnPrompt = e.PromptID
	rec := a.runByMsgIDLocked(e.MsgID)
	if rec == nil || e.PromptID == "" || rec.promptID != "" {
		return
	}
	rec.promptID, rec.sessionID = e.PromptID, sessionID
	if !a.cfg.Approve {
		return
	}
	home, promptID, msgID := a.home, e.PromptID, rec.msgID
	a.pendingOps = append(a.pendingOps, func() { _ = writeRunMarker(home, sessionID, promptID, msgID) })
}

// queueRunEndLocked queues the removal of an ended run's prompt marker, so
// the hook decides nothing more for that prompt. Caller holds a.mu.
func (a *Attachment) queueRunEndLocked(rec *runRecord) {
	sessionID, home, promptID := rec.sessionID, a.home, rec.promptID
	if sessionID == "" || promptID == "" {
		return
	}
	a.pendingOps = append(a.pendingOps, func() { removeRunMarker(home, sessionID, promptID) })
}
