package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// userTS is the transcript timestamp (unix ms) of the user line that
	// carries our msg_id; 0 until observed or when the entry has none.
	userTS    int64
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

// pollConfirmations upgrades every non-terminal run from one read of the
// target transcript and the session's Stop-marker file. Read errors are
// swallowed (the transcript may not exist yet); the evidence ladder only
// ever moves forward.
//
// Correlation is on DECODED transcript entries (codex #855 r1 item 2), and
// every native event is emitted after a.mu is released (item 6): the
// endpoint's completion handler calls AcknowledgeResult synchronously,
// which reacquires a.mu, so a callback under the lock deadlocks the run
// exactly when confirmation works.
func (a *Attachment) pollConfirmations() {
	reg, err := readSessionRegistry(a.home, a.cfg.Pid)
	if err != nil || reg == nil || reg.SessionID == "" {
		return
	}
	entries := parseTranscriptTail(readTranscriptTail(a.home, reg.Cwd, reg.SessionID))

	a.mu.Lock()
	from := a.stopConsumed
	a.mu.Unlock()
	// The poller is the single consumer of stopConsumed; reading the file
	// outside the lock is safe and keeps file I/O off the mutex.
	stops := readStopMarkers(stopMarkerPath(a.home, reg.SessionID), from)

	var events []core.NativeEvent
	a.mu.Lock()
	for _, rec := range a.runs {
		if rec.terminal {
			continue
		}
		ui := userIndex(entries, rec.msgID)
		if ui < 0 {
			continue
		}
		rec.submitted = true
		if rec.userTS == 0 {
			rec.userTS = entries[ui].TS
		}
		if !rec.admitted && assistantAfter(entries, ui) {
			rec.admitted = true
			rec.state = protocol.StateRunning
			events = append(events, core.NativeEvent{Type: core.EventStatus, Status: "busy", Attachment: "claude"})
		}
	}
	// Stop binding (item 7): a marker completes the oldest admitted run
	// whose transcript user line is at or before the marker. The target
	// delivers our frame only at a turn boundary, so the Stop of a turn
	// that was already running when we submitted predates our user line
	// and never completes our run. A marker no run can claim is dropped; a
	// marker that a not-yet-admitted run could still claim is PRESERVED
	// (the transcript read may lag the hook by a tick) — the consumed
	// offset stops there.
	if stops.truncated {
		a.stopConsumed = 0
	}
	for _, s := range stops.lines {
		if rec := a.oldestAdmittedAtOrBefore(s.ts); rec != nil {
			rec.terminal = true
			rec.state = protocol.StateCompleted
			res := &protocol.Result{Text: lastAssistantAfter(entries, userIndex(entries, rec.msgID)), NativeRef: rec.msgID}
			rec.result = res
			events = append(events, core.NativeEvent{Type: core.EventRunCompleted, Key: rec.key, RunID: rec.msgID, Result: res})
			a.stopConsumed = s.end
			continue
		}
		if a.pendingCouldClaim(s.ts) {
			break
		}
		a.stopConsumed = s.end
	}
	sink := a.eventSink
	a.mu.Unlock()

	if sink != nil {
		for _, ev := range events {
			sink(ev)
		}
	}
}

// oldestAdmittedAtOrBefore returns the admitted, non-terminal run with the
// earliest user line whose transcript timestamp is <= ts (unix ms), or
// nil. A run whose user line carried no timestamp binds on createdAt, the
// weaker lower bound. Caller holds a.mu.
func (a *Attachment) oldestAdmittedAtOrBefore(ts int64) *runRecord {
	var best *runRecord
	for _, rec := range a.runs {
		if rec.terminal || !rec.admitted || rec.bindTS() > ts {
			continue
		}
		if best == nil || rec.createdAt.Before(best.createdAt) {
			best = rec
		}
	}
	return best
}

