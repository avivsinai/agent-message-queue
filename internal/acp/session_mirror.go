package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/mirror"
)

// The machine agent mirrors each turn of a session into the session's
// channel. A Claude turn is posted at its first assistant line, edited while
// it runs and edited a last time when it ends; a turn of another harness is
// posted when it ends. The transcript is the record and the post is a view
// of it: the mirror never writes to the session, a mailbox or a binding.
// Each binding's state lives in <mirror root>/<binding>/:
//
//	state.json       the transcript cursor, the fold, the queue of turn
//	                 events not yet sent, and the watermark
//	posted/<turn>    the claim of one turn's post, created before the post;
//	                 a claim with no event id is never posted or edited again
//	carrier/<msg_id> a frame the native carrier sent (mirror.RecordCarried)
//	off              present while the owner turned the mirror off
//
// A post is claimed before it is sent, and an unknown send outcome is never
// retried: a lost post is acceptable for a view, a duplicate is not. An edit
// is idempotent, so the final edit of a turn is tried once more.

// mirrorInterval is the gap between mirror ticks.
const mirrorInterval = 2 * time.Second

// A running turn's post is edited only when what it shows changed, at
// least mirrorEditGap after the binding's last post or edit, and at most
// mirrorProgressMax times, except to show it interrupted or running again.
// A Continued edit also waits for the gap, with no cap. Each edit is a
// stored relay event that clients fetch, so the bound is on stored events,
// not on freshness. The Ended edit is sent at once and not counted.
const (
	mirrorEditGap     = 30 * time.Second
	mirrorProgressMax = 10
)

// mirrorReadMax bounds the transcript bytes one binding reads in one tick.
const mirrorReadMax = 64 << 20

// mirrorQueueMax is the most turn events a binding's queue holds; a longer
// queue keeps only its last event, which counts the turns it dropped.
const mirrorQueueMax = 5

// Backoff of one binding after a failed post.
const (
	mirrorBackoffMin = 30 * time.Second
	mirrorBackoffMax = 10 * time.Minute
)

// mirrorNote is posted once when a session's transcript cannot be read.
const mirrorNote = "This session's activity is not available here"

// mirrorState is the mirror's state, owned by the one worker goroutine that
// runs mirrorTick.
type mirrorState struct {
	// quit closes when the mirror worker stops; nil outside it.
	quit <-chan struct{}
	// release drops the mirror lead; nil when not held.
	release func()
	backoff map[string]mirrorBackoff
	// scans are each mailbox binding's buzz inbox scans, new and cur.
	scans map[string]mirrorScans
}

type mirrorBackoff struct {
	until time.Time
	step  time.Duration
}

type mirrorScans struct {
	root     string
	new, cur *inboxScan
}

// mirrorFile is one binding's state.json.
type mirrorFile struct {
	Channel string `json:"channel"`
	// Path is the transcript the cursor reads, File its identity, Offset
	// the next byte to read. Skipping is set inside a line longer than one
	// read, which is skipped; Skipped counts such lines.
	Path     string       `json:"path,omitempty"`
	File     string       `json:"file,omitempty"`
	Offset   int64        `json:"offset"`
	Skipping bool         `json:"skipping,omitempty"`
	Skipped  int          `json:"skipped,omitempty"`
	Fold     mirror.State `json:"fold"`
	// Queue holds the turn events not yet sent, oldest first.
	Queue []mirror.Event `json:"queue,omitempty"`
	// Watermark is the time the mirror first saw the transcript, then the
	// last line time of the newest turn sent: a turn that ends at or before
	// it is never posted.
	Watermark int64 `json:"watermark,omitempty"`
	// Noted is set once the note was posted, to the locator it was posted
	// for ("" for none).
	Noted *string `json:"noted,omitempty"`
	// Harness names the source of the record, so a binding that is gone
	// can still be read to its end.
	Harness string `json:"harness,omitempty"`
	// Live is the turn whose post shows it running: the post has no final
	// edit yet. Edited is the time of the binding's last post or edit, in
	// unix milliseconds.
	Live   string `json:"live,omitempty"`
	Edited int64  `json:"edited,omitempty"`
}

