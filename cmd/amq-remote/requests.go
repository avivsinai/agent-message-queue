package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
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
	if fs.NArg() > 0 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "requests takes no positional arguments (got %q)", fs.Arg(0))
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	store, err := requests.OpenReadOnly(stateDir)
	if err != nil {
		return nil, 0, err
	}
	recs, poison, err := store.ListWithPoison()
	if err != nil {
		return nil, 0, err
	}
	if *limit > 0 && len(recs) > *limit {
		recs = recs[len(recs)-*limit:]
	}
	out := make([]any, 0, len(recs)+len(poison))
	for _, r := range recs {
		out = append(out, r.Snapshot)
	}
	// A damaged record is always listed: while it stays unreadable,
	// admission refuses submits to its target.
	for _, p := range poison {
		out = append(out, damagedRequest{Schema: "request_damaged", RequestID: p.Key.RequestID, CreatorHost: p.Key.CreatorHost, TargetID: p.Key.TargetID, Path: p.Path, Error: p.Error})
	}
	// Include ALL spool envelopes (pending, failed, expired, dispatched)
	// that have no request-store record yet, with their actual terminal state
	// and last_error. Submit persisted while the companion was down; the
	// caller must see failures, not just pending. Dedup against existing
	// records so the same request doesn't appear twice.
	if spool, err := sender.Open(stateDir); err == nil {
		if envs, err := spool.List(); err == nil {
			seen := make(map[string]bool, len(recs)+len(poison))
			for _, r := range recs {
				seen[r.RequestID] = true
			}
			for _, p := range poison {
				seen[p.Key.RequestID] = true
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

// damagedRequest is a request record that cannot be read or decoded. Its
// identity comes from the file name; the rest is lost.
type damagedRequest struct {
	Schema      string `json:"schema"`
	RequestID   string `json:"request_id,omitempty"`
	CreatorHost string `json:"creator_host"`
	TargetID    string `json:"target_id,omitempty"`
	Path        string `json:"path"`
	Error       string `json:"error"`
}

// releaseWarning is printed before every release: releasing stops nothing in
// the session.
const releaseWarning = "amq-remote: warning: the request's input may still reach the session and run there as a turn that no request owns"

// releaseRequest ends one uncertain request failed with released_by_owner
// through the running endpoint, so its target admits new work. A caller
// without a terminal must pass --yes; with one, it asks once.
func releaseRequest(args []string, stdin io.Reader, stderr io.Writer, probe ...*jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("requests release", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	yes := fs.Bool("yes", false, "release without asking")
	pos, perr := parseInterleaved(fs, args)
	if perr != nil || len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "REQUEST_REF is required")
	}
	ref := pos[0]
	if _, _, _, err := protocol.DecodeRef(ref); err != nil {
		return nil, protocol.ExitUsage, err
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	say(stderr, "%s", releaseWarning)
	if !*yes {
		if !stdinIsTerminal(stdin) {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--yes is required when stdin is not a terminal")
		}
		_, _ = fmt.Fprintf(stderr, "Release %s? [y/N]: ", ref)
		line, rerr := bufio.NewReader(stdin).ReadString('\n')
		if rerr != nil && line == "" {
			return nil, protocol.ExitError, errors.New("release not confirmed; nothing changed")
		}
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			return nil, protocol.ExitError, errors.New("release not confirmed; nothing changed")
		}
	}
	resp, err := ipc.Call(stateDir, ipc.Request{Release: &ipc.ReleaseRequest{RequestRef: ref}})
	if err != nil {
		return nil, 0, err
	}
	if err := resp.AsError(); err != nil {
		return nil, 0, err
	}
	var rep protocol.Reply
	if err := json.Unmarshal(resp.Reply, &rep); err != nil {
		return nil, 0, err
	}
	// The release succeeded, though the record now reads failed.
	return rep, 0, nil
}
