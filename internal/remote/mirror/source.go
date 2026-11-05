package mirror

import (
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
)

// Source is how the mirror reads one harness's record of a session: a
// JSONL file the harness (or its AMQ bridge) appends to.
type Source struct {
	// Locate is the record of binding b, given the record the mirror
	// already reads (saved, "" the first time); "" when none is known.
	Locate func(b binding.Binding, saved string) string
	// Time is the time of one record line in unix milliseconds, 0 when the
	// line has none or cannot be read.
	Time func(line string) int64
	// Fold applies one record line to the fold.
	Fold func(st State, line string, carried Carried) (State, []Event)
}

// SourceFor is the source of a harness the mirror shows, nil for any other.
func SourceFor(harness string) *Source {
	switch harness {
	case "claude":
		return &claudeSource
	}
	return nil
}

var claudeSource = Source{
	Locate: claudeLocate,
	Time: func(line string) int64 {
		if l, ok := claude.ParseTranscriptLine(line); ok {
			return l.TS
		}
		return 0
	},
	Fold: func(st State, line string, carried Carried) (State, []Event) {
		l, ok := claude.ParseTranscriptLine(line)
		if !ok {
			return st, nil
		}
		return Fold(st, l, carried)
	},
}

// claudeLocate is the transcript the holder's session registry entry names
// when that file exists, else the transcript the mirror already reads
// (saved), else, the first time, the binding's own Transcript. A registry
// entry that cannot be read for a moment never moves the cursor back to an
// older file.
func claudeLocate(b binding.Binding, saved string) string {
	if path := claude.SessionTranscript(b.Transcript, b.Holder.PID); path != "" {
		return path
	}
	if saved != "" {
		return saved
	}
	return b.Transcript
}
