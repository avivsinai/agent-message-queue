//go:build unix

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// upLogMaxBytes bounds up.log. Rotation keeps one older file, up.log.1, so
// the log uses at most twice this.
const upLogMaxBytes int64 = 1 << 20

// upLog is the log of `up` and the serve it supervises when their output is
// up.log in the state directory. Every line it writes starts with the UTC
// time. Before a write would take the file past max, its content moves to
// up.log.1 and the file is truncated in place, so every writer that holds the
// file open keeps writing to the same, bounded file. Lines go out through a
// handle upLog opens itself with O_APPEND, so correctness does not depend on
// how the inherited descriptor was opened. One flock on up.log.lock
// serializes the writes and rotations of every process that logs through
// upLog; a writer that bypasses upLog is not serialized and may lose a line
// to a rotation.
type upLog struct {
	mu   sync.Mutex
	f    *os.File
	lk   *os.File
	path string
	max  int64
	now  func() time.Time
	bol  bool // the next byte starts a line
}

// logSink returns the writer up and serve log through. It is w itself unless
// w is up.log in stateDir; a terminal or a journal gets the output unchanged.
func logSink(stateDir string, w io.Writer) io.Writer {
	f, ok := w.(*os.File)
	if !ok {
		return w
	}
	path := filepath.Join(stateDir, "up.log")
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
	return &upLog{f: af, lk: lk, path: path, max: upLogMaxBytes, now: time.Now, bol: true}
}

func (l *upLog) Write(p []byte) (int, error) {
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
	if err := lock.Lock(l.lk); err == nil {
		defer func() { _ = lock.Unlock(l.lk) }()
		l.rotate(int64(b.Len()))
	}
	if _, err := l.f.Write(b.Bytes()); err != nil {
		return 0, err
	}
	return len(p), nil
}

// rotate moves the log to up.log.1 and empties it when n more bytes would
// take it past max. It leaves the log as it is when the path no longer names
// the open file or the copy fails, so a failed rotation never drops lines.
func (l *upLog) rotate(n int64) {
	st, err := l.f.Stat()
	if err != nil || st.Size()+n <= l.max || st.Size() == 0 {
		return
	}
	if cur, err := os.Stat(l.path); err != nil || !os.SameFile(st, cur) {
		return
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	if _, err := fsq.WriteFileAtomicFollowLinks(filepath.Dir(l.path), filepath.Base(l.path)+".1", data, 0o600); err != nil {
		return
	}
	_ = l.f.Truncate(0)
}
