package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	// userTS is the transcript timestamp (unix ms) of the entry that
	// delivered our msg_id; 0 until observed or when the entry has none.
	userTS int64
	// fromOff is the transcript size just before the frame was written:
	// the delivery entry can only appear at or after it, so the cursor
	// starts there and never needs a window that could scroll past it
	// (codex #855 r2 item 1). -1 when the size could not be read.
	fromOff int64
	// turnEnded/turnEndTS: a later prompt started a new turn, so no Stop
	// after turnEndTS belongs to this run (codex #855 r2 item 2).
	turnEnded bool
	turnEndTS int64
	// lastText is the newest non-empty assistant text of this run's turn.
	lastText  string
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
	// The frame's msg_id is derived from the request key, not random, so a
	// restarted endpoint can find this delivery in the transcript by key
	// alone (611.25). The same key always yields the same id, which also
	// lets the receiver's msg_id dedup absorb a retried send.
	frame.MsgID = frameMsgID(req.Key)
	wire, err := EncodeFrames(token, frame)
	if err != nil {
		return refusal(fmt.Errorf("%w: %v", ErrNotSent, err)), nil
	}

	fromOff := transcriptSize(transcriptPath(a.home, reg.Cwd, reg.SessionID))
	if err := a.sendFrame(reg.MessagingSocketPath, wire); err != nil {
		return refusal(err), nil
	}

	rec := &runRecord{
		key:       req.Key,
		epoch:     req.Epoch,
		msgID:     frame.MsgID,
		state:     protocol.StateRunning,
		fromOff:   fromOff,
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
	if rec, ok := a.runs[key]; ok {
		ev := evidenceFor(rec)
		a.mu.Unlock()
		return ev, nil
	}
	_, released := a.released[key]
	a.mu.Unlock()
	if released {
		// Acknowledged: the endpoint holds the outcome and nothing is
		// retained here. Never recover it from the transcript.
		return core.Evidence{Known: true, Class: core.EvidenceUnknown}, nil
	}
	return a.recoverRun(key), nil
}

// kickConfirmations starts the transcript confirmation poller if it is not
// running. The poller is NOT on the request path (no child process either):
// it follows the target transcript on a ticker and upgrades evidence
// tentative→submitted→admitted→terminal per the ladder. It stops by itself
// when no run is left to confirm, and for good when the endpoint
// unsubscribes (codex #855 r2 item 6: it used to run forever).
func (a *Attachment) kickConfirmations() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.confirmCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.confirmCancel = cancel
	go a.confirmLoop(ctx, cancel)
}

func (a *Attachment) confirmLoop(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.pollConfirmations()
			if a.idleStop() {
				return
			}
		}
	}
}

// idleStop ends the poller when no run is left to confirm. The check and
// the reset happen under a.mu, and Submit records its run under a.mu before
// kicking, so a run added concurrently either keeps this loop alive or
// starts a fresh one. The cursor is reset so the next run seeds it from its
// own submit-time offset instead of replaying the idle gap.
func (a *Attachment) idleStop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, rec := range a.runs {
		if !rec.terminal {
			return false
		}
	}
	a.confirmCancel = nil
	a.cur = transcriptCursor{}
	a.owner = nil
	return true
}

// transcriptCursor is the poller's position in one transcript file.
type transcriptCursor struct {
	path     string
	off      int64
	skipping bool
}

// transcriptSize is the size of a regular transcript file, 0 when it does
// not exist yet (the delivery will create it), -1 when it cannot be read.
func transcriptSize(path string) int64 {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return 0
	case err != nil || !fi.Mode().IsRegular():
		return -1
	}
	return fi.Size()
}

// pollConfirmations advances the cursor over the transcript lines appended
// since the last tick, applies them in file order to the turn state
// machine, then binds Stop markers to turns. File I/O happens outside a.mu
// (the poller is the only writer of the cursor), and every native event is
// emitted after a.mu is released: the endpoint's completion handler calls
// AcknowledgeResult synchronously, which takes a.mu (codex #855 r1 item 6).
func (a *Attachment) pollConfirmations() {
	reg, err := readSessionRegistry(a.home, a.cfg.Pid)
	if err != nil || reg == nil || reg.SessionID == "" {
		return
	}
	path := transcriptPath(a.home, reg.Cwd, reg.SessionID)

	a.mu.Lock()
	cur := a.cur
	gen := a.curGen
	fresh := cur.path != path
	if fresh {
		// New or switched transcript: start at the earliest submit-time
		// offset of a run still waiting for its delivery entry.
		cur = transcriptCursor{path: path, off: -1}
		for _, rec := range a.runs {
			if !rec.terminal && rec.fromOff >= 0 && (cur.off < 0 || rec.fromOff < cur.off) {
				cur.off = rec.fromOff
			}
		}
		a.owner = nil
	}
	from := a.stopConsumed
	a.mu.Unlock()

	// Stop markers are read BEFORE the transcript and bound only once the
	// cursor has reached the end of the file it read (codex #855 r3 item
	// 1). The harness writes a turn's entries before the Stop hook runs, so
	// every marker read here names a turn whose entries all precede the
	// transcript read. Binding while the cursor lags would publish a result
	// taken from a partial turn and miss a later turn boundary.
	stops := readStopMarkers(stopMarkerPath(a.home, reg.SessionID), from)

	if cur.off < 0 {
		// No run knows where its delivery starts: begin at the last chunk,
		// discarding the partial first line.
		size := transcriptSize(path)
		if size < 0 {
			return
		}
		cur.off, cur.skipping = transcriptStartOffset(size)
	}
	rd, err := readTranscriptFrom(path, cur.off, cur.skipping)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	caughtUp := err == nil && !rd.skipping && rd.next >= rd.size
	if err == nil && rd.size < cur.off {
		// Truncated or replaced in place: restart from the top next tick,
		// and bind nothing until that re-read has caught up.
		rd, caughtUp = transcriptRead{next: 0}, false
	}
	cur.off, cur.skipping = rd.next, rd.skipping

	var events []core.NativeEvent
	a.mu.Lock()
	for _, line := range rd.lines {
		if e, ok := parseTranscriptLine(line); ok {
			events = a.applyEntryLocked(e, events)
		}
	}
	if a.curGen == gen {
		a.cur = cur
	}
	if caughtUp {
		events = a.bindStopsLocked(stops, events)
	}
	sink := a.eventSink
	a.mu.Unlock()

	if sink != nil {
		for _, ev := range events {
			sink(ev)
		}
	}
}

