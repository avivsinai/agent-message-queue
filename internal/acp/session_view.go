package acp

import (
	"slices"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// sessionViewReconcile is the longest the workers go without a full read of
// the bindings and the channel records.
const sessionViewReconcile = time.Minute

// sessionView is the decoded bindings and active channel records that the
// machine, dialog and mirror workers share. A worker that enumerates them
// takes the current snapshot; the snapshot is read again when the version of
// the binding file, the bindings directory or the channel directory changed,
// when one of them had not settled, or after sessionViewReconcile. Every
// writer of a binding or a record renames a file into its directory, which
// changes the directory's version. The snapshot holds file content only: a
// session's liveness is asked of the binding on each use, and a step that
// acts reads its binding and record again.
type sessionView struct {
	mu   sync.Mutex
	key  sessionViewKey
	keep bool // key was settled when snap was read
	at   time.Time
	snap *sessionSnapshot
}

type sessionViewKey struct {
	file, dir, channels    string
	fileV, dirV, channelsV fsq.FileVersion
}

// sessionSnapshot is one read of the bindings and the active records. It is
// never changed after it is made.
type sessionSnapshot struct {
	bindings []binding.Binding
	listErr  error
	byName   map[string]binding.Binding
	records  []channelRecord
	slots    map[string]channelRecord
}

// snapshot returns the current snapshot. It never waits on a relay call:
// the lock covers only file reads.
func (s *Server) snapshot() *sessionSnapshot {
	v := &s.view
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	key, settled := s.viewKey(now)
	if v.snap != nil && v.keep && key == v.key && now.Sub(v.at) < sessionViewReconcile {
		return v.snap
	}
	snap := &sessionSnapshot{byName: map[string]binding.Binding{}}
	snap.records, snap.slots = s.readSlots()
	snap.bindings, snap.listErr = binding.List()
	for _, b := range snap.bindings {
		snap.byName[b.Name] = b
	}
	v.key, v.keep, v.at, v.snap = key, settled, now, snap
	return snap
}

// viewKey is the version of each watched path; settled is false when one
// could not be read or changed too recently to stand for its content.
func (s *Server) viewKey(now time.Time) (sessionViewKey, bool) {
	var k sessionViewKey
	var err error
	if k.file, k.dir, err = binding.WatchPaths(); err != nil {
		return k, false
	}
	k.channels = s.channelDir()
	settled := true
	for _, w := range []struct {
		path string
		v    *fsq.FileVersion
	}{{k.file, &k.fileV}, {k.dir, &k.dirV}, {k.channels, &k.channelsV}} {
		v, ok, err := fsq.LstatVersion(w.path, now)
		*w.v = v
		settled = settled && ok && err == nil
	}
	return k, settled
}

// binding is name's binding as the snapshot listed it; ok is false when the
// snapshot has none, and a caller that acts on its absence reads it again.
func (v *sessionSnapshot) binding(name string) (binding.Binding, bool) {
	b, ok := v.byName[name]
	return b, ok
}

// list is binding.List as of the snapshot.
func (v *sessionSnapshot) list() ([]binding.Binding, error) {
	return slices.Clone(v.bindings), v.listErr
}

// activeRecords is the good records in the name slots as of the snapshot.
func (v *sessionSnapshot) activeRecords() []channelRecord {
	out := make([]channelRecord, len(v.records))
	for i, r := range v.records {
		r.Orphans = slices.Clone(r.Orphans)
		out[i] = r
	}
	return out
}

// slot is the good record in name's slot as of the snapshot.
func (v *sessionSnapshot) slot(name string) (channelRecord, bool) {
	r, ok := v.slots[name]
	r.Orphans = slices.Clone(r.Orphans)
	return r, ok
}
