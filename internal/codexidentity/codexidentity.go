// Package codexidentity keeps the AMQ identity that amq coop exec gave a
// Codex thread it created on Codex's shared app-server daemon. Codex 0.160
// runs a TUI's tool commands in the daemon's environment, so they see none
// of the AM_* variables coop exec set; they do see CODEX_THREAD_ID, the
// thread coop exec created. A command finds its identity here by that id.
//
// The store is per user ($HOME/.amq/codex-threads, mode 0700): one record
// per thread under threads/, and under current/ the thread that the newest
// coop exec gave each (root identity, handle). A record whose thread is no
// longer current for its handle is revoked: a later coop exec for the same
// handle replaced it. Under bindings/ each publication also leaves one
// event with its time, so the history of which handle carried which thread,
// and when that changed, survives a later publication of the same thread.
// Records are never deleted by AMQ, so a revoked or damaged record stays
// distinguishable from a thread AMQ never managed.
// The store keeps the owner user's trust boundary; it is no defense against
// another process running as the same user.
package codexidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// Record is the identity coop exec put in the environment of the TUI it
// started on Thread.
type Record struct {
	Schema     int    `json:"schema"`
	Thread     string `json:"thread"`
	CodexHome  string `json:"codex_home"`
	Root       string `json:"root"`
	BaseRoot   string `json:"base_root"`
	Session    string `json:"session,omitempty"`
	Me         string `json:"me"`
	RootID     string `json:"root_id"`
	BaseRootID string `json:"base_root_id"`
}

// ErrNone means AMQ never recorded an identity for the thread.
var ErrNone = errors.New("no AMQ identity is recorded for this Codex thread")

const (
	schema   = 1
	maxBytes = 4096
)

var threadRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidThread reports whether id has the shape of a Codex thread id.
func ValidThread(id string) bool { return threadRe.MatchString(id) }

// Publish durably records r, then makes r.Thread the current thread of its
// (root identity, handle), under the store lock so the record and its
// current marker change together. Both writes are atomic and synced, and
// the store's directories are synced into their parents first, so a thread
// is current only after its record is durable. The newest publication for a
// handle wins; a caller publishes a launch's thread once.
func Publish(r Record) error {
	r.Schema = schema
	if err := r.check(); err != nil {
		return err
	}
	dir, err := storeDir(true)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	current, err := json.Marshal(currentRecord{Schema: schema, Thread: r.Thread})
	if err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(filepath.Join(dir, ".lock"), func() error {
		// Make every directory of the store durable in its parent on every
		// publication, not only when this call created it: an earlier
		// publication may have failed after mkdir, or another may still be
		// between its mkdir and its sync.
		for _, d := range []string{filepath.Dir(filepath.Dir(dir)), filepath.Dir(dir), dir} {
			if err := fsq.SyncDir(d); err != nil {
				return fmt.Errorf("sync %s: %w", d, err)
			}
		}
		if _, err := fsq.WriteFileAtomic(filepath.Join(dir, "threads"), r.Thread+".json", append(raw, '\n'), 0o600); err != nil {
			return fmt.Errorf("record the Codex thread identity: %w", err)
		}
		if _, err := fsq.WriteFileAtomic(filepath.Join(dir, "current"), currentKey(r)+".json", append(current, '\n'), 0o600); err != nil {
			return fmt.Errorf("record the current Codex thread: %w", err)
		}
		now := time.Now().UTC()
		event, err := json.Marshal(bindingEvent{Schema: schema, Thread: r.Thread, Me: r.Me, RootID: r.RootID, Bound: now.Format(time.RFC3339Nano)})
		if err != nil {
			return err
		}
		if _, err := fsq.WriteFileAtomic(filepath.Join(dir, "bindings"), fmt.Sprintf("%020d-%s.json", now.UnixNano(), r.Thread), append(event, '\n'), 0o600); err != nil {
			return fmt.Errorf("record the Codex thread binding: %w", err)
		}
		return nil
	})
}