// mirrorStopped reports whether the mirror worker is stopping.
func (s *Server) mirrorStopped() bool {
	select {
	case <-s.mirrors.quit:
		return true
	default:
		return false
	}
}

// mirrorLead reports whether this worker runs its machine agent's mirror:
// the worker that holds <machine>/<self>.mirror.lock.
func (s *Server) mirrorLead() bool {
	if s.mirrors.release != nil || !lock.AdvisoryLockAvailable() {
		return true
	}
	if buzzSelf == "" {
		return false
	}
	dir := s.agentMarkerDir(machineName)
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	release, ok, err := lock.TryExclusive(filepath.Join(dir, buzzSelf+".mirror.lock"))
	if err != nil || !ok {
		return false
	}
	s.mirrors.release = release
	return true
}

// mirrorTick mirrors every live Claude binding with a channel, then posts
// the open turn of each binding that ended and removes the state of each
// binding that is gone.
func (s *Server) mirrorTick(now time.Time) {
	served := map[string]bool{}
	for _, t := range s.dialogTargets() {
		if s.mirrorStopped() {
			return
		}
		served[t.name] = true
		// A mirrored binding is read fresh: it names the transcript to post.
		if mirror.SourceFor(t.binding.Harness) == nil {
			continue
		}
		if b, err := binding.ReadNamed(t.name); err == nil && mirror.SourceFor(b.Harness) != nil {
			s.mirrorBinding(t, b, now, false)
		}
	}
	if !s.mirrorStopped() {
		s.dropGoneMirrors(served, now)
	}
}

// dropGoneMirrors posts the open turn of each binding that is gone or ended
// as "session ended", then removes its state, claims and carrier records.
// A post proved not sent keeps them for the next tick.
func (s *Server) dropGoneMirrors(served map[string]bool, now time.Time) {
	root, err := mirror.Root()
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(root)
	snap := s.snapshot()
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || served[name] {
			continue
		}
		// The snapshot only lets a live binding be skipped: whether it is
		// gone or ended is decided from a fresh read, since a session can
		// attach again while an earlier name's relay call runs.
		if b, ok := snap.binding(name); ok && live(b) {
			continue
		}
		b, err := binding.ReadNamed(name)
		switch {
		case errors.Is(err, binding.ErrNone), binding.ValidName(name) != nil:
		case err == nil && !live(b):
		default:
			continue
		}
		dir := filepath.Join(root, name)
		if st, _, found := readMirrorFile(dir); found && st.Channel != "" && binding.ValidName(name) == nil {
			if !s.mirrorBinding(dialogTarget{name: name, channel: st.Channel}, b, now, true) {
				continue
			}
		}
		_ = os.RemoveAll(dir)
		delete(s.mirrors.scans, name)
		delete(s.mirrors.backoff, name)
	}
}

// mirrorBinding is one tick of one binding: locate, read, fold, queue and
// send. ended reads what the transcript still holds and ends the open turn
// as "session ended". It reports whether the queue was sent out; a panic is
// recovered and backs the binding off.
func (s *Server) mirrorBinding(t dialogTarget, b binding.Binding, now time.Time, ended bool) (sent bool) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "amq-acp: session mirror %s: %v\n", t.name, r)
			s.mirrorBackOff(t.name, now)
			sent = false
		}
	}()
	if bo, ok := s.mirrors.backoff[t.name]; ok && now.Before(bo.until) {
		return false
	}
	dir, err := mirror.DirFor(t.name)
	if err != nil {
		return true
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	st, saved, _ := readMirrorFile(dir)
	st.Channel = t.channel
	if b.Harness != "" {
		st.Harness = b.Harness
	}
	src := mirror.SourceFor(st.Harness)
	if src == nil {
		return true
	}
	off := mirror.Off(dir)
	switch path := src.Locate(b, st.Path); {
	case ended:
		if st.Path != "" {
			s.mirrorRead(dir, src, &st, st.Path, now)
		}
		var evs []mirror.Event
		st.Fold, evs = mirror.Close(st.Fold, mirror.EndSessionEnded)
		st.Queue = append(st.Queue, evs...)
	case path == "":
		s.mirrorUnavailable(dir, &st, "", off)
	case !s.mirrorRead(dir, src, &st, path, now):
		s.mirrorUnavailable(dir, &st, path, off)
	}
	st.Queue = mergeQueue(st.Queue, st.Watermark, st.Live)
	if off {
		st.offQueue()
	}
	if raw, err := json.Marshal(st); err != nil || !bytes.Equal(raw, saved) && writeMirrorFile(dir, st) != nil {
		return false
	}
	return s.mirrorSend(dir, t, b, &st, now)
}

