package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// lateReplyInterval is how often an open ACP stream sweeps for late replies.
const lateReplyInterval = 30 * time.Second

// lateReplyHorizon bounds the sweep to recent mailbox claims, so it never
// scans unbounded history.
const lateReplyHorizon = 24 * time.Hour

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
	}
}

// sweepLateReplies posts the final reply of each recent mailbox event whose
// turn ended without one, for example on a timeout or a client that left
// (review F1). An event is skipped while its turn can still be open (younger
// than the turn timeout), once it has a final-answer marker, or when it was
// cancelled. The marker is created before the post, so each event's reply is
// posted at most once. A failure skips that event until the next sweep. The
// buzz inbox scans are kept per root across sweeps, so a tick costs one
// listing of new and cur per root plus the headers of names not seen yet.
func (s *Server) sweepLateReplies(now time.Time) {
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	entries, err := os.ReadDir(filepath.Join(s.cfg.StateDir, "remote-events"))
	if err != nil {
		return
	}
	for _, e := range entries {
		eventID, ok := strings.CutSuffix(e.Name(), ".mailbox.json")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err != nil || now.Sub(info.ModTime()) > lateReplyHorizon {
			continue
		}
		s.sweepEvent(eventID, now)
	}
}

// sweepEvent posts one event's late final reply, if it has one. Caller
// holds sweepMu.
func (s *Server) sweepEvent(eventID string, now time.Time) {
	if s.eventCancelled(eventID) {
		return
	}
	if _, ok := s.mailboxAnswered(eventID); ok {
		return
	}
	dir := filepath.Join(s.cfg.StateDir, "remote-events")
	var claim mailboxClaim
	if raw, err := readSmallRegular(filepath.Join(dir, eventID+".mailbox.json")); err != nil || json.Unmarshal(raw, &claim) != nil {
		return
	}
	created, err := time.Parse(time.RFC3339Nano, claim.Created)
	if err != nil || claim.Channel == "" || claim.MessageID == "" || now.Before(created.Add(s.cfg.TurnTimeout)) {
		return
	}
	var b binding.Binding
	if raw, err := readSmallRegular(filepath.Join(dir, eventID+".json")); err != nil || json.Unmarshal(raw, &b) != nil || b.Valid() != nil || !b.Mailbox() {
		return
	}
	watch := s.watchMailbox(b, claim.Thread, claim.MessageID, created)
	watch.adopt = true
	if s.sweepScans == nil {
		s.sweepScans = map[string][2]*inboxScan{}
	}
	scans, ok := s.sweepScans[b.Root]
	if !ok {
		scans = [2]*inboxScan{watch.inboxNew, watch.inboxCur}
		s.sweepScans[b.Root] = scans
	}
	watch.inboxNew, watch.inboxCur = scans[0], scans[1]
	replyID, text, _, err := watch.poll(true)
	if err != nil || text == "" {
		return
	}
	raw, err := json.Marshal(mailboxReplied{ReplyID: replyID})
	if err != nil {
		return
	}
	s.postOnce(eventID, postFinal, raw, claim.Channel, text)
}
