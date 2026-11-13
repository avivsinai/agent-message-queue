package adapter

import (
	"context"

	"github.com/avivsinai/agent-message-queue/internal/writerlock"
)

// WriterLockInspector reports whether a Codex thread-writer lock is held
// WITHOUT acquiring it. The idle-thread case is an untested follow-up; this
// seat only claims submitted delivery when a writer already holds the lock.
// Native inspection errors fail closed (no lsof fallback: an open file is not
// a held flock).
type WriterLockInspector interface {
	Held(ctx context.Context, path string) (bool, error)
}

// platformWriterLockInspector is the native inspector in internal/writerlock.
type platformWriterLockInspector struct{}

func (platformWriterLockInspector) Held(ctx context.Context, path string) (bool, error) {
	return writerlock.Held(ctx, path)
}