// Lookup returns the identity recorded for thread under codexHome. It
// returns ErrNone when AMQ never recorded one, and an error when a record
// exists but is damaged, belongs to another CODEX_HOME, or was revoked by a
// later coop exec for the same handle.
func Lookup(thread, codexHome string) (Record, error) {
	if !ValidThread(thread) {
		return Record{}, fmt.Errorf("CODEX_THREAD_ID %q is not a Codex thread id", thread)
	}
	dir, err := storeDir(false)
	if err != nil {
		return Record{}, err
	}
	if dir == "" {
		return Record{}, ErrNone
	}
	var r Record
	if err := readStrict(filepath.Join(dir, "threads", thread+".json"), &r); err != nil {
		return Record{}, err
	}
	if err := r.check(); err != nil {
		return Record{}, fmt.Errorf("the identity record of Codex thread %s is invalid: %v", thread, err)
	}
	if r.Thread != thread {
		return Record{}, fmt.Errorf("the identity record of Codex thread %s names thread %s", thread, r.Thread)
	}
	if filepath.Clean(r.CodexHome) != filepath.Clean(codexHome) {
		return Record{}, fmt.Errorf("the Codex thread %s was recorded for CODEX_HOME %s, not %s", thread, r.CodexHome, codexHome)
	}
	var cur currentRecord
	if err := readStrict(filepath.Join(dir, "current", currentKey(r)+".json"), &cur); err != nil {
		if errors.Is(err, ErrNone) {
			return Record{}, fmt.Errorf("the Codex thread %s has an identity record but no current-thread record for %s", thread, r.Me)
		}
		return Record{}, err
	}
	if cur.Thread != thread {
		return Record{}, fmt.Errorf("a later amq coop exec gave handle %s in %s to Codex thread %s; this thread %s no longer carries that identity", r.Me, r.Root, cur.Thread, thread)
	}
	return r, nil
}

// Binding is one interval during which a handle of a root carried a Codex
// thread. Bound is when coop exec gave the handle the thread; Revoked is
// when a later publication gave the handle another thread or gave the
// thread to another handle, and is zero while the binding holds. Uncertain
// marks an open binding that the thread record or the handle's current
// marker does not confirm.
type Binding struct {
	Thread    string
	Me        string
	RootID    string
	Bound     time.Time
	Revoked   time.Time
	Uncertain bool
}

// bindingEvent is one publication, kept under bindings/ apart from the
// thread's identity record so that a later publication of the same thread
// never rewrites an earlier binding or its time.
type bindingEvent struct {
	Schema int    `json:"schema"`
	Thread string `json:"thread"`
	Me     string `json:"me"`
	RootID string `json:"root_id"`
	Bound  string `json:"bound"`
}

