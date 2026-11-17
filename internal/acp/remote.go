package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	remoteturn "github.com/avivsinai/agent-message-queue/internal/remote/turn"
)

// remoteStateDir is amq-remote's state directory under the queue root
// (cmd/amq-remote stateDirName). The endpoint's owner-only socket lives there.
const remoteStateDir = "extensions/remote"

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

// eventDir is the directory of this server's event state, remote-events.
func (s *Server) eventDir() string { return filepath.Join(s.cfg.StateDir, "remote-events") }

// createExclusive publishes raw at path only if nothing is there yet. The
// bytes are complete and synced before the link makes them visible, so a
// reader never sees a partial claim. It reports whether this call won.
func createExclusive(path string, raw []byte) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	return fsq.CreateNewFile(path, raw, 0o600)
}