// applyEntryLocked feeds one transcript entry to the turn state machine.
//
//   - A prompt carrying our msg_id (a new user entry, or a frame absorbed
//     into the running turn) marks that run submitted and makes it the
//     owner of the current turn. msg_id proves delivery ownership only.
//   - Any other non-meta prompt that starts a new turn (not absorbed) ends
//     the current owner's turn and clears the owner: what follows answers
//     someone else (codex #855 r2 item 2).
//   - An assistant entry while a run owns the turn admits it and records
//     the turn's newest text as the result.
//
// Caller holds a.mu.
func (a *Attachment) applyEntryLocked(e transcriptEntry, events []core.NativeEvent) []core.NativeEvent {
	switch {
	case e.Type == "user" && !e.Meta && e.Text != "":
		if rec := a.runByMsgIDLocked(e.MsgID); rec != nil {
			if !rec.submitted {
				rec.submitted, rec.userTS = true, e.TS
			}
			if !e.Absorbed && a.owner != nil && a.owner != rec {
				a.owner.endTurn(e.TS)
			}
			a.owner = rec
			return events
		}
		if !e.Absorbed {
			if a.owner != nil {
				a.owner.endTurn(e.TS)
			}
			a.owner = nil
		}
	case e.Type == "assistant" && a.owner != nil && !a.owner.terminal:
		rec := a.owner
		if !rec.admitted {
			rec.admitted = true
			rec.state = protocol.StateRunning
			events = append(events, core.NativeEvent{Type: core.EventStatus, Status: "busy", Attachment: "claude"})
		}
		if e.Text != "" {
			rec.lastText = e.Text
		}
	}
	return events
}

// runByMsgIDLocked returns the non-terminal run whose frame carried msgID.
func (a *Attachment) runByMsgIDLocked(msgID string) *runRecord {
	if msgID == "" {
		return nil
	}
	for _, rec := range a.runs {
		if !rec.terminal && rec.msgID == msgID {
			return rec
		}
	}
	return nil
}

// endTurn records that a later prompt started a new turn at ts.
func (rec *runRecord) endTurn(ts int64) {
	if !rec.turnEnded {
		rec.turnEnded, rec.turnEndTS = true, ts
	}
}

// claims reports whether a Stop at ts can belong to this run's turn: at or
// after the delivery, and not after a later prompt ended the turn. A turn
// that ended at an unknown time (no timestamp on the next prompt) claims
// nothing.
func (rec *runRecord) claims(ts int64) bool {
	if rec.terminal || rec.bindTS() > ts {
		return false
	}
	return !rec.turnEnded || (rec.turnEndTS > 0 && ts <= rec.turnEndTS)
}

// bindTS is the earliest time a Stop for this run can carry: its delivery
// entry's timestamp when observed, else its submit time.
func (rec *runRecord) bindTS() int64 {
	if rec.userTS > 0 {
		return rec.userTS
	}
	return rec.createdAt.UnixMilli()
}