// pendingCouldClaim reports whether a non-terminal run that is not yet
// admitted could still own a marker at ts: its admission evidence may
// arrive on a later tick, so the marker must not be dropped yet. Caller
// holds a.mu.
func (a *Attachment) pendingCouldClaim(ts int64) bool {
	for _, rec := range a.runs {
		if !rec.terminal && !rec.admitted && rec.bindTS() <= ts {
			return true
		}
	}
	return false
}

// bindTS is the earliest time a Stop for this run can carry: its
// transcript user line when observed, else its submit time.
func (rec *runRecord) bindTS() int64 {
	if rec.userTS > 0 {
		return rec.userTS
	}
	return rec.createdAt.UnixMilli()
}

// stopObs is one decoded Stop-marker line: the receiver's wall-clock
// timestamp and the byte offset just past the line.
type stopObs struct {
	ts  int64
	end int64
}

type stopMarkers struct {
	lines []stopObs
	// truncated: the file is now shorter than the consumed offset (rotated
	// or removed); the caller restarts from 0.
	truncated bool
}

// maxStopMarkerBytes bounds the marker read; each line is ~60 bytes and
// settled lines are never re-read.
const maxStopMarkerBytes = 1 << 20

// readStopMarkers decodes the marker lines appended after byte offset
// `from`. The open goes through openRegular (lstat gate, no-follow,
// non-blocking, fstat recheck). Missing or unreadable markers yield no
// lines; a partial trailing line is left for the next poll.
func readStopMarkers(path string, from int64) stopMarkers {
	f, fi, err := openRegular(path, 0)
	if err != nil {
		return stopMarkers{}
	}
	defer func() { _ = f.Close() }()
	size := fi.Size()
	if size < from {
		return stopMarkers{truncated: true}
	}
	if size == from {
		return stopMarkers{}
	}
	if size-from > maxStopMarkerBytes {
		size = from + maxStopMarkerBytes
	}
	if _, err := f.Seek(from, 0); err != nil {
		return stopMarkers{}
	}
	buf := make([]byte, size-from)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]
	var out stopMarkers
	pos := int64(0)
	for {
		nl := bytes.IndexByte(buf[pos:], '\n')
		if nl < 0 {
			break
		}
		line := buf[pos : pos+int64(nl)]
		end := from + pos + int64(nl) + 1
		pos += int64(nl) + 1
		var m struct {
			TS int64 `json:"ts"`
		}
		if err := json.Unmarshal(line, &m); err != nil || m.TS == 0 {
			// Undecodable marker: settle past it; it names no turn.
			out.lines = append(out.lines, stopObs{ts: -1, end: end})
			continue
		}
		out.lines = append(out.lines, stopObs{ts: m.TS, end: end})
	}
	return out
}

// userIndex returns the index of the delivered entry whose origin.msg_id
// is our frame's msg_id, or -1. The msg_id is generated fresh per frame,
// so a match is exact ownership, never a text coincidence.
func userIndex(entries []transcriptEntry, msgID string) int {
	if msgID == "" {
		return -1
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == "user" && entries[i].MsgID == msgID {
			return i
		}
	}
	return -1
}

// assistantAfter reports whether an assistant entry follows index ui —
// the transcript-side signal that the model turn began (the Stop hook is
// the turn END).
func assistantAfter(entries []transcriptEntry, ui int) bool {
	for i := ui + 1; i < len(entries); i++ {
		if entries[i].Type == "assistant" {
			return true
		}
	}
	return false
}

// lastAssistantAfter returns the newest non-empty assistant text of the
// turn that follows index ui: entries after ui up to the next real user
// prompt (a user entry with text; tool results decode to ""). Best-effort
// attribution: "" when the tail holds none.
func lastAssistantAfter(entries []transcriptEntry, ui int) string {
	end := len(entries)
	for i := ui + 1; i < len(entries); i++ {
		if entries[i].Type == "user" && entries[i].Text != "" {
			end = i
			break
		}
	}
	for i := end - 1; i > ui; i-- {
		if entries[i].Type == "assistant" && entries[i].Text != "" {
			return entries[i].Text
		}
	}
	return ""
}
