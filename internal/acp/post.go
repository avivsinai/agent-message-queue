package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/buzzop"
)

// A machine agent's chat reply exists only if the agent publishes it, so the
// server posts every text the owner must read into the channel of the turn
// itself.

// defaultPostTimeout bounds one post (Config.PostTimeout).
const defaultPostTimeout = 30 * time.Second

// publish posts text for the owner when the turn came from Buzz. It reports
// the outcome for the result meta: "posted", "" when there is no Buzz
// channel, or the error.
func (s *Server) publish(channel, text string) string {
	result, _ := s.post(noteKey(), channel, text)
	return result
}

// finalPost is the outbound state of an event's final post, kept as a
// marker beside the final marker. accepted, uncertain and unsent are created
// once a post returns; a retry renames unsent to sending before it posts. A
// final marker with no unsent marker is a post in flight, interrupted, or
// made by an older build: never posted again.
type finalPost string

const (
	finalPostNone      finalPost = ""
	finalPostAccepted  finalPost = "accepted"
	finalPostUncertain finalPost = "uncertain"
	finalPostUnsent    finalPost = "unsent"
	// finalPostSending alone is a retry that crashed while it posted: in
	// doubt, never posted again.
	finalPostSending finalPost = "sending"
)

// publishFinal is publish for an event's final text: it also records what
// the post proves. Only a post the transport did not send is unsent, and
// the late-reply sweep may make it again (resendFinal) under the same key.
func (s *Server) publishFinal(eventID, channel, text string) string {
	s.stopWorking(channel)
	result, err := s.post(finalKey(eventID), channel, text)
	if eventID == "" || channel == "" || strings.TrimSpace(text) == "" {
		return result
	}
	return s.recordFinal(eventID, result, err)
}

// recordFinal records what a final post proves and returns its result.
func (s *Server) recordFinal(eventID, result string, err error) string {
	state := finalPostAccepted
	switch {
	case notSent(err):
		state = finalPostUnsent
	case err != nil:
		state = finalPostUncertain
	}
	if _, err := createExclusive(s.finalStatePath(eventID, state), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n")); err != nil {
		return result + "; record post state: " + err.Error()
	}
	return result
}

// post is publish that also returns the post's error. key names the post.
// A server with no transport never posts: its result is a post that was
// not sent.
func (s *Server) post(key buzzop.Key, channel, text string) (string, error) {
	if channel == "" || strings.TrimSpace(text) == "" {
		return "", nil
	}
	var err error
	if s.posts == nil {
		err = resultError(buzzop.Result{Outcome: buzzop.NotSent, Reason: "this server has no Buzz transport"})
	} else {
		err = resultError(s.posts.Send(context.Background(), buzzop.Message{Key: key, Channel: channel, Content: text}))
	}
	if err != nil {
		return "error: " + err.Error(), err
	}
	return "posted", nil
}

func (s *Server) finalStatePath(eventID string, state finalPost) string {
	return filepath.Join(s.eventDir(), eventID+".posted."+postFinal+"."+string(state))
}

// finalPostState reads the event's final-post state: accepted wins over
// uncertain, over unsent, over sending. A marker that cannot be read as
// present or absent makes the state uncertain, so it is never posted again.
func (s *Server) finalPostState(eventID string) finalPost {
	for _, state := range []finalPost{finalPostAccepted, finalPostUncertain, finalPostUnsent, finalPostSending} {
		_, err := os.Lstat(s.finalStatePath(eventID, state))
		switch {
		case err == nil:
			return state
		case !errors.Is(err, os.ErrNotExist):
			return finalPostUncertain
		}
	}
	return finalPostNone
}

// finalUnsent reports whether the event's final answer is a reply that was
// definitely never posted.
func (s *Server) finalUnsent(eventID string, out mailboxOutcome) bool {
	return out.ReplyID != "" && out.damaged == nil && s.finalPostState(eventID) == finalPostUnsent
}