// bindStopsLocked settles Stop markers in file order. A marker completes
// the oldest admitted run whose turn claims it, with that turn's newest
// assistant text as the result. A marker no run can claim is dropped; a
// marker that a not-yet-admitted run could still claim is PRESERVED (the
// transcript may lag the hook by a tick) and the consumed offset stops
// there. Caller holds a.mu.
func (a *Attachment) bindStopsLocked(stops stopMarkers, events []core.NativeEvent) []core.NativeEvent {
	if stops.truncated {
		a.stopConsumed = 0
	}
	for _, s := range stops.lines {
		var done, pending *runRecord
		for _, rec := range a.runs {
			if !rec.claims(s.ts) {
				continue
			}
			if rec.admitted {
				if done == nil || rec.createdAt.Before(done.createdAt) {
					done = rec
				}
			} else {
				pending = rec
			}
		}
		if done == nil && pending != nil {
			break
		}
		a.stopConsumed = s.end
		if done == nil {
			continue
		}
		done.terminal = true
		done.state = protocol.StateCompleted
		done.result = &protocol.Result{Text: done.lastText, NativeRef: done.msgID}
		if a.owner == done {
			a.owner = nil
		}
		events = append(events, core.NativeEvent{Type: core.EventRunCompleted, Key: done.key, RunID: done.msgID, Result: done.result})
	}
	return events
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

// frameMsgID derives the frame msg_id from the request key: the first 16
// bytes of SHA-256 over a domain tag and the key's three parts, formatted
// as an RFC 4122 version-5-style UUID. Distinct keys give distinct ids.
func frameMsgID(key requests.Key) string {
	sum := sha256.Sum256([]byte("amq-remote/claude/msg_id\x00" + key.CreatorHost + "\x00" + key.TargetID + "\x00" + key.RequestID))
	u := sum[:16]
	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// recoverScanBytes bounds how far back the first recovery scan for a key
// reads; later scans for the same key read only what was appended since.
const recoverScanBytes = 16 << 20

// recoverRun answers Lookup for a key this attachment holds no record of,
// which after an endpoint restart is every key that was in flight (611.25).
// It scans the transcript for the entry that delivered the key's frame
// (origin.msg_id == frameMsgID(key)). Found: the run is rebuilt as
// submitted, with its delivery offset and timestamp, the poller cursor is
// reset so the turn state machine replays from that entry, and the answer
// is Tentative; admission and completion follow from the poller exactly as
// for a live run, including a Stop recorded while the endpoint was down.
// Not found: Unknown, as before, and the scan offset is kept so the next
// tick reads only new bytes. File I/O runs without a.mu.
func (a *Attachment) recoverRun(key requests.Key) core.Evidence {
	unknown := core.Evidence{Known: true, Class: core.EvidenceUnknown}
	reg, err := readSessionRegistry(a.home, a.cfg.Pid)
	if err != nil || reg == nil || reg.SessionID == "" {
		return unknown
	}
	path := transcriptPath(a.home, reg.Cwd, reg.SessionID)
	msgID := frameMsgID(key)

	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return unknown
	}
	a.mu.Lock()
	scan, seen := a.recoverFrom[key]
	a.mu.Unlock()
	// The saved position is only valid for the same transcript file, not
	// truncated below it (codex #859 P2): a changed session path, a replaced
	// file, or a shorter file restarts the scan from the recovery window.
	if !seen || scan.path != path || scan.file == nil || !os.SameFile(scan.file, fi) || fi.Size() < scan.off {
		scan = recoverScan{path: path, file: fi}
		if fi.Size() > recoverScanBytes {
			scan.off, scan.skipping = fi.Size()-recoverScanBytes, true
		}
	}
	found, off, ts, next, nextSkipping := scanForDelivery(path, scan.off, scan.skipping, msgID)

	a.mu.Lock()
	if rec, ok := a.runs[key]; ok { // a concurrent Lookup won the race
		ev := evidenceFor(rec)
		a.mu.Unlock()
		return ev
	}
	if !found {
		a.recoverFrom[key] = recoverScan{path: path, file: fi, off: next, skipping: nextSkipping}
		a.mu.Unlock()
		return unknown
	}
	delete(a.recoverFrom, key)
	rec := &runRecord{
		key:       key,
		msgID:     msgID,
		state:     protocol.StateRunning,
		submitted: true,
		userTS:    ts,
		fromOff:   off,
		createdAt: a.now(),
	}
	a.runs[key] = rec
	a.cur = transcriptCursor{}
	a.owner = nil
	a.curGen++
	ev := evidenceFor(rec)
	a.mu.Unlock()
	a.kickConfirmations()
	return ev
}

// scanForDelivery reads the transcript from off to its current end looking
// for the entry that delivered msgID. It returns that entry's byte offset
// and timestamp when found, and in every case the position it reached and
// whether it stopped inside an over-long line. It always returns once a
// read makes no progress, including at an unfinished line at EOF (codex
// #859 P1: skipping at EOF looped forever).
func scanForDelivery(path string, off int64, skipping bool, msgID string) (found bool, at, ts, next int64, nextSkipping bool) {
	for {
		rd, err := readTranscriptFrom(path, off, skipping)
		if err != nil {
			return false, 0, 0, off, skipping
		}
		for i, line := range rd.lines {
			if e, ok := parseTranscriptLine(line); ok && e.Type == "user" && e.MsgID == msgID {
				return true, rd.starts[i], e.TS, rd.next, rd.skipping
			}
		}
		if rd.next == off {
			return false, 0, 0, off, rd.skipping
		}
		off, skipping = rd.next, rd.skipping
	}
}

// recoverScan is where a restart-recovery scan for one key stopped, bound to
// the transcript file it read.
type recoverScan struct {
	path     string
	file     os.FileInfo
	off      int64
	skipping bool
}