// mirrorRead reads the new lines of path and queues the turn events they
// make. A changed path ends the open turn as "session cleared" and reads
// the new file from its start; a binding seen for the first time starts at
// the end of its transcript. A file replaced in place (another identity, or
// shorter than the offset) is read from its start again: the claims and the
// watermark stop the turns already posted. For a Lazy source a missing file
// is an empty record: seen first that way, it is read from its start once it
// appears, and turns that end after now are posted. It reports whether path
// could be read.
func (s *Server) mirrorRead(dir string, src *mirror.Source, st *mirrorFile, path string, now time.Time) bool {
	first := st.Path == ""
	if st.Path != path {
		if !first {
			var evs []mirror.Event
			st.Fold, evs = mirror.Close(st.Fold, mirror.EndSessionCleared)
			st.Queue = append(st.Queue, evs...)
		}
		st.Fold.Last = nil
		st.Path, st.File, st.Offset, st.Skipping = path, "", 0, false
	}
	if first {
		// The watermark is the newest line time at first sight, or now for a
		// transcript with no timed line, so a later read from the start (a
		// resumed older session, a replaced file) never posts a turn from
		// before it.
		c, err := claude.TranscriptTail(path)
		if src.Lazy && errors.Is(err, fs.ErrNotExist) {
			st.Watermark = max(st.Watermark, now.UnixMilli())
			return true
		}
		if err != nil {
			st.Path = ""
			return false
		}
		newest := int64(0)
		for _, raw := range c.Lines {
			newest = max(newest, src.Time(raw))
		}
		if newest == 0 {
			newest = now.UnixMilli()
		}
		st.Watermark = max(st.Watermark, newest)
		st.File, st.Offset, st.Skipping = c.File, c.Next, c.Skipping
		return true
	}
	c, err := claude.ReadTranscript(path, st.Offset, st.Skipping)
	if src.Lazy && errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	if st.File != "" && c.File != st.File || c.Size < st.Offset {
		// The re-read folds every turn again; a turn open in the old file
		// must not end at its own opener.
		st.Fold, st.Offset, st.Skipping = mirror.State{}, 0, false
		if c, err = claude.ReadTranscript(path, 0, false); err != nil {
			return false
		}
	}
	st.File = c.File
	carried := mirror.CarriedIn(dir)
	// Read until caught up, at most mirrorReadMax bytes, so a backlog is
	// folded whole before anything is sent.
	for read := int64(0); ; {
		if c.Skipping && !st.Skipping {
			st.Skipped++
		}
		for _, raw := range c.Lines {
			var evs []mirror.Event
			st.Fold, evs = src.Fold(st.Fold, raw, carried)
			st.Queue = append(st.Queue, evs...)
		}
		read += c.Next - st.Offset
		moved := c.Next > st.Offset
		st.Offset, st.Skipping = c.Next, c.Skipping
		if !moved || st.Offset >= c.Size || read >= mirrorReadMax {
			return true
		}
		if c, err = claude.ReadTranscript(path, st.Offset, st.Skipping); err != nil || c.File != st.File {
			return true
		}
	}
}

