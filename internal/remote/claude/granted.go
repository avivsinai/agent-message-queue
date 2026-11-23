package claude

import (
	"encoding/json"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// Claude Code shows a session's permission mode only in a hook's input, at a
// prompt. So a granted request travels as a stub (grantedStub) and its text
// stays in the endpoint, which gives it to AMQ's prompt hook at the prompt
// the stub raises (core.Endpoint.GrantedPrompt). The attachment keeps the
// latest prompt report of its session as the evidence that decides whether
// a stub is sent, and reports what the transcript shows of the hook's
// context; it judges nothing else.

// observedPrompt is one prompt report and when the endpoint observed it.
type observedPrompt struct {
	p  core.Prompt
	at time.Time
}

// grantedRun is what the reader knows of a run that a granted stub started.
type grantedRun struct {
	// granted: the run's frame is a granted stub.
	granted bool
	// stubSeen: the reader has applied the stub's prompt line.
	stubSeen bool
	// turnKnown: when the stub's line was read, the entries read held the
	// start of its turn (an assistant line, an interrupt line, or the start
	// of the transcript).
	turnKnown bool
	// shared: the stub's turn holds another prompt, before it or after it.
	// It never clears.
	shared bool
	// contextDigest is the sha256 of the text of the run's whole hook
	// context, as the transcript holds it.
	contextDigest string
}

// share records that the run's turn holds another prompt: its answer cannot
// be attributed to the run, for good.
func (g *grantedRun) share() {
	g.shared = true
	g.contextDigest = ""
}

// grantedDigest is the digest the run reports as GrantedTextDigest: its hook
// context's, when its stub was the only prompt of a turn whose start the
// reader saw; else empty.
func (g *grantedRun) grantedDigest() string {
	if !g.granted || !g.turnKnown || g.shared {
		return ""
	}
	return g.contextDigest
}

// turnState is what the reader knows of the turn its cursor is inside: the
// prompts read since the turn began, and whether the reader saw where it
// began.
type turnState struct {
	known   bool
	prompts int
	runs    []*runRecord
}

// isPrompt reports whether e is a prompt the harness gives the model: a
// user line with text that is not an interrupt line, or a prompt folded into
// a running turn.
func isPrompt(e transcriptEntry) bool {
	return e.Type == "user" && !e.Meta && e.Text != "" && (e.Absorbed || !strings.HasPrefix(e.Text, interruptPrefix))
}

// turnBoundary reports whether e ends the prompts of a turn: an assistant
// line, or the interrupt line of a turn the user stopped.
func turnBoundary(e transcriptEntry) bool {
	return e.Type == "assistant" || (e.Type == "user" && !e.Meta && !e.Absorbed && strings.HasPrefix(e.Text, interruptPrefix))
}

// trackTurnLocked applies e to what the reader knows of the current turn,
// before the turn state machine applies it. The prompt lines with no
// assistant line between them are one turn's prompts: a granted run whose
// turn holds another one is shared. A prompt folded into a turn while a run
// owns it joins that turn. The owner's whole hook context, read before any
// assistant line confirmed the run, gives the run its context digest. Caller
// holds a.mu.
func (a *Attachment) trackTurnLocked(e transcriptEntry) {
	switch {
	case turnBoundary(e):
		a.turn = turnState{known: true}
	case isPrompt(e):
		rec := a.runByMsgIDLocked(e.MsgID)
		if rec != nil && slices.Contains(a.turn.runs, rec) {
			return // the same stub read again
		}
		if rec != nil && rec.granted && !rec.stubSeen {
			rec.stubSeen = true
			rec.turnKnown = a.turn.known
			if e.Absorbed {
				// Folded into a turn already running: other prompts ran
				// before it.
				rec.share()
			}
		}
		if e.Absorbed && a.owner != nil && !a.owner.terminal && a.owner != rec {
			a.owner.share()
		}
		a.turn.prompts++
		if rec != nil {
			a.turn.runs = append(a.turn.runs, rec)
		}
		if a.turn.prompts > 1 {
			for _, r := range a.turn.runs {
				r.share()
			}
		}
	case len(e.HookContext) > 0:
		rec := a.owner
		if rec == nil || !rec.granted || rec.admitted || rec.terminal || rec.shared {
			return
		}
		for _, c := range e.HookContext {
			if text, ok := grantedContextText(c, rec.msgID); ok {
				rec.contextDigest = sha256Hex([]byte(text))
			}
		}
	}
}

// grantedPendingLocked reports whether a run waits for its granted stub's
// line, so a fresh cursor needs to know the turn it starts inside. Caller
// holds a.mu.
func (a *Attachment) grantedPendingLocked() bool {
	for _, rec := range a.runs {
		if rec.granted && !rec.terminal && !rec.stubSeen {
			return true
		}
	}
	return false
}

// turnBefore reads at most one chunk of the transcript at path before byte
// offset off and returns what it holds of the turn open at off: known when
// it holds the turn's start (an assistant line, an interrupt line, or the
// start of the transcript), and the number of that turn's prompts before
// off. A read that fails, or that starts inside the turn, is not known.
func turnBefore(path string, off int64) turnState {
	if off <= 0 {
		return turnState{known: true}
	}
	start := max(0, off-transcriptChunkBytes)
	rd, err := readTranscriptFrom(path, start, start > 0)
	if err != nil {
		return turnState{}
	}
	t := turnState{known: start == 0}
	for i, line := range rd.lines {
		if rd.starts[i] >= off {
			break
		}
		e, ok := parseTranscriptLine(line)
		switch {
		case !ok:
		case turnBoundary(e):
			t = turnState{known: true}
		case isPrompt(e):
			t.prompts++
		}
	}
	return t
}

// locator is the content of this attachment's sentinel file: where a
// session's prompt hook reports, or nothing for an attachment built with no
// state directory.
func (a *Attachment) locator() []byte {
	if a.stateDir == "" {
		return nil
	}
	raw, err := json.Marshal(promptLocator{StateDir: a.stateDir, Target: a.target})
	if err != nil {
		return nil
	}
	return append(raw, '\n')
}

// PromptInstance implements core.PromptHandover: the name of this
// attachment's sentinel file. Empty when the attachment publishes none.
func (a *Attachment) PromptInstance() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stateDir == "" {
		return ""
	}
	return a.boundToken
}

