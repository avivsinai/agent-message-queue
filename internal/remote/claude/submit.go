package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

// submitTimeout bounds the socket dial+write: the receiver is
// fire-and-forget (capture §4 — recv stays empty until close), so the
// write returns immediately after the kernel buffers it; anything longer
// is a stuck target and the submit is refused as unsent rather than held.
const submitTimeout = 6 * time.Second

// ErrNotSent marks a pre-send failure: the frame never left this process,
// so a retry is safe and the refusal is definitive (mirrors codex B1).
var ErrNotSent = errors.New("claude submit: frame not sent")

// ErrNoInbound is the typed refusal for the documented inbound
// precondition: the target session must accept cross-session messages
// (the receiver-side `Sr.admit` guard must be enabled and the socket
// present). A target without the socket cannot receive frames, and the
// adapter says so instead of guessing.
var ErrNoInbound = errors.New("claude submit: target session does not expose a messaging socket (inbound cross-session messages disabled or unsupported)")

// runRecord is the adapter's retained correlation for one submitted key.
// The evidence ladder (architect ruling 10:59Z): a socket write alone is
// TENTATIVE; the transcript user line carrying the envelope is SUBMITTED;
// the assistant turn starting (transcript assistant line or Stop-hook
// post) is ADMITTED.
type runRecord struct {
	key   requests.Key
	epoch string
	msgID string
	state protocol.State
	// submitted: the transcript user line was observed (harness accepted
	// the payload into the transcript). Still not native ownership.
	submitted bool
	// admitted: the assistant turn started — native ownership proven.
	admitted bool
	// terminal: the turn ended (Stop hook or transcript evidence).
	terminal bool
	result   *protocol.Result
	// bodyMark is the unique body fragment correlated in the transcript.
	bodyMark  string
	createdAt time.Time
}

// Submit implements core.Attachment over the pinned 611.2 wire: the
// mandatory auth line + one newline-JSON user frame carrying the
// cross-session envelope (from-mode set — unset parity gating would park
// us). The socket send is fire-and-forget: success is only ever evidence
// TENTATIVE, upgraded by transcript observation, never by the socket.
// No child process runs on the request path — the send is one UDS write.
func (a *Attachment) Submit(req core.BoundRequest) (core.Admission, error) {
	a.mu.Lock()
	// The claude adapter pins no epoch of its own (SentinelUnpinned): any
	// epoch the endpoint holds is current, and ownership is proven by
	// transcript observation, not epoch pinning.
	if a.cancelIntent[req.Key] {
		delete(a.cancelIntent, req.Key)
		a.mu.Unlock()
		return core.Admission{Code: protocol.CodeCancelledBeforeAdmission}, nil
	}
	if existing, ok := a.runs[req.Key]; ok {
		a.mu.Unlock()
		return core.Admission{Admitted: true, RunID: existing.msgID}, nil
	}
	a.mu.Unlock()

	reg, err := readSessionRegistry(a.home, a.cfg.Pid)
	if err != nil {
		return refusal(fmt.Errorf("%w: %v", ErrNotSent, err)), nil
	}
	if reg == nil || reg.MessagingSocketPath == "" {
		return refusal(ErrNoInbound), nil
	}
	token, err := readPeerToken(a.home, a.cfg.Pid, reg.MessagingSocketPath)
	if err != nil {
		// The key file is minted by the target when inbound is enabled;
		// its absence is the same precondition failure.
		if errors.Is(err, os.ErrNotExist) {
			return refusal(ErrNoInbound), nil
		}
		return refusal(fmt.Errorf("%w: %v", ErrNotSent, err)), nil
	}

	text := req.Input.Text
	if strings.Contains(text, "</cross-session-message>") || strings.ContainsAny(text, "\x00") {
		return refusal(fmt.Errorf("%w: submit text contains an envelope terminator", ErrNotSent)), nil
	}
	frame, err := BuildFrame("amq-"+sanitizeAddr(a.target), text)
	if err != nil {
		return refusal(fmt.Errorf("%w: %v", ErrNotSent, err)), nil
	}
	wire, err := EncodeFrames(token, frame)
	if err != nil {
		return refusal(fmt.Errorf("%w: %v", ErrNotSent, err)), nil
	}

	if err := a.sendFrame(reg.MessagingSocketPath, wire); err != nil {
		return refusal(err), nil
	}

	rec := &runRecord{
		key:       req.Key,
		epoch:     req.Epoch,
		msgID:     frame.MsgID,
		state:     protocol.StateRunning,
		bodyMark:  frame.Message.Content,
		createdAt: a.now(),
	}
	a.mu.Lock()
	a.runs[req.Key] = rec
	a.mu.Unlock()
	a.kickConfirmations()
	return core.Admission{Admitted: true, RunID: frame.MsgID}, nil
}

