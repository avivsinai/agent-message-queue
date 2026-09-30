package claude

import (
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
// belongs to, shows the oldest open one to the endpoint, and closes it from
// the one file that decides how it ended: resolved/<iid>.json. The poller
// writes that file itself only when the terminal decided (the call's
// tool_result appeared, or the hook died), when the run ended, and never
// when a Buzz answer is still with a live hook: the hook's own answered
// record wins, since every resolved file is create-new.

// approval is one PermissionRequest bound to a run.
type approval struct {
	id         string
	toolName   string
	preview    string
	hash       string
	approvable bool
	hookPID    int
	deadline   time.Time
	// openedAt is when the hook raised it (unix ms): a tool_result stamped
	// earlier belongs to an earlier call.
	openedAt int64
	// toolUseID is the one call this approval is bound to; uncertain means
	// two or more calls matched, so approve is not offered and the first
	// tool_result among them closes it.
	toolUseID string
	uncertain bool
}

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
	answered map[string]bool
}

// readApprovalDisk reads every request not yet known and the resolved and
// answer state of the open approvals. known holds the ids already bound.
func (a *Attachment) readApprovalDisk(sessionID string, known map[string]bool, open []string) approvalDisk {
	d := approvalDisk{requests: map[string]approvalRequest{}, resolved: map[string]approvalResolved{}, answered: map[string]bool{}}
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
		}
	}
	for _, id := range open {
		var r approvalResolved
		if err := readApprovalJSON(filepath.Join(dir, "resolved", id+".json"), &r); err == nil && r.InteractionID == id {
			d.resolved[id] = r
		}
		if _, err := os.Lstat(filepath.Join(dir, "answers", id+".json")); err == nil {
			d.answered[id] = true
		}
	}
	return d
}

