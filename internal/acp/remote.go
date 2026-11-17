package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	remoteturn "github.com/avivsinai/agent-message-queue/internal/remote/turn"
)

// remoteStateDir is amq-remote's state directory under the queue root
// (cmd/amq-remote stateDirName). The endpoint's owner-only socket lives there.
const remoteStateDir = "extensions/remote"

const (
	remoteNotSubmitted = remoteturn.StateNotSubmitted
	remoteUncertain    = remoteturn.StateUncertain
)

// Post kinds of an event's texts (remoteturn.PostStatus, remoteturn.PostFinal).
const (
	postStatus = remoteturn.PostStatus
	postFinal  = remoteturn.PostFinal
)

// runRemote submits the prompt to the pinned amq-remote target and holds the
// turn open until the request reaches a terminal or uncertain state
// (remoteturn.Turn). A cancel while queued is durable: a later delivery of
// the same event answers cancelled and submits nothing.
func (s *Server) runRemote(sessionID, text, eventID string, turn *turnState, emit func(any) error) (any, *rpcError) {
	if s.eventCancelled(eventID) {
		return remoteturn.Result{StopReason: StopReasonCancelled, Meta: remoteturn.ResultMeta{Remote: remoteturn.Meta{Reason: string(turnCancelled)}}}, nil
	}
	r := &remoteturn.Turn{
		Name:    "amq-acp",
		Dir:     filepath.Join(s.cfg.Root, remoteStateDir),
		Target:  s.cfg.RemoteTarget,
		Native:  s.cfg.RemoteNative,
		EventID: eventID,
		Timing:  s.timing(turn),
		Done:    turn.done,
		Settle:  s.settler(turn),
	}
	// A turn with no client shows nothing and relays no native activity.
	if emit != nil {
		r.Emit = func(update, text string) error { return emitText(emit, sessionID, update, text) }
		r.Mirror = func(update json.RawMessage) error {
			note := rawSessionUpdate{JSONRPC: jsonRPCVersion, Method: "session/update"}
			note.Params.SessionID, note.Params.Update = sessionID, update
			return emit(note)
		}
	}
	result, err := r.Run(text)
	if err != nil {
		return nil, newRPCError(codeInternalError, "%v", err)
	}
	if result.CancelledBeforeAdmit {
		if err := s.recordEventCancel(eventID); err != nil {
			return nil, newRPCError(codeInternalError, "record cancel: %v", err)
		}
	}
	return result, nil
}

// rawSessionUpdate is a session/update notification that carries one native
// session update as the endpoint's activity stream sent it.
type rawSessionUpdate struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	} `json:"params"`
}

func (s *Server) eventCancelPath(eventID string) (string, error) {
	if s.cfg.StateDir == "" || eventID == "" || eventID != filepath.Base(eventID) {
		return "", fmt.Errorf("cancel has no durable event record")
	}
	return filepath.Join(s.eventDir(), eventID+".cancelled"), nil
}

