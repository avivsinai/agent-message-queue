package claude

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// PermissionRequest hook (bead 611.42.3). Claude Code runs it for every
// tool permission dialog. It decides only for a tool call of a prompt that
// an AMQ run owns in a session that a relay share with approve is serving;
// everywhere else it exits at once with no output, so the normal dialog
// decides. It never exits 2 (not honored for this event), and it never
// allows by default: a decision is printed only for a Buzz answer bound to
// this exact call.

// permissionHookMarker is the settings.json ownership signal of the
// PermissionRequest entry, distinct from the Stop hook's.
const permissionHookMarker = "AMQ_APPROVAL_HOOK=1 "

// permissionHookType is the hook event name in settings.json.
const permissionHookType = "PermissionRequest"

// DefaultPermissionWait is the hook's own deadline. The installed timeout is
// this plus PermissionHookGrace, so the hook normally ends first.
const (
	DefaultPermissionWait = 600 * time.Second
	PermissionHookGrace   = 30 * time.Second
)

// permissionInput is the subset of the PermissionRequest stdin the hook
// reads.
type permissionInput struct {
	SessionID     string          `json:"session_id"`
	PromptID      string          `json:"prompt_id"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	AgentType     string          `json:"agent_type"`
}

// permissionHook holds the hook's clock and pacing, so tests can drive it.
type permissionHook struct {
	home string
	now  func() time.Time
	wait time.Duration
	// poll is how often the hook reads answers/ and resolved/.
	poll time.Duration
	// markerGrace is how long a pinned session waits for runs/<prompt_id>:
	// the attachment writes it when its poller sees the delivery line, which
	// can trail the first tool call by a tick.
	markerGrace time.Duration
	// ticks paces the poll when set; nil uses a ticker of poll.
	ticks <-chan time.Time
}

// RunPermissionHook is the hook body. done closes when the process gets
// TERM, INT or HUP: Claude sends TERM when the terminal rejects and at the
// configured timeout. It always returns 0.
func RunPermissionHook(home string, stdin io.Reader, stdout io.Writer, done <-chan struct{}, wait time.Duration) int {
	if wait <= 0 {
		wait = DefaultPermissionWait
	}
	h := permissionHook{home: home, now: time.Now, wait: wait, poll: 200 * time.Millisecond, markerGrace: 2 * time.Second}
	return h.run(stdin, stdout, done)
}

func (h permissionHook) run(stdin io.Reader, stdout io.Writer, done <-chan struct{}) int {
	defer func() { _ = recover() }() // no decision, unconditionally
	if !noFollowSupported {
		return 0
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return 0
	}
	var in permissionInput
	if json.Unmarshal(raw, &in) != nil {
		return 0
	}
	if in.HookEventName != "" && in.HookEventName != permissionHookType {
		return 0
	}
	if !sessionIDRe.MatchString(in.SessionID) || !pinLive(h.home, in.SessionID) {
		return 0
	}
	if in.PromptID == "" || !promptIDRe.MatchString(in.PromptID) || !h.awaitRunMarker(in.SessionID, in.PromptID, done) {
		return 0
	}
	hash, ok := actionHash(in.ToolName, in.ToolInput)
	if !ok {
		return 0
	}
	preview, approvable := approvalPreview(in.ToolName, in.ToolInput, in.AgentType)
	id, err := newInteractionID()
	if err != nil {
		return 0
	}
	opened := h.now()
	deadline := opened.Add(h.wait)
	req := approvalRequest{
		Protocol: approvalProtocol, InteractionID: id, SessionID: in.SessionID, PromptID: in.PromptID,
		ToolName: in.ToolName, Preview: preview, Approvable: approvable, ActionHash: hash,
		HookPID: os.Getpid(), OpenedAt: protocol.FormatTime(opened), Deadline: protocol.FormatTime(deadline),
	}
	dir, err := ensureApproveSubdir(h.home, in.SessionID, "requests")
	if err != nil || createNewJSON(dir, id+".json", req) != nil {
		return 0
	}
	elsewhere := approvalResolved{InteractionID: id, Outcome: protocol.ResolutionElsewhere}
	answerPath := filepath.Join(approveDir(h.home, in.SessionID), "answers", id+".json")
	resolvedPath := filepath.Join(approveDir(h.home, in.SessionID), "resolved", id+".json")
	ticks := h.ticks
	if ticks == nil {
		t := time.NewTicker(h.poll)
		defer t.Stop()
		ticks = t.C
	}
	for {
		if option, ok := h.answer(answerPath, req); ok {
			// The first create-new of the resolved file decides. The hook
			// prints only after it won that with answered; when the
			// terminal's closure won first, it stays silent.
			if writeResolved(h.home, in.SessionID, approvalResolved{InteractionID: id, Outcome: protocol.ResolutionAnswered, Option: option}) == nil {
				writeDecision(stdout, option)
			}
			return 0
		}
		if resolvedExists(resolvedPath) {
			return 0 // closed elsewhere: the terminal decides
		}
		if !h.now().Before(deadline) {
			_ = writeResolved(h.home, in.SessionID, elsewhere)
			return 0
		}
		select {
		case <-done:
			_ = writeResolved(h.home, in.SessionID, elsewhere)
			return 0
		case <-ticks:
		}
	}
}

// awaitRunMarker reports whether runs/<prompt_id> exists, waiting at most
// markerGrace for it.
func (h permissionHook) awaitRunMarker(sessionID, promptID string, done <-chan struct{}) bool {
	until := h.now().Add(h.markerGrace)
	for {
		if runMarked(h.home, sessionID, promptID) {
			return true
		}
		if !h.now().Before(until) {
			return false
		}
		select {
		case <-done:
			return false
		case <-time.After(h.poll / 2):
		}
	}
}

// answer returns the Buzz answer for this exact call, if one is on disk: it
// must name the interaction and its action hash, and allow counts only for
// an approvable call.
func (h permissionHook) answer(path string, req approvalRequest) (string, bool) {
	var a approvalAnswer
	if readApprovalJSON(path, &a) != nil {
		return "", false
	}
	if a.InteractionID != req.InteractionID || a.ActionHash != req.ActionHash {
		return "", false
	}
	switch {
	case a.Option == optionDeny:
		return optionDeny, true
	case a.Option == optionAllow && req.Approvable:
		return optionAllow, true
	}
	return "", false
}

func resolvedExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

// writeDecision prints the one PermissionRequest decision. Allow is allow
// once; deny carries a message and no interrupt, so the turn continues.
func writeDecision(stdout io.Writer, option string) {
	type decision struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message,omitempty"`
	}
	d := decision{Behavior: "allow"}
	if option == optionDeny {
		d = decision{Behavior: "deny", Message: "Rejected from Buzz by the owner."}
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName string   `json:"hookEventName"`
			Decision      decision `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = permissionHookType
	out.HookSpecificOutput.Decision = d
	raw, err := json.Marshal(out)
	if err != nil {
		return
	}
	_, _ = stdout.Write(append(raw, '\n'))
}
