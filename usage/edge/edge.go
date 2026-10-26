// Package edge holds the effect contracts a usage collection runs through:
// the clock, a bounded command runner, and persistence. Collection code
// receives these as values; production wiring binds the real ones and tests
// bind ordinary implementations of the same contracts.
package edge

import (
	"context"
	"time"

	"github.com/avivsinai/agent-message-queue/usage"
)

// Clock reads wall time.
type Clock func() time.Time

// Command is one child process run. The runner starts Path with Args
// directly (no shell), with Env added to the parent's environment, in its
// own process group. It writes Stdin and keeps stdin open until Done
// reports true for the stdout read so far, then closes stdin so the child
// can exit; a nil Done closes stdin after writing. Stdout past MaxOut ends
// the run. Cancelling the context kills the whole process group, and the
// runner returns only after the child is reaped.
type Command struct {
	Path   string
	Args   []string
	Env    []string
	Stdin  []byte
	MaxOut int
	Done   func(stdout []byte) bool
}

// CommandReply is what a Command run produced. Failure is empty when the
// child exited on its own (or Done reported true); otherwise it is one of
// the usage failure codes and Err says why.
type CommandReply struct {
	Stdout   []byte
	ExitCode usage.Field[int]
	Failure  string
	Err      error
}

// Runner runs one Command.
type Runner func(context.Context, Command) CommandReply

// Journal is the durable coordination record: every reserved attempt whose
// hold may still apply.
type Journal struct {
	Attempts []usage.Attempt `json:"attempts"`
	// Dropped names each stored attempt the reader rejected and left out.
	// It describes one load and is never written back.
	Dropped []string `json:"-"`
}

// SnapshotReader reads the sanitized fact snapshot. A missing snapshot is an
// empty Result with no error.
type SnapshotReader interface {
	LoadFacts() (usage.Result, error)
}

// Persistence is the collector's durable state. TryLock takes the one
// collector lock without waiting: ok is false, with no error, while another
// holder has it. A missing journal is empty; an unreadable or corrupt file is
// an error, never an empty journal. An attempt the reader rejects is dropped
// and named in Dropped; the others are kept. Each Save is atomic and durable (data
// and directory synced) before it returns nil.
type Persistence interface {
	SnapshotReader
	TryLock() (release func(), ok bool, err error)
	LoadJournal() (Journal, error)
	SaveJournal(Journal) error
	SaveFacts(usage.Result) error
}

// Effects are the effects one collection pass may use.
type Effects struct {
	Clock   Clock
	Command Runner
	Store   Persistence
}
