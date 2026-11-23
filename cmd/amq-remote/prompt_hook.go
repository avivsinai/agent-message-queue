package main

import (
	"io"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
)

// promptHookBudget bounds the prompt hook's whole run: the read of its input
// and every call to an endpoint. The installed hook timeout is longer.
const promptHookBudget = 4 * time.Second

// promptHook is the receiver of AMQ's UserPromptSubmit hook for Claude Code.
// It decides nothing: it reports the prompt (its session, the permission
// mode the harness gave it, and the stub id when the prompt is a granted
// stub) to every endpoint that serves the session, and prints the text the
// first endpoint gives as the prompt's added context. It always returns 0
// and prints nothing on any error, so a prompt is never blocked.
func promptHook(home string, stdin io.Reader, stdout io.Writer) int {
	defer func() { _ = recover() }()
	deadline := time.Now().Add(promptHookBudget)
	read := make(chan claude.PromptHookInput, 1)
	go func() {
		defer func() { _ = recover() }()
		if in, ok := claude.ParsePromptHookInput(stdin); ok {
			read <- in
		}
		close(read)
	}()
	var in claude.PromptHookInput
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case got, ok := <-read:
		if !ok {
			return 0
		}
		in = got
	case <-timer.C:
		return 0
	}
	stub := in.StubID
	for _, ep := range claude.PromptEndpoints(home, in.SessionID) {
		left := time.Until(deadline)
		if left <= 0 {
			return 0
		}
		reply, err := ipc.Prompt(ep.StateDir, ipc.PromptRequest{
			TargetID: ep.Target, Instance: ep.Instance, NativeSession: in.SessionID,
			Mode: in.Mode, StubID: stub, WaitMS: int(left / time.Millisecond),
		}, left)
		if err != nil || reply.Text == "" || stub == "" {
			continue
		}
		_, _ = stdout.Write(claude.PromptHookOutput(stub, reply.Text))
		// The other endpoints still get the report, with no stub.
		stub = ""
	}
	return 0
}
