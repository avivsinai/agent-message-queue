//go:build unix

package loginservice

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// logMaxBytes bounds a watched log. Rotation keeps one older file, <path>.1,
// so the log uses at most twice this.
const logMaxBytes int64 = 1 << 20

// LogSink is the bounded log a companion's long-running command writes when
// its output is the log file path: every line gets the UTC time, and the
// file is rotated in place before it grows past logMaxBytes. A terminal, a
// journal, or any other writer is returned unchanged.
func LogSink(path string, w io.Writer) io.Writer {
	f, ok := w.(*os.File)
	if !ok {
		return w
	}
	got, err := f.Stat()
	if err != nil || !got.Mode().IsRegular() {
		return w
	}
	want, err := os.Stat(path)
	if err != nil || !os.SameFile(got, want) {
		return w
	}
	af, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return w
	}
	lk, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = af.Close()
		return w
	}
	return &logWriter{f: af, lk: lk, path: path, max: logMaxBytes, now: time.Now, bol: true}
}

// logWriter writes into the log file through a handle it opened itself, so
// correctness does not depend on how the inherited descriptor was opened.
// One flock on <path>.lock serializes the writes and rotations of every
// process that logs through LogSink; a writer that bypasses it is not
// serialized and may lose a line to a rotation.
type logWriter struct {
	mu   sync.Mutex
	f    *os.File
	lk   *os.File
	path string
	max  int64
	now  func() time.Time
	bol  bool // the next byte starts a line
}

func (l *logWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	stamp := l.now().UTC().Format(time.RFC3339) + " "
	var b bytes.Buffer
	for rest := p; len(rest) > 0; {
		if l.bol {
			b.WriteString(stamp)
		}
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line = rest[:i+1]
		}
		b.Write(line)
		rest = rest[len(line):]
		l.bol = line[len(line)-1] == '\n'
	}
	// A line is never dropped for the sake of the bound: with no lock the
	// write goes on without rotation, and a rotation that cannot run leaves
	// the log as it is.
	locked := lock.Lock(l.lk) == nil
	if locked {
		defer func() { _ = lock.Unlock(l.lk) }()
	}
	for b.Len() > 0 {
		chunk := b.Next(int(l.max))
		if locked {
			_ = l.rotate(int64(len(chunk)))
		}
		if _, err := l.f.Write(chunk); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// rotate archives the newest max bytes and empties the open log before
// the next write would cross its limit. An older unbounded log can be large.
func (l *logWriter) rotate(n int64) error {
	st, err := l.f.Stat()
	if err != nil || st.Size()+n <= l.max {
		return err
	}
	src, err := os.Open(l.path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	cur, err := src.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(st, cur) {
		return fmt.Errorf("log file %s was replaced", l.path)
	}
	data, err := io.ReadAll(io.NewSectionReader(src, max(0, st.Size()-l.max), l.max))
	if err != nil {
		return err
	}
	dir := filepath.Dir(l.path)
	if _, err := fsq.WriteStateFile(filepath.Dir(dir), filepath.Join(filepath.Base(dir), filepath.Base(l.path)+".1"), data, 0o600); err != nil {
		return err
	}
	return l.f.Truncate(0)
}
