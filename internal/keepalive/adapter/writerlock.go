package adapter

import "context"

// WriterLockInspector reports whether a Codex thread-writer lock is held
// WITHOUT acquiring it. The idle-thread case is an untested follow-up; this
// seat only claims submitted delivery when a writer already holds the lock.
// Native inspection errors fail closed (no lsof fallback: an open file is not
// a held flock).
type WriterLockInspector interface {
	Held(ctx context.Context, path string) (bool, error)
}
