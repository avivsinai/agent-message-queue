package pi

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ActivityNote is one session activity record the bridge wrote to
// activity/<session_id>.jsonl (docs/pi-bridge-protocol.md, activity). Kind is
// turn_start, turn_end, user, assistant, tool_start, or tool_end. At is Unix
// milliseconds, 0 when the record carries no parsable time.
type ActivityNote struct {
	SessionID string
	TurnID    string
	Kind      string
	ID        string
	Text      string
	Tool      string
	Status    string
	At        int64
}

type activityRecord struct {
	Protocol string `json:"protocol"`
	At       string `json:"at"`
	Turn     string `json:"turn"`
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Text     string `json:"text"`
	Tool     string `json:"tool"`
	Status   string `json:"status"`
}

// activityPoll is the tail's poll period, the bridge's own request poll
// period (POLL_MS in integrations/pi/amq-bridge.ts).
const activityPoll = 200 * time.Millisecond

// activityReadChunk bounds one poll's read. The bridge keeps a record under
// 64 KiB and the file under 4 MiB.
const activityReadChunk = 1 << 20

// ObservePiActivity registers a read-only tail of the live bridge's
// activity file for its current session, starting at the file's current end:
// there is no history replay. A changed session id restarts the tail at the
// new file's end. Lines that do not parse, or carry another protocol, are
// skipped. The adapter never writes the directory. The callback runs on the
// tail's own goroutine, never with a.mu held; stop ends it.
func (a *Attachment) ObservePiActivity(cb func(ActivityNote)) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(activityPoll)
		defer t.Stop()
		var session string
		var off int64
		for {
			if id := a.NativeSessionID(); id != session {
				session, off = id, -1 // -1: start at the new file's end
			}
			if session != "" {
				off = a.tailActivity(session, off, cb)
			}
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-exited
		})
	}
}

// tailActivity delivers the complete lines after off and returns the new
// offset. off < 0 means the current end. A file that shrank restarts at 0.
func (a *Attachment) tailActivity(session string, off int64, cb func(ActivityNote)) int64 {
	f, err := os.Open(filepath.Join(a.dir.dir, "activity", session+".jsonl"))
	if err != nil {
		if off < 0 {
			return 0 // a file that appears later is read from its start
		}
		return off
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return off
	}
	if off < 0 {
		return st.Size()
	}
	if st.Size() < off {
		off = 0
	}
	buf := make([]byte, min(st.Size()-off, activityReadChunk))
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return off
	}
	buf = buf[:n]
	end := bytes.LastIndexByte(buf, '\n') + 1
	if end == 0 {
		if n == activityReadChunk {
			return off + int64(n) // one line past the chunk is dropped, not retried forever
		}
		return off
	}
	for _, line := range bytes.Split(buf[:end], []byte{'\n'}) {
		var rec activityRecord
		if len(line) == 0 || json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Protocol != "" && rec.Protocol != a.dir.names.protocol {
			continue
		}
		note := ActivityNote{SessionID: session, TurnID: rec.Turn, Kind: rec.Kind, ID: rec.ID,
			Text: rec.Text, Tool: rec.Tool, Status: rec.Status}
		if at, err := time.Parse(time.RFC3339Nano, rec.At); err == nil {
			note.At = at.UnixMilli()
		}
		cb(note)
	}
	return off + int64(end)
}
