package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

func clientSimple(args []string, op protocol.Op, positional string, probe ...*jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet(string(op), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	cmd := &protocol.Command{Schema: protocol.SchemaCommand, Op: op}
	if positional != "" {
		if len(pos) != 1 {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%s is required", positional)
		}
		cmd.TargetID = pos[0]
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	resp, err := ipc.Call(stateDir, ipc.Request{Command: cmd})
	if err != nil {
		return nil, 0, err
	}
	if err := resp.AsError(); err != nil {
		return nil, 0, err
	}
	if op == protocol.OpSessionList {
		var out []protocol.Session
		if err := json.Unmarshal(resp.Reply, &out); err != nil {
			return nil, 0, err
		}
		return out, 0, nil
	}
	var out protocol.Session
	if err := json.Unmarshal(resp.Reply, &out); err != nil {
		return nil, 0, err
	}
	return out, 0, nil
}

func inspectTarget(stateDir, target string) (protocol.Session, error) {
	resp, err := ipc.Call(stateDir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionInspect, TargetID: target}})
	if err != nil {
		return protocol.Session{}, err
	}
	if err := resp.AsError(); err != nil {
		return protocol.Session{}, err
	}
	var s protocol.Session
	if err := json.Unmarshal(resp.Reply, &s); err != nil {
		return protocol.Session{}, err
	}
	return s, nil
}

func callReply(stateDir string, cmd *protocol.Command) (protocol.Reply, error) {
	resp, err := ipc.Call(stateDir, ipc.Request{Command: cmd})
	if err != nil {
		return protocol.Reply{}, err
	}
	if err := resp.AsError(); err != nil {
		return protocol.Reply{}, err
	}
	var rep protocol.Reply
	if err := json.Unmarshal(resp.Reply, &rep); err != nil {
		return protocol.Reply{}, err
	}
	return rep, nil
}

// exitForOutcome maps a request Reply to the exit code. An op-specific
// Outcome.Code (request_conflict, already_resolved) is action-required and must
// not exit 0 just because the snapshot state reads running/completed; otherwise
// the exit follows the recorded state.
func exitForOutcome(rep protocol.Reply) int {
	if rep.Outcome.Code != "" {
		return protocol.ExitCode(&protocol.Refusal{Code: rep.Outcome.Code})
	}
	return exitForReply(rep.Snapshot, false)
}

// exitForReply maps a snapshot to the exit code. submit and status report the
// recorded state: a still-running request is success for them, a rejected or
// uncertain one is action required. wait uses ExitForState directly.
func exitForReply(snap protocol.Snapshot, waiting bool) int {
	if waiting {
		return protocol.ExitForState(snap.State)
	}
	switch snap.State {
	case protocol.StateRejected, protocol.StateUncertain:
		return protocol.ExitActionRequired
	case protocol.StateFailed, protocol.StateCancelled:
		return protocol.ExitError
	}
	return protocol.ExitSuccess
}

// isDuplicateConflict reports whether err is a request_conflict: a prior
// submit for this request id already reached the endpoint.
func isDuplicateConflict(err error) bool {
	var r *protocol.Refusal
	return errors.As(err, &r) && r.Code == protocol.CodeRequestConflict
}

// isEndpointUnreachable reports whether the endpoint is not running (the IPC
// socket is absent or the connection refused). The intent is persisted; the
// drainer replays after restart. ipc.Call returns a typed refusal with
// CodeEndpointUnreachable for a dial failure.
func isEndpointUnreachable(err error) bool {
	var r *protocol.Refusal
	return errors.As(err, &r) && r.Code == protocol.CodeEndpointUnreachable
}

// isNotFound reports whether the endpoint returned not_found for a request
// ref — the record does not exist in the request store (the submit may be
// spooled but not yet dispatched).
func isNotFound(err error) bool {
	var r *protocol.Refusal
	return errors.As(err, &r) && r.Code == protocol.CodeNotFound
}

// isInvalid reports whether the endpoint refused the request as invalid
// (e.g. a bare request ID passed where a ref is required). Status falls back
// to the spool on invalid too, so a bare request ID is resolved from the
// sender spool even while the companion is up.
func isInvalid(err error) bool {
	var r *protocol.Refusal
	return errors.As(err, &r) && r.Code == protocol.CodeInvalid
}