// Bindings replays the publication history into bindings. It never creates
// the store; a missing store lists nothing, and a damaged event is skipped.
// A thread recorded before the history existed is listed only while its
// handle's current marker still names it, since its binding time is unknown.
func Bindings() ([]Binding, error) {
	dir, err := storeDir(false)
	if err != nil || dir == "" {
		return nil, err
	}
	names, err := os.ReadDir(filepath.Join(dir, "bindings"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	type event struct {
		bindingEvent
		at time.Time
	}
	var events []event
	for _, name := range names {
		var e bindingEvent
		if !strings.HasSuffix(name.Name(), ".json") || readStrict(filepath.Join(dir, "bindings", name.Name()), &e) != nil ||
			e.Schema != schema || !ValidThread(e.Thread) || strings.TrimSpace(e.Me) == "" || strings.TrimSpace(e.RootID) == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, e.Bound)
		if err != nil {
			continue
		}
		events = append(events, event{bindingEvent: e, at: at})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	key := func(rootID, me string) string { return rootID + "\x00" + me }
	var out []Binding
	open := map[string]int{}     // handle key -> index of its open binding
	owner := map[string]string{} // thread -> handle key of its open binding
	closeOpen := func(k string, at time.Time) {
		if i, ok := open[k]; ok {
			out[i].Revoked = at
			delete(owner, out[i].Thread)
			delete(open, k)
		}
	}
	for _, e := range events {
		k := key(e.RootID, e.Me)
		if i, ok := open[k]; ok && out[i].Thread == e.Thread {
			continue
		}
		closeOpen(k, e.at)
		if other, ok := owner[e.Thread]; ok {
			closeOpen(other, e.at)
		}
		open[k] = len(out)
		owner[e.Thread] = k
		out = append(out, Binding{Thread: e.Thread, Me: e.Me, RootID: e.RootID, Bound: e.at})
	}
	for _, i := range open {
		out[i].Uncertain = !confirmed(dir, out[i].Thread, out[i].RootID, out[i].Me)
	}
	threads, err := os.ReadDir(filepath.Join(dir, "threads"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, name := range threads {
		thread, ok := strings.CutSuffix(name.Name(), ".json")
		if !ok || !ValidThread(thread) {
			continue
		}
		if _, known := owner[thread]; known || historyNames(out, thread) {
			continue
		}
		var r Record
		if readStrict(filepath.Join(dir, "threads", name.Name()), &r) != nil || r.check() != nil || r.Thread != thread {
			continue
		}
		if confirmed(dir, thread, r.RootID, r.Me) {
			out = append(out, Binding{Thread: thread, Me: r.Me, RootID: r.RootID})
		}
	}
	return out, nil
}

// historyNames reports whether the history has any binding of thread.
func historyNames(bindings []Binding, thread string) bool {
	for _, b := range bindings {
		if b.Thread == thread {
			return true
		}
	}
	return false
}

// confirmed reports whether the thread's identity record names this handle
// of this root and the handle's current marker names the thread.
func confirmed(dir, thread, rootID, me string) bool {
	var r Record
	if readStrict(filepath.Join(dir, "threads", thread+".json"), &r) != nil || r.check() != nil ||
		r.Thread != thread || r.RootID != rootID || r.Me != me {
		return false
	}
	var cur currentRecord
	return readStrict(filepath.Join(dir, "current", currentKey(r)+".json"), &cur) == nil && cur.Thread == thread
}

type currentRecord struct {
	Schema int    `json:"schema"`
	Thread string `json:"thread"`
}

func (r Record) check() error {
	switch {
	case r.Schema != schema:
		return fmt.Errorf("unknown schema %d", r.Schema)
	case !ValidThread(r.Thread):
		return fmt.Errorf("thread %q is not a Codex thread id", r.Thread)
	case !filepath.IsAbs(r.CodexHome) || !filepath.IsAbs(r.Root) || !filepath.IsAbs(r.BaseRoot):
		return errors.New("CODEX_HOME and both roots must be absolute")
	case strings.TrimSpace(r.Me) == "" || strings.TrimSpace(r.RootID) == "" || strings.TrimSpace(r.BaseRootID) == "":
		return errors.New("the handle and both root identity tokens are required")
	}
	return nil
}

// currentKey names the current-thread record of one handle in one root,
// by the root's identity token so a recreated root starts a new lineage.
func currentKey(r Record) string {
	sum := sha256.Sum256([]byte(r.RootID + "\x00" + r.Me))
	return hex.EncodeToString(sum[:16])
}

// CurrentDir is the current/ directory of the store below home's AMQ home.
// It exists once any coop exec published a thread; a hook gate tests it
// before it starts amq.
func CurrentDir(home string) string {
	return filepath.Join(amqhome.Under(home), storeName, "current")
}

const storeName = "codex-threads"

// storeDir is $HOME/.amq/codex-threads with its threads/ and current/
// directories. Every directory below the home must be a plain directory,
// not a symlink. With create, the AMQ home is made and checked by amqhome
// and missing directories below it are made with mode 0700 (Publish then
// syncs them); without, a missing store returns "".
func storeDir(create bool) (string, error) {
	amqDir, err := amqhome.Dir()
	if create {
		amqDir, err = amqhome.EnsureDir()
	}
	if err != nil {
		return "", err
	}
	store := filepath.Join(amqDir, storeName)
	for _, cur := range []string{filepath.Dir(store), store, filepath.Join(store, "threads"), filepath.Join(store, "current"), filepath.Join(store, "bindings")} {
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				if cur == filepath.Dir(store) || cur == store {
					return "", nil
				}
				return store, nil
			}
			if err := os.Mkdir(cur, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			fi, err = os.Lstat(cur)
		}
		if err != nil {
			return "", err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is not a plain directory; refusing", cur)
		}
	}
	return store, nil
}

// readStrict decodes a small regular 0600 file at path into v, opened
// without following a symlink; the opened file must be regular and 0600.
func readStrict(path string, v any) error {
	f, fi, err := fsq.OpenRegularNoFollowBounded(path, maxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNone
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if fi.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s is not a small regular 0600 file; refusing", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
