package main

import (
	"flag"
	"io"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/sender"
)

func status(args []string, probe ...*jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	pos, perr := parseInterleaved(fs, args)
	if perr != nil || len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "REQUEST_REF is required")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	// A bare request ID (not a ref) resolves through the sender spool: a
	// running endpoint refuses a bare id as invalid. The spool's stored
	// identity names the endpoint record, and the endpoint is asked first
	// whatever the spool state: the spool records delivery of intent, the
	// endpoint records execution, so a record the endpoint holds wins over a
	// failed or expired envelope. The spool receipt answers only when the
	// endpoint has no record or cannot be reached.
	refOrID := pos[0]
	if !strings.HasPrefix(refOrID, protocol.RefPrefix) {
		if spoolReceipt, ok := lookupSpoolStatus(stateDir, refOrID); ok {
			refOrID = protocol.EncodeRef(spoolReceipt.CreatorHost, spoolReceipt.TargetID, spoolReceipt.RequestID)
		}
	}
	rep, err := callReply(stateDir, &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpRequestGet, RequestRef: refOrID})
	if err != nil {
		// If the endpoint is unreachable or the record is not found,
		// check the sender spool. A submit persisted while the companion
		// was down has no request-store record; status must still tell the
		// caller the truth (pending/dispatched/expired/failed).
		if isEndpointUnreachable(err) || isNotFound(err) || isInvalid(err) {
			if spoolReceipt, ok := lookupSpoolStatus(stateDir, pos[0]); ok {
				return spoolReceipt, exitForSpoolReceipt(spoolReceipt), nil
			}
		}
		return nil, 0, err
	}
	return rep, exitForOutcome(rep), nil
}

// lookupSpoolStatus checks the sender spool for a request ref OR a raw
// request ID and returns a SpoolReceipt describing the envelope's state.
// Status takes the envelope id directly as a positional argument (no
// --request-id flag) because spooled submits mint no refs.
// Returns (zero, false) when no envelope matches.
func lookupSpoolStatus(stateDir, refOrID string) (sender.SpoolReceipt, bool) {
	var creatorHost, requestID string
	if h, _, id, err := protocol.DecodeRef(refOrID); err == nil {
		creatorHost, requestID = h, id
	} else {
		// Not a ref — treat as a raw request ID and scan the spool.
		requestID = refOrID
	}
	spool, err := sender.Open(stateDir)
	if err != nil {
		return sender.SpoolReceipt{}, false
	}
	if creatorHost != "" {
		env, exists, err := spool.Get(creatorHost, requestID)
		if err != nil || !exists {
			return sender.SpoolReceipt{}, false
		}
		return env.Receipt("spool_lookup"), true
	}
	// No creator host: scan all envelopes for the request ID.
	envs, err := spool.List()
	if err != nil {
		return sender.SpoolReceipt{}, false
	}
	for _, env := range envs {
		if env.RequestID == requestID {
			return env.Receipt("spool_lookup"), true
		}
	}
	return sender.SpoolReceipt{}, false
}

// exitForSpoolReceipt maps a spool envelope state to an exit code. A
// pending/dispatched envelope is success (the work is in flight). A
// failed/expired envelope is ExitError: native work did not happen (or its
// window closed) and the caller takes the failure path.
func exitForSpoolReceipt(r sender.SpoolReceipt) int {
	switch r.State {
	case sender.StateFailed, sender.StateExpired:
		return protocol.ExitError
	}
	return protocol.ExitSuccess
}
