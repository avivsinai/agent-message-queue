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
// began and every entry since.
type turnState struct {
	known   bool
	prompts int
	runs    []*runRecord
}

// turnEntry is what one transcript line is to the turn reader.
type turnEntry int

const (
	// entryOther changes nothing the reader knows of the turn.
	entryOther turnEntry = iota
	// entryBoundary ends the prompts of a turn: an assistant line, or the
	// interrupt line of a turn the user stopped.
	entryBoundary
	// entryPrompt is input to the model from the user side, whatever its
	// content (text, an image, a document, an empty text), or a prompt
	// folded into a running turn. A tool result is not a prompt.
	entryPrompt
	// entryContext is the context a UserPromptSubmit hook added.
	entryContext
	// entryUnknown is a gap in the evidence: a line that did not parse, a
	// user line whose content cannot be read, or a line of a type the reader
	// does not know that carries a message or an attachment.
	entryUnknown
)

// classifyTurnEntry is the one judgment of a transcript line for the turn
// reader, live and in recovery's read back. parsed is false for a line that
// did not parse. An attachment that is not a folded prompt or a hook's
// context is context the harness adds, not input from the user side; an
// api-request-blob copies a request whose entries the transcript holds.
func classifyTurnEntry(e transcriptEntry, parsed bool) turnEntry {
	switch {
	case !parsed:
		return entryUnknown
	case e.Type == "assistant":
		return entryBoundary
	case e.Type == "user" && e.Absorbed:
		return entryPrompt
	case e.Type == "user" && e.Meta:
		return entryOther
	case e.Type == "user" && strings.HasPrefix(e.Text, interruptPrefix):
		return entryBoundary
	case e.Type == "user" && e.Content == contentToolResults:
		return entryOther
	case e.Type == "user" && e.Content == contentNone:
		return entryUnknown
	case e.Type == "user":
		return entryPrompt
	case len(e.HookContext) > 0:
		return entryContext
	case e.Type == "attachment" || e.Type == "api-request-blob" || !e.CarriesInput:
		return entryOther
	}
	return entryUnknown
}

// trackTurnLocked applies e, of kind k, to what the reader knows of the
// current turn, before the turn state machine applies it. The prompt lines
// with no assistant line between them are one turn's prompts: a granted run
// whose turn holds another one is shared. A prompt folded into a turn while
// a run owns it joins that turn. The owner's whole hook context, read before
// any assistant line confirmed the run, gives the run its context digest. A
// gap in the evidence shares every run of the turn and the owner, and leaves
// the turn not known until the next boundary. Caller holds a.mu.
func (a *Attachment) trackTurnLocked(e transcriptEntry, k turnEntry) {
	switch k {
	case entryBoundary:
		a.turn = turnState{known: true}
	case entryUnknown:
		a.turn.known = false
		for _, r := range a.turn.runs {
			r.share()
		}
		if a.owner != nil && !a.owner.terminal {
			a.owner.share()
		}
	case entryPrompt:
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
	case entryContext:
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
// start of the transcript) and no gap after it (classifyTurnEntry), and the
// number of that turn's prompts before off. A read that fails, or that
// starts inside the turn, is not known.
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
		switch classifyTurnEntry(e, ok) {
		case entryBoundary:
			t = turnState{known: true}
		case entryUnknown:
			t.known = false
		case entryPrompt:
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

// needsSignature starts the refusal of a granted request that this session
// takes only with a signature now.
const needsSignature = "grant_needs_signature: "

// stateInWorkspaceRefusal refuses a granted request of a session that can
// write the endpoint's state with no dialog; signing one task does not help.
const stateInWorkspaceRefusal = "grant_unsupported: state_in_workspace: the session can write this machine's state with no dialog"

// PromptRefusal implements core.PromptHandover with the values of p alone.
// It is the one judgment of a prompt report, before a stub is sent and at
// the hand-over of its text: "" when the report's session is the live
// registry session, its mode asks the user before a tool acts, and the
// session cannot write the endpoint's state with no dialog; else the whole
// message of the refusal.
func (a *Attachment) PromptRefusal(p core.Prompt) string {
	switch sid := a.NativeSessionID(); {
	case sid == "" || p.NativeSession != sid:
		return needsSignature + "session_changed"
	case p.Mode == "":
		return needsSignature + "mode_unreadable"
	case !modeAsks(p.Mode):
		return needsSignature + p.Mode
	case a.WritesWithoutAsking(a.stateDir):
		return stateInWorkspaceRefusal
	}
	return ""
}

// storedRefusal is PromptRefusal of the session's latest prompt report, with
// that report's mode. With no report the mode is not known.
func (a *Attachment) storedRefusal() (msg, mode string) {
	a.mu.Lock()
	stored := a.prompt
	a.mu.Unlock()
	if stored == nil {
		return needsSignature + "mode_unreadable", ""
	}
	return a.PromptRefusal(stored.p), stored.p.Mode
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

// SessionAsks implements core.PermissionReader from storedRefusal. Whether
// the session can write a directory is WritesWithoutAsking's question, so
// only a refusal that asks for a signature makes asks false.
func (a *Attachment) SessionAsks() (bool, string) {
	msg, mode := a.storedRefusal()
	if why, ok := strings.CutPrefix(msg, needsSignature); ok {
		return false, why
	}
	return true, mode
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