// mergeQueue drops each event at or before the watermark, which is never
// sent, except one of the live turn. It keeps one event for each change of a
// turn's post: a Progress replaces the turn's queued Opened or Progress; an
// Ended or Continued replaces the turn's queued Progress and folds into its
// queued Opened (then an Ended: the turn is posted once, ended), Ended or
// Continued. A queue longer than mirrorQueueMax keeps its last event, which
// counts the turns it dropped, and the live turn's last event.
func mergeQueue(q []mirror.Event, watermark int64, live string) []mirror.Event {
	var out []mirror.Event
	for _, ev := range q {
		if ev.Kind != mirror.Continued && ev.Turn.LastTS <= watermark && ev.Turn.ID != live {
			continue
		}
		out = mergeEvent(out, ev)
	}
	if len(out) <= mirrorQueueMax {
		return out
	}
	last := out[len(out)-1]
	keep := -1
	for i := len(out) - 2; i >= 0 && live != "" && last.Turn.ID != live; i-- {
		if out[i].Turn.ID == live {
			keep = i
			break
		}
	}
	for _, ev := range out[:len(out)-1] {
		if ev.Kind == mirror.Ended && ev.Turn.ID != live {
			last.Turn.Earlier += 1 + ev.Turn.Earlier
		}
	}
	if keep < 0 {
		return []mirror.Event{last}
	}
	return []mirror.Event{out[keep], last}
}

// mergeEvent adds ev to the merged queue out.
func mergeEvent(out []mirror.Event, ev mirror.Event) []mirror.Event {
	queued := func(kinds ...string) int {
		for i := len(out) - 1; i >= 0; i-- {
			if out[i].Turn.ID == ev.Turn.ID {
				if slices.Contains(kinds, out[i].Kind) {
					return i
				}
				return -1
			}
		}
		return -1
	}
	replace := func(i int, kind string) []mirror.Event {
		earlier := out[i].Turn.Earlier
		out[i].Kind, out[i].Turn = kind, ev.Turn
		out[i].Turn.Earlier = max(earlier, ev.Turn.Earlier)
		return out
	}
	switch ev.Kind {
	case mirror.Progress:
		if i := queued(mirror.Opened, mirror.Progress); i >= 0 {
			return replace(i, out[i].Kind)
		}
	case mirror.Ended, mirror.Continued:
		if i := queued(mirror.Progress); i >= 0 {
			out = slices.Delete(out, i, i+1)
		}
		if i := queued(mirror.Opened, mirror.Ended, mirror.Continued); i >= 0 {
			kind := out[i].Kind
			if kind == mirror.Opened {
				kind = mirror.Ended
			}
			return replace(i, kind)
		}
	}
	return append(out, ev)
}

// offQueue is the queue while the mirror is off: nothing, or one final edit
// of the live turn's post that shows it ended by the switch.
func (st *mirrorFile) offQueue() {
	var ev mirror.Event
	found := false
	for i := len(st.Queue) - 1; i >= 0 && st.Live != ""; i-- {
		if q := st.Queue[i]; q.Turn.ID == st.Live {
			if q.Kind == mirror.Ended && q.Turn.End == mirror.EndMirrorOff {
				// Already queued: its tries are kept.
				st.Queue = []mirror.Event{q}
				return
			}
			if !found {
				ev, found = q, true
			}
		}
	}
	for _, t := range []*mirror.Turn{st.Fold.Open, st.Fold.Last} {
		if t != nil && st.Live != "" && t.ID == st.Live {
			ev, found = mirror.Event{Turn: *t}, true
			break
		}
	}
	st.Queue = nil
	if !found {
		st.Live = ""
		return
	}
	ev.Kind, ev.Tries = mirror.Ended, 0
	ev.Turn.End, ev.Turn.Text = mirror.EndMirrorOff, ""
	st.Queue = []mirror.Event{ev}
}

// turnIDRe matches a turn id that names a claim file.
var turnIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// errMirrorWait holds a progress or Continued edit until mirrorEditGap has
// passed.
var errMirrorWait = errors.New("the mirror edit waits for the edit gap")

