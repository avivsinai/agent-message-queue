package linkio

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Handler is the endpoint's command entry point (core.Endpoint.Handle).
type Handler func(cmd *protocol.Command, src core.Source) (any, error)

// Bounds of the request dispatcher (design §6 transport rules, §14).
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
}

// dispatcher runs server requests on bounded workers, in arrival order per
// request, and never makes the reader wait: a full queue answers busy.
type dispatcher struct {
	mu      sync.Mutex
	queues  map[string][]*job // per request key, head is running or next
	ready   chan string       // keys whose head may run
	queued  int
	stopped bool
}

func newDispatcher(ctx context.Context) *dispatcher {
	d := &dispatcher{queues: map[string][]*job{}, ready: make(chan string, dispatchQueued)}
	for range dispatchWorkers {
		go d.work(ctx)
	}
	return d
}

// enqueue adds a job; false means the dispatcher is full.
func (d *dispatcher) enqueue(j *job) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.queued >= dispatchQueued {
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

func (d *dispatcher) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			d.mu.Lock()
			d.stopped = true
			d.mu.Unlock()
			return
		case key := <-d.ready:
			d.mu.Lock()
			j := d.queues[key][0]
			d.mu.Unlock()
			out, err := j.run()
			answer(ctx, j.sess, j.f.ID, out, err)
			d.mu.Lock()
			d.queued--
			if q := d.queues[key][1:]; len(q) > 0 {
				d.queues[key] = q
				d.ready <- key
			} else {
				delete(d.queues, key)
			}
			d.mu.Unlock()
		}
	}
}

// answer replies to a server request: the reply body, or a typed refusal.
// A worker may wait for room on the data lane; the reader never does. The
// control lane stays for acknowledgements and the reader's own refusals.
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
	raw, merr := json.Marshal(body)
	if merr != nil {
		return
	}
	frame, merr := json.Marshal(Frame{Schema: SchemaFrame, Re: re, Gen: sess.gen, Body: raw})
	if merr == nil {
		select {
		case sess.data <- frame:
		case <-ctx.Done():
		}
	}
}

// outcomeReply is the reply to a signed submit: AMQ's Outcome, never a
// revision.
type outcomeReply struct {
	Outcome protocol.Outcome `json:"outcome"`
}
