package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// lateReplyPosts caps the posts of one sweep; the rest wait for the next.
const lateReplyPosts = 8

// lateReplySweep is the late-reply sweep state of one server. One worker
// runs every sweep; the ticker and prompt starts only kick it, so at most
// one sweep waits behind a running one.
//
// One sweep costs: one listing read of at most lateReplyBudget entries of
// remote-events, a few small state reads per pending claim, one listing of
// buzz inbox/new and inbox/cur per queue root with an eligible claim, the
// headers of names not seen before, one pass over each root's cached
// headers, and at most lateReplyPosts posts. Pending claims are the ones
// this process made plus those found by the rolling listing, so a dead
// peer's claim is found within entries/lateReplyBudget sweeps. A claim
// leaves the set once it has an outcome or passes the horizon; a root's
// header cache is dropped when it has no eligible claim.
type lateReplySweep struct {
	kick chan struct{}

	pendingMu sync.Mutex
	pending   map[string]struct{}

	// Owned by the worker (or a test calling sweepLateReplies).
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

// startLateReplySweep runs the sweep worker until the returned stop is
// called; stop waits for a running sweep, so a post is never cut off.
func (s *Server) startLateReplySweep() (stop func()) {
	kick := make(chan struct{}, 1)
	s.mu.Lock()
	s.late.kick = kick
	s.mu.Unlock()
	done, quit := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(lateReplyInterval)
		defer tick.Stop()
		for {
			select {
			case <-quit:
				return
			case <-tick.C:
			case <-kick:
			}
			s.sweepLateReplies(time.Now())
		}
	}()
	return func() {
		close(quit)
		<-done
		s.mu.Lock()
		s.late.kick = nil
		s.mu.Unlock()
		if s.late.cursor != nil {
			_ = s.late.cursor.Close()
			s.late.cursor = nil
		}
	}
}

// requestSweep asks the worker for a sweep at a prompt start without
// holding the turn. A kick already waiting covers this one; without a
// running worker it does nothing.
func (s *Server) requestSweep() {
	s.mu.Lock()
	kick := s.late.kick
	s.mu.Unlock()
	if kick == nil {
		return
	}
	select {
	case kick <- struct{}{}:
	default:
	}
}

// sweepLateReplies posts the final reply of each recent mailbox event whose
// turn ended without one, for example on a timeout or a client that left
// (review F1). An event waits while its turn can still be open (younger
// than the turn timeout) or while it has no DM channel, and leaves the
// sweep once it has an outcome or passes the horizon. The post and a cancel
// both decide the event's outcome through one exclusive record, so exactly
// one of them happens and each reply is posted at most once.
func (s *Server) sweepLateReplies(now time.Time) {
	l := &s.late
	s.discoverClaims(now)
	byRoot := map[string]map[string]*lateClaim{}
	for _, eventID := range l.pendingIDs() {
		c, keep := s.loadLateClaim(eventID, now)
		switch {
		case !keep:
			l.drop(eventID)
		case c != nil:
			if byRoot[c.b.Root] == nil {
				byRoot[c.b.Root] = map[string]*lateClaim{}
			}
			byRoot[c.b.Root][c.claim.MessageID] = c
		}
	}
	scans := map[string][2]*inboxScan{}
	posts := 0
	for root, index := range byRoot {
		if posts == lateReplyPosts {
			break
		}
		sc, ok := l.scans[root]
		if !ok {
			sc = [2]*inboxScan{newInboxScan(fsq.AgentInboxNew(root, mailboxSender)), newInboxScan(fsq.AgentInboxCur(root, mailboxSender))}
		}
		scans[root] = sc
		if sc[0].refresh() != nil || sc[1].refresh() != nil {
			continue
		}
		newHits, curHits := matchClaims(sc[0], index), matchClaims(sc[1], index)
		for promptID, c := range index {
			if posts == lateReplyPosts {
				break
			}
			w := &mailboxWatch{stateDir: s.cfg.StateDir, b: c.b, threadID: c.claim.Thread, promptID: promptID, created: c.created, adopt: true}
			replyID, text, _, err := w.forward(newHits[promptID], curHits[promptID])
			if err != nil || text == "" {
				continue
			}
			posts++
			final, won, err := s.decideOutcome(c.eventID, mailboxOutcome{ReplyID: replyID})
			if err != nil {
				continue
			}
			if won && !final.Cancelled {
				_ = s.publish(c.channel, text)
			}
			l.drop(c.eventID)
		}
	}
	l.scans = scans // a root with no eligible claim loses its cache
}

// matchClaims makes one pass over a box's cached headers and returns, per
// prompt id in index, the replies to it, oldest first.
func matchClaims(sc *inboxScan, index map[string]*lateClaim) map[string][]replyHit {
	hits := map[string][]replyHit{}
	for name, header := range sc.headers {
		if header == nil {
			continue
		}
		for _, ref := range header.Refs {
			c, ok := index[ref]
			if !ok {
				continue
			}
			if hit, ok := replyTo(name, header, c.b.Handle, c.claim.Thread, c.created); ok {
				hits[ref] = append(hits[ref], hit)
			}
		}
	}
	for _, h := range hits {
		sortHits(h)
	}
	return hits
}

// discoverClaims lists the next lateReplyBudget entries of remote-events
// and adds the recent mailbox claims among them. The listing resumes where
// the last sweep stopped and starts over after its end.
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
