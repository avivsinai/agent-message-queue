// Drainer replays pending sender spool envelopes through the endpoint after a
// restart (and on each tick while the companion runs). It is the recovery
// half of the durable sender: the CLI persists an envelope before returning
// `submitted`; the drainer hands the exact command bytes to Endpoint.Handle,
// retrying the same identity until the endpoint accepts it or the admission
// window closes.
//
// The drainer does NOT own the request record. Once Endpoint.Handle returns a
// non-transient reply, the endpoint owns the record and the spool envelope is
// settled (MarkDispatched) for reaping. A transient failure (endpoint
// unreachable, draining, storage_full) leaves the envelope pending and the
// drainer retries on the next tick.
//
// An envelope whose NotAfter has passed is expired WITHOUT dispatch — the
// caller is told the request expired before the endpoint could admit it,
// never that it was dispatched late.
package sender

import (
	"context"
	"errors"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Dispatcher is the interface the drainer dispatches through. Endpoint
// satisfies it. It is an interface so the drainer can be tested without a
// live endpoint.
type Dispatcher interface {
	Handle(cmd *protocol.Command, src core.Source) (any, error)
}

// Drainer replays pending envelopes through a Dispatcher.
type Drainer struct {
	spool *Spool
	ep    Dispatcher
	now   func() time.Time
}

// NewDrainer returns a drainer that replays pending envelopes in spool through
// ep. now defaults to time.Now.
func NewDrainer(spool *Spool, ep Dispatcher, now func() time.Time) *Drainer {
	if now == nil {
		now = time.Now
	}
	return &Drainer{spool: spool, ep: ep, now: now}
}

// Drain replays every pending envelope once. It is safe to call on each tick:
// a settled envelope is skipped, a pending one is dispatched or marked with a
// transient/terminal failure. Returns the number of envelopes acted on
// (dispatched, expired, or failed).
//
// A per-envelope error never aborts the sweep: one bad envelope must not stop
// another's recovery (mirrors the carrier's ImportOnce contract).
func (d *Drainer) Drain(_ context.Context) (int, error) {
	envs, err := d.spool.List()
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, env := range envs {
		if env.State != StatePending {
			continue
		}
		k := env.key()
		// Expire first: an envelope whose window closed while it waited is
		// expired WITHOUT dispatch. The caller is told the request expired
		// before admission, never that it was dispatched late.
		if expired, err := d.spool.Expire(k, d.now()); err != nil {
			errs = append(errs, err)
			continue
		} else if expired {
			n++
			continue
		}
		n++
		reply, herr := d.ep.Handle(env.Command, core.Source{Host: env.CreatorHost, Origin: env.Origin})
		// B2: classify by Outcome.Code, NOT by error. The endpoint returns
		// busy, request_conflict, expired, stale_epoch, unshared, invalid and
		// unsupported as a Reply with Outcome.Code and a nil error. Checking
		// herr == nil alone marks refused envelopes as dispatched.
		code := classifyReply(reply, herr)
		switch {
		case code == "":
			// No code = the endpoint accepted the command and owns the record.
			if err := d.spool.MarkDispatched(k); err != nil {
				errs = append(errs, err)
			}
		case isTransientCode(code):
			// Busy / draining / unreachable / storage_full: retry next tick.
			if err := d.spool.MarkAttempt(k, string(code)); err != nil {
				errs = append(errs, err)
			}
		default:
			// Terminal refusal (expired, stale_epoch, unshared,
			// request_conflict, invalid, unsupported): no retry will succeed.
			if err := d.spool.MarkFailed(k, string(code)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return n, errors.Join(errs...)
}

// classifyReply extracts the Outcome.Code from the endpoint's reply. The
// endpoint returns refusals as (Reply, nil) with Outcome.Code set; a nil
// error with no code means success. A non-nil error that is not a typed
// protocol refusal is a transient IPC/network failure (retry). A typed
// refusal error (from a path that returns (nil, error)) is mapped to its
// code.
func classifyReply(reply any, herr error) protocol.Code {
	if herr != nil {
		var r *protocol.Refusal
		if errors.As(herr, &r) {
			return r.Code
		}
		// Not a protocol refusal (network/IPC error): transient.
		return protocol.CodeEndpointUnreachable
	}
	if reply == nil {
		return ""
	}
	// The endpoint returns a protocol.Reply VALUE (not a pointer);
	// extract its Outcome.Code. A pointer assertion never matches
	// (round-3 B2: dead code — every refusal became MarkDispatched).
	if rep, ok := reply.(protocol.Reply); ok {
		return rep.Outcome.Code
	}
	return ""
}

// isTransientCode reports whether a dispatch outcome code is worth retrying.
// Busy is NOT transient (round-3 item 4): busy=reject settles as a refusal
// per the ADR; replay only via explicit user resubmit, never automatic.
func isTransientCode(code protocol.Code) bool {
	switch code {
	case protocol.CodeDraining,
		protocol.CodeEndpointUnreachable,
		protocol.CodeStorageFull:
		return true
	}
	return false
}
