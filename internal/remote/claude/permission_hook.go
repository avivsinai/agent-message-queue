package claude

import (
	"encoding/json"
	"errors"
	"fmt"
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
// decides. It never exits 2 (not honored for this event). It prints deny
// for the owner's Buzz answer bound to this exact call. It prints allow
// only with proof (bead 611.42.4): its command line pins the owner's
// pubkey (--owner), the call is shown whole, and the answer carries the
// owner's signed approve reaction on the approval message whose content is
// the hook's own rendering of this call. A missing pin, any failed check,
// an error or a timeout prints no allow.

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
	// stderr receives the one note about an answer the hook ignored.
	stderr io.Writer
	// owner is the pinned owner pubkey from the hook's command line, and
	// verify checks an allow's evidence against it. Without both, allow is
	// impossible.
	owner  string
	verify AllowVerifier
}

// reactionSkew is how much earlier than the request a reaction's signed
// date may be: the owner's phone clock can run behind this machine's.
const reactionSkew = 30 * time.Second

// RunPermissionHook is the hook body. done closes when the process gets
// TERM, INT or HUP: Claude sends TERM when the terminal rejects and at the
// configured timeout. owner is the pinned owner pubkey ("" for none) and
// verify checks an allow's evidence; allow is impossible unless both are
// set and owner is 64 lowercase hex. It always returns 0.
func RunPermissionHook(home string, stdin io.Reader, stdout, stderr io.Writer, done <-chan struct{}, wait time.Duration, owner string, verify AllowVerifier) int {
	if wait <= 0 {
		wait = DefaultPermissionWait
	}
	h := permissionHook{home: home, now: time.Now, wait: wait, poll: 200 * time.Millisecond, markerGrace: 2 * time.Second, stderr: stderr, owner: owner, verify: verify}
	return h.run(stdin, stdout, done)
}

// canAllow reports whether this hook can ever apply an allow.
func (h permissionHook) canAllow() bool {
	return ValidOwner(h.owner) && h.verify != nil
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
	id, err := newInteractionID()
	if err != nil {
		return 0
	}
	preview, whole := approvalView(in.ToolName, in.ToolInput, in.AgentType)
	approvable := false
	if whole && h.canAllow() {
		if p, ok := signedPrompt(preview, id, hash); ok {
			preview, approvable = p, true
		}
	}
	opened := h.now()
	deadline := opened.Add(h.wait)
	req := approvalRequest{
		Protocol: approvalProtocol, InteractionID: id, SessionID: in.SessionID, PromptID: in.PromptID,
		ToolName: in.ToolName, Preview: preview, ActionHash: hash, Approvable: approvable,
		HookPID: os.Getpid(), OpenedAt: protocol.FormatTime(opened), Deadline: protocol.FormatTime(deadline),
	}
	// The window an allow's reaction must be dated in. req is the hook's
	// own memory; nothing read from disk changes what it allows.
	notBefore := opened.Add(-reactionSkew)
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
	ignored := false
	for {
		switch option := h.answer(answerPath, req, notBefore, deadline); option {
		case optionDeny, optionAllow:
			// The first create-new of the resolved file decides, and it
			// is the single use of an allow. The hook prints only after it
			// won that claim; when the terminal's closure won first, it
			// stays silent. The claim becomes an answer only with the
			// delivery record of a whole write.
			if writeResolved(h.home, in.SessionID, approvalResolved{InteractionID: id, Outcome: outcomeHookClaim, Option: option}) == nil {
				err := writeDecision(stdout, option)
				_ = writeDelivery(h.home, in.SessionID, id, err == nil)
			}
			return 0
		case answerIgnored:
			if !ignored && h.stderr != nil {
				_, _ = fmt.Fprintf(h.stderr, "amq-remote: ignored a Buzz answer for %s: not a deny, and not an allow proven by the pinned owner's signed reaction\n", id)
			}
			ignored = true
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

// answerIgnored is an answer file the hook does not apply; "" is none.
const answerIgnored = "ignored"

// answer reads the Buzz answer for this exact call: it must name the
// interaction and its action hash. A deny is applied as it is. An allow is
// applied only for an approvable call of a hook that pins an owner, and
// only when its evidence verifies against that owner, this call's signed
// prompt and the window [notBefore, deadline]. Anything else is ignored.
func (h permissionHook) answer(path string, req approvalRequest, notBefore, deadline time.Time) string {
	var a approvalAnswer
	if readApprovalJSON(path, &a) != nil {
		return ""
	}
	if a.InteractionID != req.InteractionID || a.ActionHash != req.ActionHash {
		return answerIgnored
	}
	switch {
	case a.Option == optionDeny:
		return optionDeny
	case a.Option == optionAllow && req.Approvable && h.canAllow() &&
		h.verify(a.Evidence, h.owner, req.Preview, notBefore, deadline) == nil:
		return optionAllow
	}
	return answerIgnored
}

func resolvedExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

// writeDecision prints the hook's decision: deny with a message and no
// interrupt, so the turn continues, or allow with the call's own input. It
// reports whether the whole decision was written and flushed.
func writeDecision(stdout io.Writer, option string) error {
	type decision struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message,omitempty"`
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName string   `json:"hookEventName"`
			Decision      decision `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = permissionHookType
	out.HookSpecificOutput.Decision = decision{Behavior: "deny", Message: "Rejected from Buzz by the owner."}
	if option == optionAllow {
		out.HookSpecificOutput.Decision = decision{Behavior: "allow"}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	n, err := stdout.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if f, ok := stdout.(interface{ Flush() error }); ok && err == nil {
		err = f.Flush()
	}
	return err
}
