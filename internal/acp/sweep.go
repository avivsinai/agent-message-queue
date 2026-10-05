package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// lateReplyInterval is how often an open ACP stream sweeps for late replies.
const lateReplyInterval = 30 * time.Second

// lateReplyHorizon bounds the sweep to recent mailbox claims, so it never
// scans unbounded history.
const lateReplyHorizon = 24 * time.Hour

// lateReplyBudget is how many remote-events entries one sweep lists to
// discover claims this process did not make.
const lateReplyBudget = 512

// lateReplySweep is the late-reply sweep state of one server.
//
// One sweep costs: one listing read of at most lateReplyBudget entries of
// remote-events, a few small state reads per pending claim, one listing of
// buzz inbox/new and inbox/cur per queue root with an eligible claim, and
// the headers of names not seen by an earlier sweep. Pending claims are the
// ones this process made plus those discovered by the rolling listing, so a
// dead peer's claim is found within entries/lateReplyBudget sweeps. A claim
// leaves the set once it has an outcome, is cancelled, or passes the
// horizon.
type lateReplySweep struct {
	// mu is held for a whole sweep; queued coalesces prompt-triggered
	// sweeps, so at most one waits behind a running one.
	mu      sync.Mutex
	queued  atomic.Bool
	running sync.WaitGroup

	pendingMu sync.Mutex
	pending   map[string]struct{}

	// Guarded by mu.
	cursor *os.File
	scans  map[string][2]*inboxScan
}

// add records a mailbox event this process claimed.
func (l *lateReplySweep) add(eventID string) {
	if eventID == "" {
		return
	}
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if l.pending == nil {
		l.pending = map[string]struct{}{}
	}
	l.pending[eventID] = struct{}{}
}

func (l *lateReplySweep) drop(eventID string) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	delete(l.pending, eventID)
}

func (l *lateReplySweep) pendingIDs() []string {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	ids := make([]string, 0, len(l.pending))
	for id := range l.pending {
		ids = append(ids, id)
	}
	return ids
}

// startLateReplySweep sweeps on a slow tick until the returned stop is
// called; stop waits for a running sweep, so a post is never cut off.
func (s *Server) startLateReplySweep() (stop func()) {
	done, quit := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(lateReplyInterval)
		defer tick.Stop()
		for {
			select {
			case <-quit:
				return
			case now := <-tick.C:
				s.sweepLateReplies(now)
			}
		}
	}()
	return func() {
		close(quit)
		<-done
		s.late.mu.Lock()
		defer s.late.mu.Unlock()
		if s.late.cursor != nil {
			_ = s.late.cursor.Close()
			s.late.cursor = nil
		}
	}
}

// requestSweep starts a sweep at a prompt start without holding the turn.
// A sweep already queued covers this request.
func (s *Server) requestSweep() {
	if !s.late.queued.CompareAndSwap(false, true) {
		return
	}
	s.late.running.Add(1)
	go func() {
		defer s.late.running.Done()
		s.sweepLateReplies(time.Now())
	}()
}

// sweepLateReplies posts the final reply of each recent mailbox event whose
// turn ended without one, for example on a timeout or a client that left
// (review F1). An event waits while its turn can still be open (younger
// than the turn timeout) and leaves the sweep once it has an outcome, is
// cancelled, or passes the horizon. The final post and a cancel both take
// the exclusive final-answer marker, so exactly one of them happens, and
// each event's reply is posted at most once.
func (s *Server) sweepLateReplies(now time.Time) {
	l := &s.late
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queued.Store(false)
	s.discoverClaims(now)
	byRoot := map[string][]lateClaim{}
	for _, eventID := range l.pendingIDs() {
		c, keep := s.loadLateClaim(eventID, now)
		switch {
		case !keep:
			l.drop(eventID)
		case c != nil:
			byRoot[c.b.Root] = append(byRoot[c.b.Root], *c)
		}
	}
	if l.scans == nil {
		l.scans = map[string][2]*inboxScan{}
	}
	for root, claims := range byRoot {
		scans, ok := l.scans[root]
		if !ok {
			scans = [2]*inboxScan{newInboxScan(fsq.AgentInboxNew(root, mailboxSender)), newInboxScan(fsq.AgentInboxCur(root, mailboxSender))}
			l.scans[root] = scans
		}
		if scans[0].refresh() != nil || scans[1].refresh() != nil {
			continue
		}
		for _, c := range claims {
			w := &mailboxWatch{stateDir: s.cfg.StateDir, b: c.b, threadID: c.claim.Thread, promptID: c.claim.MessageID, created: c.created, inboxNew: scans[0], inboxCur: scans[1], adopt: true, refreshed: true}
			replyID, text, _, err := w.poll(true)
			if err != nil || text == "" {
				continue
			}
			raw, err := json.Marshal(mailboxOutcome{ReplyID: replyID})
			if err != nil {
				continue
			}
			s.postOnce(c.eventID, postFinal, raw, c.channel, text)
			l.drop(c.eventID)
		}
	}
}

// discoverClaims lists the next lateReplyBudget entries of remote-events
// and adds the recent mailbox claims among them. The listing resumes where
// the last sweep stopped and starts over after its end. Caller holds mu.
func (s *Server) discoverClaims(now time.Time) {
	l := &s.late
	if l.cursor == nil {
		f, err := os.Open(filepath.Join(s.cfg.StateDir, "remote-events"))
		if err != nil {
			return
		}
		l.cursor = f
	}
	entries, err := l.cursor.ReadDir(lateReplyBudget)
	for _, e := range entries {
		eventID, ok := strings.CutSuffix(e.Name(), ".mailbox.json")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) <= lateReplyHorizon {
			l.add(eventID)
		}
	}
	if err != nil || len(entries) < lateReplyBudget {
		_ = l.cursor.Close()
		l.cursor = nil
	}
}

// lateClaim is one mailbox event the sweep may post for.
type lateClaim struct {
	eventID, channel string
	claim            mailboxClaim
	created          time.Time
	b                binding.Binding
}

// loadLateClaim returns the event's claim when the sweep may post for it
// now; keep is false when the event leaves the sweep for good.
func (s *Server) loadLateClaim(eventID string, now time.Time) (*lateClaim, bool) {
	if s.eventCancelled(eventID) {
		return nil, false
	}
	if _, ok := s.mailboxAnswered(eventID); ok {
		return nil, false
	}
	dir := filepath.Join(s.cfg.StateDir, "remote-events")
	c := lateClaim{eventID: eventID}
	if raw, err := readSmallRegular(filepath.Join(dir, eventID+".mailbox.json")); err != nil || json.Unmarshal(raw, &c.claim) != nil || c.claim.MessageID == "" {
		return nil, false
	}
	created, err := time.Parse(time.RFC3339Nano, c.claim.Created)
	if err != nil || now.Sub(created) > lateReplyHorizon {
		return nil, false
	}
	c.created = created
	if now.Before(created.Add(s.cfg.TurnTimeout)) {
		return nil, true // its turn can still be open
	}
	if raw, err := readSmallRegular(filepath.Join(dir, eventID+".json")); err != nil || json.Unmarshal(raw, &c.b) != nil || c.b.Valid() != nil || !c.b.Mailbox() {
		return nil, false
	}
	// No delivery named a channel yet: a later one may record it.
	if c.channel = s.eventChannel(eventID, c.claim.Channel, ""); c.channel == "" {
		return nil, true
	}
	return &c, true
}