func (s *Server) eventCancelled(eventID string) bool {
	path, err := s.eventCancelPath(eventID)
	if err != nil {
		return false
	}
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// recordEventCancel records the durable cancel of a queued turn,
// <event>.cancelled. A prompt with no event id has nothing to record.
func (s *Server) recordEventCancel(eventID string) error {
	if eventID == "" {
		return nil
	}
	path, err := s.eventCancelPath(eventID)
	if err != nil {
		return err
	}
	_, err = createExclusive(path, []byte("cancelled\n"))
	return err
}

// sessionBinding is the binding this session drives: the one its channel
// route names (machineBinding).
func (s *Server) sessionBinding(sessionID string) (binding.Binding, error) {
	return s.machineBinding(sessionID)
}

// turnBinding fixes the binding a prompt runs under. A redelivered event
// follows the binding its first delivery recorded, never the current one,
// so a rebind between deliveries cannot run the event twice in two sessions.
// A first delivery records its binding before submit.
func (s *Server) turnBinding(sessionID, eventID string) (binding.Binding, error) {
	path := filepath.Join(s.eventDir(), eventID+".json")
	if raw, err := readSmallRegular(path); err == nil {
		var c eventClaim
		if err := json.Unmarshal(raw, &c); err != nil || c.Valid() != nil {
			return binding.Binding{}, fmt.Errorf("event %s has an unreadable recorded binding; refusing to resubmit", eventID)
		}
		if !s.ownsClaim(c) {
			return binding.Binding{}, fmt.Errorf("event %s was claimed by another Buzz agent; it runs there, not here", eventID)
		}
		// A claim can be visible before its directory entry is durable. Every
		// reader makes it durable before it submits, so a crash cannot lose
		// the claim behind a submission.
		if err := fsq.SyncDir(filepath.Dir(path)); err != nil {
			return binding.Binding{}, err
		}
		return c.binding(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return binding.Binding{}, err
	}
	b, err := s.sessionBinding(sessionID)
	if err != nil {
		return b, err
	}
	raw, err := json.Marshal(eventClaim{Binding: b, Name: b.Name, Agent: s.id.Agent})
	if err != nil {
		return binding.Binding{}, err
	}
	// The first delivery claims the event exclusively. A concurrent first
	// delivery that loses reads the winner's binding and never uses the one
	// it captured, so all deliveries of one event share one session.
	won, err := createExclusive(path, raw)
	if err != nil {
		return binding.Binding{}, err
	}
	if !won {
		return s.turnBinding(sessionID, eventID)
	}
	return b, nil
}

// eventClaim is an event's first-delivery record: the binding it runs
// under, and the Buzz agent that claimed it. Each agent keeps its claims in
// its own event directory (eventDir). A claim is checked before use: an
// agent follows and sweeps only a claim that names it (ownsClaim).
type eventClaim struct {
	binding.Binding
	Name  string `json:"name,omitempty"`
	Agent string `json:"agent,omitempty"`
	// Decided is the kind of owner decision (decidedMirror, decidedCommand,
	// decidedDialog) the event was taken as, or "" for a turn. A decided
	// event is never a turn, and its claim may carry no binding.
	Decided string `json:"decided,omitempty"`
}

func (c eventClaim) binding() binding.Binding {
	b := c.Binding
	b.Name = c.Name
	return b
}

// ownsClaim reports whether this agent made the claim. A claim that names no
// agent is no one's.
func (s *Server) ownsClaim(c eventClaim) bool {
	return c.Agent != "" && c.Agent == s.id.Agent
}

// eventsDir is remote-events, which holds the per-agent event directories
// and the event state of a process with no Buzz identity.
func (s *Server) eventsDir() string { return filepath.Join(s.cfg.StateDir, "remote-events") }

// eventDir is the directory of this process's event state. Each Buzz agent
// keeps its own, remote-events/<agent pubkey>/, because one Buzz event can
// reach several agents, and each handles it for itself. A process with no
// Buzz identity uses remote-events itself.
func (s *Server) eventDir() string {
	if s.id.Agent == "" {
		return s.eventsDir()
	}
	return filepath.Join(s.eventsDir(), s.id.Agent)
}

// createExclusive publishes raw at path only if nothing is there yet. The
// bytes are complete and synced before the link makes them visible, so a
// reader never sees a partial claim. It reports whether this call won.
func createExclusive(path string, raw []byte) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	return fsq.CreateNewFile(path, raw, 0o600)
}

// readSmallRegular reads a small regular file, refusing a symlink leaf.
func readSmallRegular(path string) ([]byte, error) {
	return fsq.ReadRegularNoFollowBounded(path, 64*1024)
}

// postOnce publishes text into channel at most once per event and kind, and
// never posts a status after the event's final text. Both go through the
// per-event post lock remote-events/<event>.post.lock:
//   - final: under the lock, reserve the final marker; release; then post.
//     The lock is not held across the network.
//   - status: under the lock, skip if the final marker exists or the status
//     was already posted; post to Buzz; then create the status marker;
//     release. While a status post holds the lock, no final can reserve.
//     Config.PostTimeout bounds the hold, and a crash releases the flock.
//
// A status post that failed is not retried: its key is made for one send
// (noteKey). A mailbox answer whose post was not sent is posted again by
// the late-reply sweep under the event's final key (finalUnsent). The
// event id is 64 lowercase hex, checked when the prompt is parsed (event.go
// eventIDsFromMeta) or the id of the owner's signed event, so it cannot
// leave the state dir. Without an event id
// it just posts; without a channel it posts nothing and records nothing.
func (s *Server) postOnce(eventID, kind string, record []byte, channel, text string) string {
	if eventID == "" || channel == "" || strings.TrimSpace(text) == "" {
		return s.publish(channel, text)
	}
	if kind == postFinal {
		won, err := s.reserveFinal(eventID, record)
		switch {
		case err != nil:
			return "error: record post marker: " + err.Error()
		case !won:
			return "duplicate: this event already posted its " + kind + " text"
		}
		return s.publishFinal(eventID, channel, text)
	}
	dir := s.eventDir()
	result := ""
	err := s.withPostLock(eventID, func() error {
		if _, final := s.mailboxAnswered(eventID); final {
			result = "superseded: this event already has its final text"
			return nil
		}
		marker := filepath.Join(dir, eventID+".posted."+kind)
		if _, err := os.Lstat(marker); err == nil {
			result = "duplicate: this event already posted its " + kind + " text"
			return nil
		}
		result = s.publish(channel, text)
		_, err := createExclusive(marker, record)
		return err
	})
	if err != nil {
		return "error: record post marker: " + err.Error()
	}
	return result
}

// reserveFinal creates the event's final marker exclusively under the post
// lock, so it never lands while a status post is in flight.
func (s *Server) reserveFinal(eventID string, record []byte) (bool, error) {
	won := false
	err := s.withPostLock(eventID, func() error {
		var err error
		won, err = createExclusive(filepath.Join(s.eventDir(), eventID+".posted."+postFinal), record)
		return err
	})
	return won, err
}

// withPostLock runs fn under the event's post lock.
func (s *Server) withPostLock(eventID string, fn func() error) error {
	dir := s.eventDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(filepath.Join(dir, eventID+".post.lock"), fn)
}