// mirrorSend sends the queue from its head and reports whether it was sent
// out. An Opened or an Ended with no claim posts the turn; a Progress of the
// live turn, and an Ended or Continued whose claim holds an event id, edit
// its post. An edit that waits for the gap stays queued and the events after
// it are sent. A send proved not sent keeps the event queued and backs the
// binding off. Any other failure leaves the outcome unknown: a post's claim
// stays with no event id and the turn is never sent again, a progress edit
// is dropped, and the final edit of a turn is tried once more after the
// backoff. Only an end moves the watermark.
func (s *Server) mirrorSend(dir string, t dialogTarget, b binding.Binding, st *mirrorFile, now time.Time) bool {
	for i := 0; i < len(st.Queue); {
		if s.mirrorStopped() {
			return false
		}
		ev := st.Queue[i]
		id := ev.Turn.ID
		claim := filepath.Join(dir, "posted", id+".json")
		final := ev.Kind == mirror.Ended || ev.Kind == mirror.Continued
		since := now.UnixMilli() - st.Edited
		ready := since < 0 || since >= mirrorEditGap.Milliseconds()
		var err error
		sent, edit := false, false
		switch {
		case !turnIDRe.MatchString(id):
		case ev.Kind == mirror.Opened && ev.Turn.LastTS > st.Watermark:
			var event string
			if event, err = s.mirrorPost(claim, t, b, ev.Turn); err == nil && event != "" {
				st.Live, sent = id, true
			}
		case id == st.Live && ev.Kind == mirror.Progress:
			sent, err = s.mirrorProgress(claim, ev.Turn, ready)
		case final && mirrorPosted(claim):
			edit = true
			sent, err = s.mirrorEdit(claim, t, b, ev.Turn, ready || ev.Kind == mirror.Ended)
		case ev.Kind == mirror.Ended && ev.Turn.LastTS > st.Watermark:
			var event string
			event, err = s.mirrorPost(claim, t, b, ev.Turn)
			sent = err == nil && event != ""
		}
		if errors.Is(err, errMirrorWait) {
			i++
			continue
		}
		var notSent *notSentError
		if errors.As(err, &notSent) || errors.Is(err, errNoBuzzIdentity) {
			s.mirrorBackOff(t.name, now)
			return false
		}
		if err != nil && edit && ev.Tries == 0 {
			st.Queue[i].Tries++
			_ = writeMirrorFile(dir, *st)
			s.mirrorBackOff(t.name, now)
			return false
		}
		st.Queue = slices.Delete(st.Queue, i, i+1)
		if sent {
			st.Edited = now.UnixMilli()
		}
		if final {
			st.Watermark = max(st.Watermark, ev.Turn.LastTS)
			if id == st.Live {
				st.Live = ""
			}
		}
		if writeMirrorFile(dir, *st) != nil {
			return false
		}
		if err != nil {
			s.mirrorBackOff(t.name, now)
			return false
		}
		delete(s.mirrors.backoff, t.name)
	}
	return len(st.Queue) == 0
}

// mirrorClaim is one turn's posted/<turn>.json: the post, the fingerprint
// of what it shows, whether it shows the turn interrupted, and the number of
// progress edits it had.
type mirrorClaim struct {
	dialogPost
	Shown  string `json:"shown,omitempty"`
	Halted bool   `json:"halted,omitempty"`
	Edits  int    `json:"edits,omitempty"`
}

func readMirrorClaim(path string) (mirrorClaim, bool) {
	raw, err := readSmallRegular(path)
	if err != nil {
		return mirrorClaim{}, false
	}
	var c mirrorClaim
	if json.Unmarshal(raw, &c) != nil {
		return mirrorClaim{}, false
	}
	return c, true
}

func writeMirrorClaim(path string, c mirrorClaim) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), raw, 0o600)
	return err
}

// mirrorPosted reports whether claim holds the event id of a post.
func mirrorPosted(claim string) bool {
	c, found := readMirrorClaim(claim)
	return found && c.Event != ""
}

