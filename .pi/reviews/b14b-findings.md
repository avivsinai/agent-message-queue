# B14b (#737) — NO-GO. Fresh Opus reviewer (probed) + ChatGPT Pro (partial, corroborated
# the blocker independently before a 10-min generation timeout). Lead verified both by reading.
# This recut SIMPLIFIES B14b — mostly removing dead machinery.

## BLOCKER (confirmed by both reviewers + lead read + a deterministic probe)
Close teardown order panics: send on closed channel.
  rpc.go Close(): workerStop.Do{ close(events); close(reqQ) } -> waitCallbacks() (<=2s) -> ws.close()
The read pump (readLoop) stays LIVE during the whole waitCallbacks window. An inbound
server-request (approval) frame in that window hits `case c.reqQ <- sr:` (rpc.go:177) on a
CLOSED reqQ -> panic, no recover in readLoop -> process crash. Probe reproduced it in <30 iters.
FIX: tear the socket down and DRAIN the read pump FIRST, then close the queues.
  c.ws.close() -> wait on c.closed (readLoop closes it on exit) -> THEN close(reqQ)/close(events).
  Or guard dispatchServerRequest against a closing state. Add a regression that runs Close
  concurrently with readLoop delivering a server request (the current tests drive dispatch
  DIRECTLY and never through readLoop teardown — that gap is why -race missed it).

## DESIGN — remove the dead notification-worker subsystem (SIMPLIFY)
readLoop calls c.OnNotification SYNCHRONOUSLY (rpc.go:267-268). The c.events / dispatch /
eventWorker / startWorker / eventsInFlight / workerStart / workerDone machinery has NO production
caller (only tests call c.dispatch). It is dead weight, and it makes B9/waitCallbacks track the
WRONG path. This is fine functionally: onNative is a bounded state-apply (after B14a the ack is
outside e.mu), so running notifications synchronously in the read pump does NOT wedge it — only
reply-required approval handlers can wedge, and those ARE off-pump via reqQ.
FIX: DELETE the events/dispatch/eventWorker/eventsInFlight/startWorker/workerStart/workerDone
machinery. Keep ONLY reqQ + reqWorker (the live reply-required path). B9 then reduces to the
reqWorker lifecycle: start on newClient, stop cleanly on Close (after the read-pump drain above),
and waitCallbacks tracks the reqWorker's in-flight count (a reqInFlight atomic), NOT eventsInFlight.
Net: less code, and waitCallbacks actually waits for the callbacks that exist.

## SECONDARY (fix in the same recut)
- LOW/MED: overflow respondError writes from INSIDE the read pump via the UNBOUNDED lockWrite +
  writeFrameBody (no deadline). On a wedged writer/full socket buffer the read pump blocks — the
  exact thing B8 exists to prevent, on B8's own overflow path. Bound that write (deadline, or make
  it non-blocking). Same unbounded-write sibling exists on the pong reply in readText — bound it too.
- LOW: waitCallbacks mis-tracking (covered by the DESIGN fix above — track reqWorker in-flight).
- NIT: reqQCapacity field (rpc.go ~93/124) is written, never read. Delete.

## CONFIRMED CORRECT (keep)
- B6 wmu channel semaphore: correct. Every acquire paired with release; lockWriteCtx ctx-bounded;
  close() non-blocking try-acquire; deadline set/cleared under the held slot; no leftover Mutex API.
- B8 reply-required reqQ: non-blocking dispatch + strictly error-only overflow response are correct
  (undercut only by the BLOCKER teardown + the read-pump write above).
- The two extra fixes (close-frame off the blocking path; acceptServerWS via constructor so the
  channel wmu is non-nil) are correct.

## TEST-BAR
Keep the hard-deadline discipline (5s, no NumGoroutine). ADD the missing coverage: a Close-vs-
readLoop teardown regression (server request arriving during Close) — that is the gap that let the
blocker through. After removing the dead events machinery, drop the tests that only drove the dead
dispatch/events path.

Reviews: Opus in lead session; Pro partial corroboration (timed out mid-generation, same blocker).
