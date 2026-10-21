package codex

import "time"

// WithClock overrides the attachment's clock.
func WithClock(now func() time.Time) Option { return func(a *Attachment) { a.now = now } }

// WithConfirmTimeout overrides the confirm timeout.
func WithConfirmTimeout(d time.Duration) Option { return func(a *Attachment) { a.confirmTimeout = d } }