// mirrorPost claims turn's post, posts it and records its event id. A turn
// already claimed is not posted. It returns the event id the claim holds.
func (s *Server) mirrorPost(claim string, t dialogTarget, b binding.Binding, turn mirror.Turn) (string, error) {
	if turn.End != "" {
		s.mirrorAnswered(t.name, b, &turn)
	}
	p := mirrorClaim{dialogPost: dialogPost{Channel: t.channel, Text: mirror.Render(turn)}, Shown: mirror.Fingerprint(turn), Halted: turn.Halted}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if won, err := createExclusive(claim, raw); err != nil || !won {
		if err == nil {
			had, _ := readMirrorClaim(claim)
			return had.Event, nil
		}
		return "", err
	}
	id, err := postAnswer(t.channel, p.Text, s.cfg.PostTimeout)
	var notSent *notSentError
	if errors.As(err, &notSent) || errors.Is(err, errNoBuzzIdentity) {
		_ = os.Remove(claim)
		return "", err
	}
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("buzz messages send gave no event id")
	}
	p.Event = id
	return id, writeMirrorClaim(claim, p)
}

// mirrorProgress edits the post of the running turn when its fingerprint
// changed and the turn had fewer than mirrorProgressMax progress edits, or
// when it was interrupted or runs again, and reports whether it edited it.
// Elapsed time alone is no change. An edit that is not ready waits.
func (s *Server) mirrorProgress(claim string, turn mirror.Turn, ready bool) (bool, error) {
	c, found := readMirrorClaim(claim)
	shown := mirror.Fingerprint(turn)
	capped := c.Edits >= mirrorProgressMax && turn.Halted == c.Halted
	if !found || c.Event == "" || capped || shown == c.Shown {
		return false, nil
	}
	if !ready {
		return false, errMirrorWait
	}
	text := mirror.Render(turn)
	if err := editMessage(c.Event, text, s.cfg.PostTimeout); err != nil {
		return false, err
	}
	c.Text, c.Shown, c.Halted, c.Edits = text, shown, turn.Halted, c.Edits+1
	return true, writeMirrorClaim(claim, c)
}

// mirrorEdit makes the edit of an ended turn's post and reports whether it
// edited it. A turn with no post, or one whose outcome is unknown, is not
// edited; an edit that is not ready waits. The edit that shows the mirror
// turned off is the post's last.
func (s *Server) mirrorEdit(claim string, t dialogTarget, b binding.Binding, turn mirror.Turn, ready bool) (bool, error) {
	p, found := readMirrorClaim(claim)
	if !found || p.Event == "" {
		return false, nil
	}
	if turn.End != "" {
		s.mirrorAnswered(t.name, b, &turn)
	}
	text := mirror.Render(turn)
	if text == p.Text {
		return false, nil
	}
	if !ready {
		return false, errMirrorWait
	}
	if err := editMessage(p.Event, text, s.cfg.PostTimeout); err != nil {
		return false, err
	}
	p.Text, p.Shown, p.Halted = text, mirror.Fingerprint(turn), turn.Halted
	if turn.End == mirror.EndMirrorOff {
		p.Event = ""
	}
	return true, writeMirrorClaim(claim, p)
}

// mirrorAnswered marks a mailbox binding's turn answered when the buzz inbox
// of its root holds a message from its handle created between the turn's
// first assistant line and its last line. The reply is a tool call inside
// the turn, so it is delivered before the closing message is written.
func (s *Server) mirrorAnswered(name string, b binding.Binding, turn *mirror.Turn) {
	if turn.Answered || !b.Mailbox() || turn.FirstAssistantTS == 0 {
		return
	}
	from, to := time.UnixMilli(turn.FirstAssistantTS), time.UnixMilli(turn.LastTS)
	if s.mirrors.scans == nil {
		s.mirrors.scans = map[string]mirrorScans{}
	}
	sc, ok := s.mirrors.scans[name]
	if !ok || sc.root != b.Root {
		sc = mirrorScans{root: b.Root, new: newInboxScan(fsq.AgentInboxNew(b.Root, mailboxSender)), cur: newInboxScan(fsq.AgentInboxCur(b.Root, mailboxSender))}
		s.mirrors.scans[name] = sc
	}
	for _, box := range []*inboxScan{sc.new, sc.cur} {
		if box.refresh() != nil {
			continue
		}
		for _, h := range box.headers {
			if h == nil || h.From != b.Handle {
				continue
			}
			if created, err := time.Parse(time.RFC3339Nano, h.Created); err == nil && !created.Before(from) && !created.After(to) {
				turn.Answered = true
				if turn.Cause == mirror.CauseMail {
					turn.Cause = mirror.CauseBuzz
				}
				return
			}
		}
	}
}

