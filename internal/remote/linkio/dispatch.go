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
	// keep makes the job run when the connection ends before it starts, in
	// its place behind the jobs of its key that still run. Every other job
	// not yet started then never runs.
	keep bool
}

// dispatcher runs server requests on bounded workers, in arrival order per
// request, and never makes the reader wait: a full queue answers busy.
type dispatcher struct {
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
	d := &dispatcher{queues: map[string][]*job{}, ready: make(chan string, dispatchQueued), drained: make(chan struct{})}
	for range dispatchWorkers {
		go d.work(ctx)
	}
	return d
}

// enqueue adds a job; false means the dispatcher is full, stopped, or the
// job's admit refused it.
func (d *dispatcher) enqueue(j *job) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.queued >= dispatchQueued {
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

// work runs heads of ready keys until the connection ends, then the jobs
// stop kept, until none is left.
func (d *dispatcher) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			d.stop()
			for {
				select {
				case <-d.drained:
					return
				case key := <-d.ready:
					d.runHead(ctx, key)
				}
			}
		case key := <-d.ready:
			d.runHead(ctx, key)
		}
	}
}

// runHead runs the head of key's queue and readies the next job of key.
func (d *dispatcher) runHead(ctx context.Context, key string) {
	d.mu.Lock()
	j := d.queues[key][0]
	d.mu.Unlock()
	out, err := j.run()
	answer(ctx, j.sess, j.f.ID, out, err)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queued--
	if q := d.queues[key][1:]; len(q) > 0 {
		d.queues[key] = q
		d.ready <- key
	} else {
		delete(d.queues, key)
	}
	d.closeIfDrainedLocked()
}

// stop ends the dispatcher once. A head a worker already took (its key is
// not in ready) finishes once. Of the jobs not started, those with keep stay
// in order behind it and run; every other one never runs.
func (d *dispatcher) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.stopped = true
	// A worker takes keys from ready without the lock, so the drain never
	// waits on it.
	idle := map[string]bool{}
drain:
	for {
		select {
		case key := <-d.ready:
			idle[key] = true
		default:
			break drain
		}
	}
	for key, q := range d.queues {
		var kept []*job
		if !idle[key] {
			kept, q = append(kept, q[0]), q[1:]
		}
		for _, j := range q {
			if j.keep {
				kept = append(kept, j)
			}
		}
		d.queued -= len(d.queues[key]) - len(kept)
		switch {
		case len(kept) == 0:
			delete(d.queues, key)
		case idle[key]:
			d.queues[key] = kept
			d.ready <- key // cannot block: ready was drained and holds at most dispatchQueued keys
		default:
			d.queues[key] = kept
		}
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