// sendFrame dials the target's UDS and writes the framed bytes. The
// receiver answers nothing (capture §4); the write itself is the only
// transport signal, and any error before the full write is a definitive
// not-sent.
func (a *Attachment) sendFrame(sockPath string, wire []byte) error {
	ctx, cancel := context.WithTimeout(a.ctx, submitTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sockPath)
	if err != nil {
		return fmt.Errorf("%w: dial %s: %v", ErrNotSent, sockPath, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	}
	n, err := conn.Write(wire)
	if err != nil || n != len(wire) {
		_ = conn.Close()
		return fmt.Errorf("%w: short write to %s (%d/%d): %v", ErrNotSent, sockPath, n, len(wire), err)
	}
	// Half-close: the receiver never sends bytes back (capture §4), so a
	// clean shutdown is the correct end of the exchange.
	if tc, ok := conn.(*net.UnixConn); ok {
		_ = tc.CloseWrite()
	}
	_ = conn.Close()
	return nil
}

func refusal(err error) core.Admission {
	return core.Admission{Code: protocol.CodeUnsupported, Message: err.Error()}
}

// sanitizeAddr reduces the target id to the envelope's safe attribute
// class ([a-z0-9-]) so the from attribute always round-trips.
func sanitizeAddr(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	return strings.Trim(b.String(), "-")
}

// evidenceFor renders a run record as endpoint evidence per the ladder:
// admitted -> Confirmed; socket-written only -> Tentative. A lost record
// (restart) is Unknown, never None — the socket has no admission
// primitive to prove non-admission.
func evidenceFor(rec *runRecord) core.Evidence {
	switch {
	case rec.admitted:
		ev := core.Evidence{Known: true, Class: core.EvidenceConfirmed, Admitted: true, RunID: rec.msgID, State: rec.state}
		if rec.terminal && rec.result != nil {
			ev.Result = rec.result
		}
		return ev
	default:
		return core.Evidence{Known: true, Class: core.EvidenceTentative, RunID: rec.msgID, State: rec.state}
	}
}

// Lookup implements core.Attachment over the retained run map: a key this
// adapter submitted is Tentative (or Confirmed once admitted); anything
// else is Unknown — the socket wire has no rejection primitive, so a lost
// submit and a never-submitted key are indistinguishable and must never
// read as EvidenceNone.
func (a *Attachment) Lookup(key requests.Key, _ string) (core.Evidence, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if rec, ok := a.runs[key]; ok {
		return evidenceFor(rec), nil
	}
	return core.Evidence{Known: true, Class: core.EvidenceUnknown}, nil
}

// kickConfirmations starts the transcript confirmation poller once. The
// poller is NOT on the request path (no child process either): it reads
// the target transcript on a ticker and upgrades evidence
// tentative→submitted→admitted→terminal per the ladder.
func (a *Attachment) kickConfirmations() {
	a.mu.Lock()
	if a.confirmCancel != nil {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.confirmCancel = cancel
	a.mu.Unlock()
	go a.confirmLoop(ctx)
}

func (a *Attachment) confirmLoop(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.pollConfirmations()
		}
	}
}

