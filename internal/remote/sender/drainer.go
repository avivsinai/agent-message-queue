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
// The endpoint alone decides whether an envelope's window has closed. The
// drainer never expires an envelope on its own clock: a crash after the
// endpoint accepted the command but before the envelope was settled leaves a
// pending envelope whose request already ran, and only the endpoint knows
// that. Replaying it returns the endpoint's existing record (settled as
// dispatched); an envelope the endpoint never saw is refused as expired
// without dispatch (settled as expired).
package sender

import (
	"context"
	"errors"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// drainReconcile is the longest a drainer goes without a full read of the
// spool.
const drainReconcile = time.Minute

// Drainer replays pending envelopes through the endpoint.
//
// It keeps the keys of the pending envelopes its last full read of the
// spool found, with the version of the spool directory taken before that
// read. Every write to the spool, by any process, renames a file into the
// directory and so changes its version. While the version is unchanged and
// settled and the read is younger than drainReconcile, a drain reads only
// those envelopes again; otherwise it reads the whole spool. A Drainer
// belongs to one goroutine.
type Drainer struct {
	spool *Spool
	ep    *core.Endpoint

	pendingKeys []Key
	pendingDir  fsq.FileVersion
	pendingAt   time.Time
	pendingOK   bool
}

// NewDrainer returns a drainer that replays pending envelopes in spool through
// ep.
func NewDrainer(spool *Spool, ep *core.Endpoint) *Drainer {
	return &Drainer{spool: spool, ep: ep}
}

// Drain replays every pending envelope once. It is safe to call on each tick:
// a settled envelope is skipped, a pending one is dispatched or marked with a
// transient/terminal failure. Returns the number of pending envelopes it
// replayed, whatever the outcome (dispatched, expired, failed, or left
// pending for a transient code).
//
// A per-envelope error never aborts the sweep: one bad envelope must not stop
// another's recovery (mirrors the carrier's ImportOnce contract).
func (d *Drainer) Drain(_ context.Context) (int, error) {
	envs, err := d.pending()
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
		n++
		// A spooled submit carries no origin: the spool is a local file, and
		// an origin is a carrier's claim that only the carrier may make.
		reply, herr := d.ep.Handle(env.Command, core.Source{Host: env.CreatorHost})
		// Classify by Outcome.Code, NOT by error. The endpoint returns
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
		case code == protocol.CodeExpired:
			// The endpoint has no record and the window closed: nothing ran.
			if err := d.spool.MarkExpired(k); err != nil {
				errs = append(errs, err)
			}
		case isTransientCode(code):
			// Busy / draining / unreachable / storage_full: retry next tick.
			if err := d.spool.MarkAttempt(k, string(code)); err != nil {
				errs = append(errs, err)
			}
		default:
			// Terminal refusal (stale_epoch, unshared,
			// request_conflict, invalid, unsupported): no retry will succeed.
			if err := d.spool.MarkFailed(k, string(code)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return n, errors.Join(errs...)
}

// pending returns the pending envelopes, oldest first: the ones the last
// full read found, each read again, while the spool directory is unchanged;
// else a full read of the spool.
func (d *Drainer) pending() ([]*Envelope, error) {
	now := d.spool.now()
	dir, stable, verr := fsq.LstatVersion(d.spool.dir, now)
	if verr == nil && stable && d.pendingOK && dir == d.pendingDir && now.Sub(d.pendingAt) < drainReconcile {
		if len(d.pendingKeys) == 0 {
			return nil, nil
		}
		return d.spool.reread(d.pendingKeys)
	}
	d.pendingOK = false
	envs, err := d.spool.List()
	if err != nil {
		return nil, err
	}
	var out []*Envelope
	var keys []Key
	for _, env := range envs {
		if env.State == StatePending {
			out = append(out, env)
			keys = append(keys, env.key())
		}
	}
	d.pendingKeys, d.pendingDir, d.pendingAt, d.pendingOK = keys, dir, now, verr == nil && stable
	return out, nil
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
	// extract its Outcome.Code. A pointer assertion never matches, so every
	// refusal would be marked dispatched.
	if rep, ok := reply.(protocol.Reply); ok {
		return rep.Outcome.Code
	}
	return ""
}

// isTransientCode reports whether a dispatch outcome code is worth retrying.
// Busy IS transient: a target busy ONCE is not a permanent failure of the
// caller's command. The envelope stays pending, retrying next tick, bounded
// by NotAfter (only expiry ends it). The spool exists for the caller who is
// not there to retry; abandoning an accepted envelope on one busy tick would
// defeat its purpose. The live CLI submit still returns busy to the present
// caller with exit 6 — that half is right.
func isTransientCode(code protocol.Code) bool {
	switch code {
	case protocol.CodeBusy,
		protocol.CodeDraining,
		protocol.CodeEndpointUnreachable,
		protocol.CodeStorageFull:
		return true
	}
	return false
}