// GrantedStubID implements core.PromptHandover: the stub of key names it by
// the msg_id of its frame.
func (a *Attachment) GrantedStubID(key requests.Key) string { return frameMsgID(key) }

// promptVerdict is the one judgment of a prompt report: its session is the
// live registry session, and its mode asks the user before a tool acts.
func (a *Attachment) promptVerdict(p core.Prompt) (bool, string) {
	if sid := a.NativeSessionID(); sid == "" || p.NativeSession != sid {
		return false, "session_changed"
	}
	if p.Mode == "" {
		return false, "mode_unreadable"
	}
	return modeAsks(p.Mode), p.Mode
}

// PromptAsks implements core.PromptHandover with the values of p alone.
func (a *Attachment) PromptAsks(p core.Prompt) (bool, string) {
	if asks, why := a.promptVerdict(p); !asks {
		return false, why
	}
	if a.WritesWithoutAsking(a.stateDir) {
		return false, "state_in_workspace"
	}
	return true, p.Mode
}

// ObservePrompt implements core.PromptHandover: it keeps p as the session's
// latest report when p names the live session, and drops the kept report
// otherwise.
func (a *Attachment) ObservePrompt(p core.Prompt, at time.Time) {
	sid := a.NativeSessionID()
	a.mu.Lock()
	defer a.mu.Unlock()
	if sid != "" && p.NativeSession == sid {
		a.prompt = &observedPrompt{p: p, at: at}
		return
	}
	a.prompt = nil
}

// ForgetPrompt implements core.PromptHandover: it drops the kept report when
// it was observed before before, so a late call keeps a newer report.
func (a *Attachment) ForgetPrompt(before time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prompt != nil && a.prompt.at.Before(before) {
		a.prompt = nil
	}
}

// SessionAsks implements core.PermissionReader from the session's latest
// prompt report, judged as PromptAsks judges a report. With no report the
// mode is not known.
func (a *Attachment) SessionAsks() (bool, string) {
	a.mu.Lock()
	stored := a.prompt
	a.mu.Unlock()
	if stored == nil {
		return false, "mode_unreadable"
	}
	return a.promptVerdict(stored.p)
}

// WritesWithoutAsking implements core.PermissionReader: true when dir is not
// absolute, the registry gives no working directory, or the session's working
// directory overlaps dir, the AMQ home or AMQ's marker directory. Claude Code
// reports no additional working directory, so a directory the person added
// to the session is not seen here.
func (a *Attachment) WritesWithoutAsking(dir string) bool {
	if !filepath.IsAbs(dir) {
		return true
	}
	reg, err := readSessionRegistry(a.configDir, a.cfg.Pid)
	if err != nil || reg == nil || !filepath.IsAbs(reg.Cwd) {
		return true
	}
	for _, d := range []string{dir, amqhome.Under(a.home), markerDir(a.home)} {
		if core.PathsOverlap(reg.Cwd, d) {
			return true
		}
	}
	return false
}

// GrantTextLimit implements core.GrantTextLimiter.
func (a *Attachment) GrantTextLimit() int { return GrantedTextLimit }

// maxPromptHookInputBytes bounds the prompt hook's stdin. A prompt longer
// than this is no stub, and its report is not needed.
const maxPromptHookInputBytes = 4 << 20

// PromptHookInput is what the prompt hook reads from its input.
type PromptHookInput struct {
	SessionID string
	Mode      string
	// StubID is the message id of the granted stub the prompt is, or "".
	StubID string
}

// ParsePromptHookInput reads a UserPromptSubmit hook input from r, bounded.
// ok is false for another event, input that does not decode, or a session
// id that is not file-safe.
func ParsePromptHookInput(r io.Reader) (PromptHookInput, bool) {
	raw, err := io.ReadAll(io.LimitReader(r, maxPromptHookInputBytes))
	if err != nil {
		return PromptHookInput{}, false
	}
	var in struct {
		SessionID      string `json:"session_id"`
		PermissionMode string `json:"permission_mode"`
		Prompt         string `json:"prompt"`
		HookEventName  string `json:"hook_event_name"`
	}
	if json.Unmarshal(raw, &in) != nil || in.HookEventName != "UserPromptSubmit" || !sessionIDRe.MatchString(in.SessionID) {
		return PromptHookInput{}, false
	}
	id, _ := StubMsgID(in.Prompt)
	return PromptHookInput{SessionID: in.SessionID, Mode: in.PermissionMode, StubID: id}, true
}

// PromptHookOutput is the hook output that gives the model the text of the
// granted request msgID as added context.
func PromptHookOutput(msgID, text string) []byte {
	out, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "UserPromptSubmit",
		"additionalContext": GrantedContext(msgID, text),
	}})
	if err != nil {
		return nil
	}
	return append(out, '\n')
}