// approvalIDsLocked lists the ids bound to any run and the open ones.
func (a *Attachment) approvalIDsLocked() (map[string]bool, []string) {
	known := map[string]bool{}
	var open []string
	for _, rec := range a.runs {
		for id := range rec.approvals {
			known[id] = true
		}
		for _, ap := range rec.open {
			open = append(open, ap.id)
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
// closes its approval, and the poller decides when the terminal answered.
// It returns the events to emit and the resolved files to write outside
// a.mu. Caller holds a.mu.
func (a *Attachment) applyApprovalsLocked(sessionID string, d approvalDisk, events []core.NativeEvent) ([]core.NativeEvent, []approvalResolved) {
	var writes []approvalResolved
	if !a.cfg.Approve {
		return events, nil
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
		ap := &approval{id: id, toolName: r.ToolName, preview: preview, hash: r.ActionHash, approvable: r.Approvable,
			hookPID: r.HookPID, deadline: deadline, openedAt: opened.UnixMilli()}
		if rec.approvals == nil {
			rec.approvals = map[string]*approval{}
		}
		rec.approvals[id] = ap
		rec.open = append(rec.open, ap)
		a.bindCallLocked(rec, ap)
		if len(rec.open) == 1 {
			events = append(events, rec.questionEvent(ap))
		}
	}
	for _, rec := range a.runs {
		for _, ap := range append([]*approval(nil), rec.open...) {
			if r, ok := d.resolved[ap.id]; ok {
				events = rec.closeApproval(ap.id, protocol.Resolution{InteractionID: ap.id, Outcome: r.Outcome, Option: r.Option}, events)
				continue
			}
			was := ap.uncertain
			a.bindCallLocked(rec, ap)
			if ap.uncertain && !was && len(rec.open) > 0 && rec.open[0] == ap {
				events = append(events, rec.questionEvent(ap)) // approve withdrawn
			}
			if !a.terminalAnswered(rec, ap) && !hookDead(ap.hookPID) {
				continue
			}
			if d.answered[ap.id] && !hookDead(ap.hookPID) {
				// A Buzz answer is with the live hook: its answered record,
				// written after it prints, decides.
				continue
			}
			writes = append(writes, approvalResolved{InteractionID: ap.id, Outcome: protocol.ResolutionElsewhere})
		}
	}
	return events, writes
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

// projectApproval is the endpoint's view of one open approval. Reject is
// always offered; approve only for an approvable call bound to exactly one
// tool call candidate or not yet matched.
func projectApproval(ap *approval) *protocol.Interaction {
	in := &protocol.Interaction{InteractionID: ap.id, Kind: "approval", Prompt: ap.preview, Options: []string{optionDeny},
		RemoteAnswer: true, RejectOption: optionDeny}
	if ap.approvable && !ap.uncertain {
		in.ApproveOption = optionAllow
		in.Options = []string{optionAllow, optionDeny}
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
	case protocol.ResolutionAnswered, protocol.ResolutionElsewhere, protocol.ResolutionRunEnded:
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

// endApprovals closes every open approval as run_ended, queued ones first,
// and returns the resolved files that tell a lingering hook to stop.
func (rec *runRecord) endApprovals(events []core.NativeEvent) ([]core.NativeEvent, []approvalResolved) {
	var writes []approvalResolved
	for len(rec.open) > 0 {
		id := rec.open[len(rec.open)-1].id
		events = rec.closeApproval(id, protocol.Resolution{InteractionID: id, Outcome: protocol.ResolutionRunEnded}, events)
		writes = append(writes, approvalResolved{InteractionID: id, Outcome: protocol.ResolutionRunEnded})
	}
	return events, writes
}

// Respond implements core.Attachment for DM approvals. An answer already on
// disk with the same identity is delivered. A fresh answer is written only
// for the run's head approval, with an option it offers, while its hook is
// alive, before its deadline, and before anything resolved it. The hook
// applies the answer only to the call with the same action hash. The
// endpoint owns first-answer-wins.
func (a *Attachment) Respond(key requests.Key, _, interactionID, option string) (protocol.Code, error) {
	a.mu.Lock()
	rec := a.runs[key]
	var ap *approval
	if rec != nil {
		ap = rec.approvals[interactionID]
	}
	sessionID := a.boundSession
	if ap == nil || sessionID == "" || !interactionIDRe.MatchString(interactionID) {
		a.mu.Unlock()
		return protocol.CodeAlreadyResolved, nil
	}
	ans := approvalAnswer{InteractionID: interactionID, ActionHash: ap.hash, Option: option, At: protocol.FormatTime(a.now())}
	head := len(rec.open) > 0 && rec.open[0] == ap
	offered := false
	for _, o := range projectApproval(ap).Options {
		offered = offered || o == option
	}
	hookPID, deadline := ap.hookPID, ap.deadline
	a.mu.Unlock()

	dir := approveDir(a.home, sessionID)
	if done, prior := answerOnDisk(filepath.Join(dir, "answers", interactionID+".json"), ans); prior {
		if done {
			return "", nil
		}
		return protocol.CodeAlreadyResolved, nil
	}
	switch {
	case resolvedExists(filepath.Join(dir, "resolved", interactionID+".json")), !head, hookDead(hookPID):
		return protocol.CodeAlreadyResolved, nil
	case !a.now().Before(deadline):
		return protocol.CodeExpired, nil
	case !offered:
		return protocol.CodeInvalid, nil
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
		for _, sub := range []string{"requests", "answers", "resolved"} {
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
	rec.promptID = e.PromptID
	if !a.cfg.Approve {
		return
	}
	home, promptID, msgID := a.home, e.PromptID, rec.msgID
	a.pendingOps = append(a.pendingOps, func() { _ = writeRunMarker(home, sessionID, promptID, msgID) })
}

// queueRunEndLocked queues the files a run's end writes: a run_ended record
// for each approval still open, so a lingering hook stops, and the removal
// of its prompt marker. Caller holds a.mu.
func (a *Attachment) queueRunEndLocked(rec *runRecord, writes []approvalResolved) {
	sessionID, home, promptID := a.boundSession, a.home, rec.promptID
	if sessionID == "" || (promptID == "" && len(writes) == 0) {
		return
	}
	a.pendingOps = append(a.pendingOps, func() {
		for _, w := range writes {
			_ = writeResolved(home, sessionID, w)
		}
		if promptID != "" {
			removeRunMarker(home, sessionID, promptID)
		}
	})
}
