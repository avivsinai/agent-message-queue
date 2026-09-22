package claude

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// openNoFollow opens path refusing to follow a final-component symlink
// (unix: O_NOFOLLOW; windows: the syscall path has no symlink-follow on
// FILE_FLAG_OPEN_REPARSE_POINT open via os.OpenFile is not available, so
// the open-then-fstat recheck below carries the guarantee there).

// transcriptPath maps cwd+sessionId to the target's JSONL transcript:
// ~/.claude/projects/<slug>/<sessionId>.jsonl (slug = cwd with every
// non-alphanumeric character replaced by '-' — the observed rule, e.g.
// "Application Support" → "Application-Support").
func transcriptPath(home, cwd, sessionID string) string {
	return filepath.Join(home, ".claude", "projects", slugifyCwd(cwd), sessionID+".jsonl")
}

func slugifyCwd(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	return b.String()
}

// transcriptTailBytes is the tail window the confirmation poller reads:
// transcripts grow unboundedly, and the evidence ladder only needs the
// newest entries. 256 KiB covers many turns.
const transcriptTailBytes = 256 << 10

// readTranscriptTail returns the last transcriptTailBytes of the target's
// transcript. The leaf is lstat'd before opening — regular files only
// (the same local-writable-path rule as the session registry: a FIFO
// would block an unbounded read, and a symlink must never be followed);
// a missing transcript reads as an empty tail (the harness may not have
// flushed anything yet). All failures are non-fatal: the ladder only
// moves forward on positive observation.
func readTranscriptTail(home, cwd, sessionID string) string {
	path := transcriptPath(home, cwd, sessionID)
	fi, err := os.Lstat(path)
	if err != nil {
		return "" // missing (or unreadable link): empty tail
	}
	if !fi.Mode().IsRegular() {
		return "" // FIFO/dir/symlink at the leaf: never read
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollowFlag, 0)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	// Re-check via f.Stat: the lstat→open race is closed by stat-ing the
	// OPEN file description, not the path.
	fi2, err := f.Stat()
	if err != nil || !fi2.Mode().IsRegular() {
		return ""
	}
	size := fi2.Size()
	if size > transcriptTailBytes {
		if _, err := f.Seek(size-transcriptTailBytes, 0); err != nil {
			return ""
		}
	}
	buf := make([]byte, transcriptTailBytes)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, os.ErrClosed) {
		if n <= 0 {
			return ""
		}
	}
	// A partial trailing line (append racing the read) is dropped: the
	// poller only needs complete JSONL entries.
	out := string(buf[:n])
	if i := strings.LastIndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	} else {
		return "" // no complete line in the window
	}
	return out
}
