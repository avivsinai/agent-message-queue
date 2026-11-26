package linkio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Handler is the endpoint's command entry point (core.Endpoint.Handle).
type Handler func(cmd *protocol.Command, src core.Source) (any, error)

// Bounds of the request dispatcher.
const (
	dispatchWorkers  = 4
	dispatchQueued   = 64
	busyRetryAfterMS = 2000
)

// job is one server request waiting for a worker.
type job struct {
	sess *session
	f    Frame
	key  string // request identity: commands for one request run in order
	run  func() (any, error)
	// admit, when set, runs under the dispatcher's lock once the job has a
	// place, and before any worker can run it; false refuses the job, which
	// then takes no place.
	admit func() bool
	// keep means the job must finish after disconnect. A named revoke earns
	// it on admission; an ordinary job earns it only when a worker claims it.
	keep bool
	// onFinish releases a retiring queue's binding after its last kept job.
	// The dispatcher lock orders registration with completion.
	onFinish func()
}

// dispatcher runs server requests on bounded workers, in arrival order per
// request, and never makes the reader wait: a full queue answers busy.
type dispatcher struct {
	ctx     context.Context
	mu      sync.Mutex
	queues  map[string][]*job // per request key, head is running or next
	ready   chan string       // keys whose head may run
	queued  int
	stopped bool
	// drained is closed once the dispatcher stopped and no job is queued or
	// running; the workers then return.
	drained  chan struct{}
	isClosed bool
}

func newDispatcher(ctx context.Context) *dispatcher {
	d := &dispatcher{ctx: ctx, queues: map[string][]*job{}, ready: make(chan string, dispatchQueued), drained: make(chan struct{})}
	for range dispatchWorkers {
		go d.work()
	}
	return d
}

// enqueue adds a job; false means the dispatcher is full, stopped, or the
// job's admit refused it.
func (d *dispatcher) enqueue(j *job) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil || d.queued >= dispatchQueued {
		return false
	}
	if j.admit != nil && !j.admit() {
		return false
	}
	d.queued++
	q := d.queues[j.key]
	d.queues[j.key] = append(q, j)
	if len(q) == 0 {
		d.ready <- j.key // cannot block: ready holds at most dispatchQueued keys
	}
	return true
}

// Each ready key has exactly one worker until that worker publishes its
// successor. Cancellation wakes idle workers even when there is no work.
func (d *dispatcher) work() {
	done := d.ctx.Done()
	for {
		select {
		case <-done:
			d.mu.Lock()
			d.stopped = true
			d.closeIfDrainedLocked()
			d.mu.Unlock()
			done = nil
		case <-d.drained:
			return
		case key := <-d.ready:
			d.runHead(key)
		}
	}
}

// runHead runs the head of key's queue and readies the next job of key.
func (d *dispatcher) runHead(key string) {
	d.mu.Lock()
	j := d.queues[key][0]
	// Receiving a ready key reserves the queue. Checking the connection
	// here claims its head; only a retained job can be claimed after cancel.
	if d.ctx.Err() == nil {
		j.keep = true
	}
	run := j.keep
	d.mu.Unlock()
	if run {
		out, err := j.run()
		answer(d.ctx, j.sess, j.f.ID, out, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queued--
	if j.onFinish != nil {
		j.onFinish()
	}
	if q := d.queues[key][1:]; len(q) > 0 {
		d.queues[key] = q
		d.ready <- key
	} else {
		delete(d.queues, key)
	}
	d.closeIfDrainedLocked()
}

func (d *dispatcher) closeIfDrainedLocked() {
	if d.stopped && len(d.queues) == 0 && !d.isClosed {
		d.isClosed = true
		close(d.drained)
	}
}

// codeTooLarge answers a request whose reply would exceed the link's frame
// limit.
const codeTooLarge = "too_large"

// answer replies to a server request: the reply body, or a typed refusal.
// A worker may wait for room on the data lane; the reader never does. The
// control lane stays for acknowledgements and the reader's own refusals.
// A reply over the connection's frame limit is answered with too_large
// instead: the server closes the socket on an oversize frame and would ask
// again after the reconnect.
func answer(ctx context.Context, sess *session, re string, out any, err error) {
	body := out
	if err != nil {
		e := ErrorBody{Code: string(protocol.CodeInvalid), Message: err.Error()}
		var lr *Refusal
		var pr *protocol.Refusal
		switch {
		case errors.As(err, &lr):
			e = ErrorBody{Code: lr.Code, Message: lr.Message}
		case errors.As(err, &pr):
			e = ErrorBody{Code: string(pr.Code), Message: pr.Message}
		}
		body = errorReply{Error: e}
	}
	frame, merr := replyFrame(sess, re, body)
	if merr != nil {
		return
	}
	if sess.frameLimit > 0 && len(frame) > sess.frameLimit {
		e := ErrorBody{Code: codeTooLarge, Message: fmt.Sprintf("the reply is %d bytes, over the link's %d-byte frame limit", len(frame), sess.frameLimit)}
		if frame, merr = replyFrame(sess, re, errorReply{Error: e}); merr != nil {
			return
		}
	}
	select {
	case sess.data <- frame:
	case <-ctx.Done():
	}
}

func replyFrame(sess *session, re string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Frame{Schema: SchemaFrame, Re: re, Gen: sess.gen, Body: raw})
}

// outcomeReply is the reply to a signed submit: AMQ's Outcome, never a
// revision.
type outcomeReply struct {
	Outcome protocol.Outcome `json:"outcome"`
}
