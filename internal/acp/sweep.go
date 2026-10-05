package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
// than the turn timeout), once it has a .replied outcome, or when it was
// cancelled. The .replied record is created before the post, so each reply
// is posted at most once. A failure skips that event until the next sweep.
// Each eligible event costs one scan of buzz inbox/new and one of inbox/cur;
// eligible events are few, because each one either gets its reply or ages
// out of the horizon.
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

// sweepEvent posts one event's late final reply, if it has one.
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
	// Claim new replies into cur, then read cur: it also holds a reply a
	// turn claimed but never posted.
	if _, _, _, err := mailboxReplies(b, claim.Thread, claim.MessageID, created, map[string]bool{}); err != nil {
		return
	}
	replyID, text := claimedReply(b, claim.Thread, claim.MessageID, created)
	if text == "" {
		return
	}
	raw, err := json.Marshal(mailboxReplied{ReplyID: replyID})
	if err != nil {
		return
	}
	s.postMarked(eventID, ".replied", raw, claim.Channel, text)
}

// claimedReply returns the newest final reply to the prompt in buzz
// inbox/cur, with its message id.
func claimedReply(b binding.Binding, threadID, promptID string, created time.Time) (string, string) {
	dir := fsq.AgentInboxCur(b.Root, mailboxSender)
	hits, err := scanReplies(dir, b.Handle, threadID, promptID, created)
	if err != nil {
		return "", ""
	}
	for i := len(hits) - 1; i >= 0; i-- {
		if hits[i].header.Kind == string(format.KindStatus) {
			continue
		}
		msg, err := format.ReadMessageFile(filepath.Join(dir, hits[i].filename))
		if body := strings.TrimSpace(msg.Body); err == nil && body != "" {
			return strings.TrimSuffix(hits[i].filename, ".md"), body
		}
	}
	return "", ""
}
