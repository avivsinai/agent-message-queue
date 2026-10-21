package main

import (
	"flag"
	"io"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
	"github.com/avivsinai/agent-message-queue/internal/remote/sender"
)

func listRequests(args []string, probe ...*jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("requests", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	limit := fs.Int("limit", 20, "most recent records to show")
	if err := fs.Parse(args); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	store, err := requests.OpenReadOnly(stateDir)
	if err != nil {
		return nil, 0, err
	}
	recs, err := store.List()
	if err != nil {
		return nil, 0, err
	}
	if *limit > 0 && len(recs) > *limit {
		recs = recs[len(recs)-*limit:]
	}
	out := make([]protocol.Snapshot, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Snapshot)
	}
	// Include ALL spool envelopes (pending, failed, expired, dispatched)
	// that have no request-store record yet, with their actual terminal state
	// and last_error. Submit persisted while the companion was down; the
	// caller must see failures, not just pending. Dedup against existing
	// records so the same request doesn't appear twice.
	if spool, err := sender.Open(stateDir); err == nil {
		if envs, err := spool.List(); err == nil {
			seen := make(map[string]bool, len(recs))
			for _, r := range recs {
				seen[r.RequestID] = true
			}
			for _, env := range envs {
				if seen[env.RequestID] {
					continue
				}
				state := protocol.StateReceived
				switch env.State {
				case sender.StateFailed:
					state = protocol.StateFailed
				case sender.StateDispatched:
					state = protocol.StateDispatching
				case sender.StateExpired:
					state = protocol.StateRejected
				}
				snap := protocol.Snapshot{
					Schema:      "sender_submitted",
					RequestID:   env.RequestID,
					CreatorHost: env.CreatorHost,
					TargetID:    env.TargetID,
					Epoch:       env.Epoch,
					State:       state,
					InputDigest: env.InputDigest,
					NotAfter:    env.NotAfter,
					ObservedAt:  env.CreatedAt,
				}
				if env.LastError != "" {
					snap.Code = protocol.Code(env.LastError)
				}
				if env.State == sender.StateExpired && snap.Code == "" {
					snap.Code = protocol.CodeExpired
				}
				out = append(out, snap)
			}
		}
	}
	return out, 0, nil
}
