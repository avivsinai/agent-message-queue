package main

import (
	"flag"
	"io"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

func cancel(args []string, probe ...*jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
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
	ref := pos[0]
	_, targetID, _, err := protocol.DecodeRef(ref)
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	session, err := inspectTarget(stateDir, targetID)
	if err != nil {
		return nil, 0, err
	}
	// Use the STORED request's epoch, not the current session epoch.
	// A fresh attachment generates a fresh epoch; using it to cancel a
	// request stored under an older epoch is rejected with stale_epoch.
	// Fetch the stored request to get the original epoch.
	epoch := session.Epoch
	getRep, gerr := callReply(stateDir, &protocol.Command{
		Schema:     protocol.SchemaCommand,
		Op:         protocol.OpRequestGet,
		RequestRef: ref,
	})
	if gerr != nil {
		return nil, 0, gerr
	}
	if getRep.Snapshot.Epoch != "" {
		epoch = getRep.Snapshot.Epoch
	}
	rep, err := callReply(stateDir, &protocol.Command{
		Schema:     protocol.SchemaCommand,
		Op:         protocol.OpRequestCancel,
		RequestRef: ref,
		TargetID:   targetID,
		Epoch:      epoch,
		NotAfter:   protocol.FormatTime(time.Now().Add(2 * time.Minute)),
	})
	if err != nil {
		return nil, 0, err
	}
	return rep, exitForOutcome(rep), nil
}
