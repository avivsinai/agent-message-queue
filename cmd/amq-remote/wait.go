package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/signal"

	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

func wait(args []string, probe ...*jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	timeout := fs.Duration("timeout", 0, "give up after this long; the work continues (0 = no limit)")
	pos, perr := parseInterleaved(fs, args)
	if perr != nil || len(pos) != 1 {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "REQUEST_REF is required")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	type outcome struct {
		resp *ipc.Response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := ipc.Call(stateDir, ipc.Request{Wait: &ipc.WaitRequest{RequestRef: pos[0], TimeoutMS: timeout.Milliseconds()}})
		done <- outcome{resp, err}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	select {
	case <-sig:
		return nil, protocol.ExitInterrupted, protocol.Refuse(protocol.CodeUnsupported, "wait interrupted; the request keeps running")
	case o := <-done:
		if o.err != nil {
			return nil, 0, o.err
		}
		if err := o.resp.AsError(); err != nil {
			return nil, 0, err
		}
		var snap protocol.Snapshot
		if err := json.Unmarshal(o.resp.Reply, &snap); err != nil {
			return nil, 0, err
		}
		if o.resp.TimedOut {
			return snap, protocol.ExitTimeout, nil
		}
		return snap, exitForReply(snap, true), nil
	}
}
