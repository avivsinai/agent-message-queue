package claude

import (
	"context"
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
// tool permission dialog. It raises a request for a tool call of a prompt
// that an AMQ run owns in a pinned session, and decides only where a relay
// share with approve is serving (an observe-only pin shows, never decides);
// everywhere else it exits at once with no output, so the normal dialog
// decides. It never exits 2 (not honored for this event). It prints deny
// for the owner's Buzz answer bound to this exact call. It prints allow
// only with proof (bead 611.42.4): its command line pins the owner's
// pubkey and the AMQ root whose manifest names the share, the call is
// shown whole, the answer carries the owner's signed approve reaction on
// that share's approval message whose content is the hook's own rendering
// of this call, and the hook's own read of the relay finds no edit that
// changed the call and no deletion. A missing pin, any failed check, an
// error, a cancellation or a timeout prints no allow.

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
	// allow is the pinned owner, share and verifier; without all three,
	// allow is impossible.
	allow AllowConfig
}

// reactionSkew is how much earlier than the request a reaction's signed
// date may be: the owner's phone clock can run behind this machine's.
const reactionSkew = 30 * time.Second

// RunPermissionHook is the hook body. done closes when the process gets
// TERM, INT or HUP: Claude sends TERM when the terminal rejects and at the
// configured timeout. allow is what an allow needs; a zero AllowConfig
// makes the hook reject-only. It always returns 0.
func RunPermissionHook(home string, stdin io.Reader, stdout, stderr io.Writer, done <-chan struct{}, wait time.Duration, allow AllowConfig) int {
	if wait <= 0 {
		wait = DefaultPermissionWait
	}
	h := permissionHook{home: home, now: time.Now, wait: wait, poll: 200 * time.Millisecond, markerGrace: 2 * time.Second, stderr: stderr, allow: allow}
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
	id, err := newInteractionID()
	if err != nil {
		return 0
	}
	preview, whole := approvalView(in.ToolName, in.ToolInput, in.AgentType)
	// Only observing pins: the request is raised so the run shows it, and
	// no answer file is ever applied. The terminal decides.
	answering := answeringPinLive(h.home, in.SessionID)
	approvable := false
	var share AllowShare
	if answering && whole && h.allow.usable() {
		var err error
		if share, err = h.allow.share(in.SessionID); err == nil {
			if p, ok := signedPrompt(preview, id, hash); ok {
				preview, approvable = p, true
			}
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
	// over reports that the hook may no longer decide: the terminal or the
	// endpoint closed the approval, its deadline passed, or Claude ended
	// the hook. It records how it ended when nothing else did.
	over := func() bool {
		if resolvedExists(resolvedPath) {
			return true
		}
		select {
		case <-done:
		default:
			if h.now().Before(deadline) {
				return false
			}
		}
		_ = writeResolved(h.home, in.SessionID, elsewhere)
		return true
	}
	for {
		if over() {
			return 0
		}
		var option string
		var evidence json.RawMessage
		if answering {
			option, evidence = h.answer(answerPath, req)
		}
		if option == optionAllow {
			want := AllowCheck{Share: share, Prompt: req.Preview, NotBefore: notBefore, NotAfter: deadline}
			if _, decided := hookVerdict(h.home, in.SessionID, id, evidence); decided {
				option = "" // decided and not applied: wait for a deny or a new proof
			} else if err := h.verifyAllow(evidence, want, done); err != nil {
				// Not applied and not consumed: no resolved record. The
				// refused verdict tells the endpoint why, and the rejected
				// record retires exactly this proof, so a ❌ or a new ✅
				// can still answer.
				option = ""
				_ = claimVerdict(h.home, in.SessionID, id, evidence, approvalVerdict{Verdict: verdictRefused, Reason: err.Error(), Altered: errors.Is(err, ErrAllowAltered)})
				_ = markRejected(h.home, in.SessionID, id, evidence, err.Error())
				if h.stderr != nil {
					_, _ = fmt.Fprintf(h.stderr, "amq-remote: ignored a Buzz allow for %s: %v\n", id, err)
				}
			} else if over() {
				return 0 // verified too late: never an allow after the end
			} else if err := claimVerdict(h.home, in.SessionID, id, evidence, approvalVerdict{Verdict: verdictAllow}); err != nil {
				option = "" // a verdict on this proof stands already, or none could be written
				if !errors.Is(err, errFileExists) && h.stderr != nil {
					_, _ = fmt.Fprintf(h.stderr, "amq-remote: ignored a Buzz allow for %s: %v\n", id, err)
				}
			} else if _, refused := rejectedRecord(h.home, in.SessionID, id, evidence); refused {
				// A refusal of this proof, forged or not, came before the
				// allow verdict: the endpoint may have dropped the intent
				// and taken a ❌ (611.42.6, Pro review of #986 r1). The
				// proof never applies.
				option = ""
			}
		}
		switch option {
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
// interaction and its action hash. A deny is applied as it is. An allow
// is a candidate only for an approvable call of a hook that can allow; it
// comes back with its evidence, unverified. Anything else is ignored.
func (h permissionHook) answer(path string, req approvalRequest) (string, json.RawMessage) {
	var a approvalAnswer
	if readApprovalJSON(path, &a) != nil {
		return "", nil
	}
	switch {
	case a.InteractionID != req.InteractionID || a.ActionHash != req.ActionHash:
		return answerIgnored, nil
	case a.Option == optionDeny:
		return optionDeny, nil
	case a.Option == optionAllow && req.Approvable && h.allow.usable():
		return optionAllow, a.Evidence
	}
	return answerIgnored, nil
}

// verifyAllow runs the verifier on evidence within allowVerifyTimeout,
// relay sign-in and history read included, so an unreachable relay never
// holds the hook while a ❌ waits; Claude ending the hook cancels it too.
func (h permissionHook) verifyAllow(evidence json.RawMessage, want AllowCheck, done <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), allowVerifyTimeout)
	defer cancel()
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return h.allow.Verify(ctx, evidence, want)
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