// pollConfirmations upgrades every non-terminal run by reading the target
// transcript once per tick. Read errors are swallowed (the transcript may
// not exist yet); the evidence ladder only ever moves forward.
func (a *Attachment) pollConfirmations() {
	reg, err := readSessionRegistry(a.home, a.cfg.Pid)
	if err != nil || reg == nil || reg.SessionID == "" {
		return
	}
	tail := readTranscriptTail(a.home, reg.Cwd, reg.SessionID)
	if tail == "" {
		return
	}
	stopGrew := a.consumeStopMarker(reg.SessionID)
	lastAssistant := lastAssistantText(tail)
	a.mu.Lock()
	defer a.mu.Unlock()
	var newest *runRecord
	for _, rec := range a.runs {
		if rec.terminal {
			continue
		}
		if !rec.submitted && strings.Contains(tail, rec.bodyMark) {
			rec.submitted = true
		}
		if rec.submitted && !rec.admitted && transcriptAssistantTurnStarted(tail, rec.bodyMark) {
			rec.admitted = true
			rec.state = protocol.StateRunning
			if cb := a.eventSink; cb != nil {
				cb(core.NativeEvent{Type: core.EventStatus, Status: "busy", Attachment: "claude"})
			}
		}
		if rec.admitted && newest == nil || (newest != nil && rec.createdAt.After(newest.createdAt)) {
			if rec.admitted {
				newest = rec
			}
		}
	}
	// A Stop event ends the most recent admitted turn: the endpoint
	// serializes one in-flight request per target, so the newest admitted
	// non-terminal run is the turn that stopped.
	if stopGrew && newest != nil {
		newest.terminal = true
		newest.state = protocol.StateCompleted
		res := &protocol.Result{Text: lastAssistant, NativeRef: newest.msgID}
		newest.result = res
		if cb := a.eventSink; cb != nil {
			cb(core.NativeEvent{Type: core.EventRunCompleted, Key: newest.key, RunID: newest.msgID, Result: res})
		}
	}
}

// consumeStopMarker drains the receiver's marker file for the session:
// returns true when at least one new stop event arrived since the last
// poll. The leaf is lstat-guarded like every other local-writable file;
// unreadable or missing markers are silent no-ops (the ladder never
// moves backward).
func (a *Attachment) consumeStopMarker(sessionID string) bool {
	path := stopMarkerPath(a.home, sessionID)
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if !fi.Mode().IsRegular() {
		return false
	}
	if fi.Size() == a.stopSeen {
		return false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollowFlag, 0)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if fi2, err := f.Stat(); err != nil || !fi2.Mode().IsRegular() {
		return false
	}
	a.stopSeen = fi.Size()
	return true
}

// lastAssistantText extracts the newest assistant message's text from a
// transcript tail. The content field is a string or a block array; both
// shapes are handled, anything unparseable yields "" (the completion
// still flows — the text is best-effort attribution).
func lastAssistantText(tail string) string {
	const marker = `"type":"assistant"`
	idx := strings.LastIndex(tail, marker)
	if idx < 0 {
		return ""
	}
	// Find the start of the enclosing JSON line.
	start := strings.LastIndexByte(tail[:idx], '\n') + 1
	end := strings.IndexByte(tail[idx:], '\n')
	if end < 0 {
		end = len(tail) - idx
	}
	line := tail[start : idx+end]
	var entry struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(entry.Message.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(entry.Message.Content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// transcriptAssistantTurnStarted reports whether an assistant entry
// follows the submitted user line in the tail — the transcript-side
// signal that the model turn began (the Stop hook is the turn END).
func transcriptAssistantTurnStarted(tail, bodyMark string) bool {
	idx := strings.LastIndex(tail, bodyMark)
	if idx < 0 {
		return false
	}
	rest := tail[idx+len(bodyMark):]
	return strings.Contains(rest, `"type":"assistant"`) || strings.Contains(rest, `"type": "assistant"`)
}
