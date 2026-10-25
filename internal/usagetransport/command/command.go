// Package command runs one usage edge.Command as a child process: no shell,
// its own process group, bounded stdout, stdin held open until the reply is
// complete, and the whole group killed on cancellation. Run returns only
// after the child is reaped.
package command

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/usage"
	"github.com/avivsinai/agent-message-queue/usage/edge"
)

const (
	// doneGrace is how long a child may take to exit after its reply was
	// complete and its stdin closed; then its group is killed.
	doneGrace = 2 * time.Second
	// pipeCloseDelay bounds how long a descendant that outlived the child
	// may hold its output pipe open.
	pipeCloseDelay = 200 * time.Millisecond
)

var _ edge.Runner = Run

// Run runs c under parent. Stderr is discarded: it is free text that may
// carry anything. After the child is reaped, whatever is left of its
// process group is killed, however the child ended. Where the platform has
// no such group Run fails closed and starts nothing.
func Run(parent context.Context, c edge.Command) edge.CommandReply {
	if !groups {
		return edge.CommandReply{Failure: usage.FailUnsupportedPlatform, Err: errors.New("no process groups on this platform")}
	}
	path, err := exec.LookPath(c.Path)
	if err != nil {
		return edge.CommandReply{Failure: usage.FailNotInstalled, Err: err}
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, c.Args...)
	cmd.Env = append(os.Environ(), c.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return edge.CommandReply{Failure: usage.FailExit, Err: err}
	}
	var once sync.Once
	closeStdin := func() { once.Do(func() { _ = stdin.Close() }) }
	out := &watch{max: c.MaxOut, done: c.Done, overflow: cancel}
	out.onDone = func() {
		closeStdin()
		out.grace = time.AfterFunc(doneGrace, cancel)
	}
	cmd.Stdout = out
	ownGroup(cmd)
	cmd.WaitDelay = pipeCloseDelay
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return edge.CommandReply{Failure: usage.FailNotInstalled, Err: err}
		}
		return edge.CommandReply{Failure: usage.FailExit, Err: err}
	}
	go func() {
		_, _ = stdin.Write(c.Stdin)
		if c.Done == nil {
			closeStdin()
		}
	}()
	waitErr := cmd.Wait()
	groupErr := killGroup(cmd)
	stdout, over, done := out.result()

	reply := edge.CommandReply{Stdout: stdout}
	if st := cmd.ProcessState; st != nil && st.Exited() {
		reply.ExitCode = usage.Some(st.ExitCode())
	}
	switch {
	case groupErr != nil:
		reply.Failure, reply.Err = usage.FailExit, fmt.Errorf("%s: descendants not killed: %w", c.Path, groupErr)
	case over:
		reply.Failure, reply.Err = usage.FailOutputLimit, fmt.Errorf("%s: output over %d bytes", c.Path, c.MaxOut)
	case done:
		// The reply was complete; how the child exited after that does not
		// change it.
	case parent.Err() != nil:
		reply.Failure, reply.Err = usage.FailTimeout, fmt.Errorf("%s: %w", c.Path, parent.Err())
	case waitErr != nil:
		reply.Failure, reply.Err = usage.FailExit, fmt.Errorf("%s: %w", c.Path, waitErr)
	}
	return reply
}

// watch keeps up to max bytes of stdout and calls onDone once, the first
// time done reports true for what it holds. The first write past max marks
// it over and calls overflow; later writes are discarded so the child never
// blocks on a full pipe.
type watch struct {
	mu       sync.Mutex
	buf      []byte
	max      int
	over     bool
	finished bool
	done     func([]byte) bool
	overflow func()
	onDone   func()
	grace    *time.Timer
}

func (w *watch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.over {
		return len(p), nil
	}
	if len(w.buf)+len(p) > w.max {
		w.over, w.buf = true, nil
		w.overflow()
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	if !w.finished && w.done != nil && w.done(w.buf) {
		w.finished = true
		w.onDone()
	}
	return len(p), nil
}

func (w *watch) result() ([]byte, bool, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.grace != nil {
		w.grace.Stop()
	}
	return w.buf, w.over, w.finished
}