// mirrorUnavailable posts the note once for each locator that cannot be
// read. The note is recorded before it is posted, so it is posted at most
// once; a post proved not sent is tried again.
func (s *Server) mirrorUnavailable(dir string, st *mirrorFile, key string, off bool) {
	if off || st.Noted != nil && *st.Noted == key {
		return
	}
	was := st.Noted
	st.Noted = &key
	if writeMirrorFile(dir, *st) != nil {
		st.Noted = was
		return
	}
	_, err := postAnswer(st.Channel, mirrorNote, s.cfg.PostTimeout)
	var notSent *notSentError
	if errors.As(err, &notSent) || errors.Is(err, errNoBuzzIdentity) {
		st.Noted = was
	}
}

// mirrorBackOff delays a binding's next tick: 30 s, doubling to 10 min.
func (s *Server) mirrorBackOff(name string, now time.Time) {
	if s.mirrors.backoff == nil {
		s.mirrors.backoff = map[string]mirrorBackoff{}
	}
	bo := s.mirrors.backoff[name]
	bo.step = min(max(bo.step*2, mirrorBackoffMin), mirrorBackoffMax)
	bo.until = now.Add(bo.step)
	s.mirrors.backoff[name] = bo
}

// readMirrorFile returns dir's state and the bytes it was read from.
func readMirrorFile(dir string) (mirrorFile, []byte, bool) {
	raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(dir, "state.json"), 16<<20)
	if err != nil {
		return mirrorFile{}, nil, false
	}
	var st mirrorFile
	if json.Unmarshal(raw, &st) != nil {
		return mirrorFile{}, nil, false
	}
	return st, raw, true
}

func writeMirrorFile(dir string, st mirrorFile) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(dir, "state.json", raw, 0o600)
	return err
}

// Replies to the owner's mirror switch.
const (
	mirrorOffReply = "Mirror off for this session. Type `mirror on` to start it again."
	mirrorOnReply  = "Mirror on."
)

// mirrorSwitch reads an owner's message in a machine agent's channel that
// is exactly "mirror off" or "mirror on" (case ignored). It returns the
// channel's binding and the switch.
func (s *Server) mirrorSwitch(channel, prompt string) (name string, on bool, ok bool) {
	if !s.cfg.Machine || channel == "" || buzzOwner == "" || eventSender(prompt) != buzzOwner {
		return "", false, false
	}
	switch strings.ToLower(strings.TrimSpace(ownerWords(prompt))) {
	case "mirror off":
	case "mirror on":
		on = true
	default:
		return "", false, false
	}
	// Only a channel the mirror serves takes the switch: a live Claude
	// binding whose session this channel serves.
	r, found := s.channelRoute(channel)
	if !found {
		return "", false, false
	}
	if b, err := binding.ReadNamed(r.Name); err != nil || mirror.SourceFor(b.Harness) == nil || b.SessionIdentity() != r.Identity || !live(b) {
		return "", false, false
	}
	return r.Name, on, true
}

// setMirror turns the mirror of binding name on or off and returns the
// reply for the owner.
func setMirror(name string, on bool) string {
	err := mirror.SetOff(name, !on)
	switch {
	case err != nil:
		return "The mirror could not be switched: " + err.Error()
	case on:
		return mirrorOnReply
	}
	return mirrorOffReply
}

// answerMirrorSwitch switches the mirror, tells the owner, and ends the
// prompt without delivering it to the session.
func (s *Server) answerMirrorSwitch(sessionID, channel, name string, on bool, emit func(any) error) (any, *rpcError) {
	text := setMirror(name, on)
	meta := remoteMeta{State: "answered", Posted: s.publish(channel, text)}
	if err := emitText(emit, sessionID, "agent_message_chunk", text); err != nil {
		return nil, newRPCError(codeInternalError, "emit ACP reply update: %v", err)
	}
	return remotePromptResult{StopReason: StopReasonEndTurn, Meta: remotePromptMeta{Remote: meta}}, nil
}
