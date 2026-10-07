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
// handle replaced it. Records are never deleted by AMQ, so a revoked or
// damaged record stays distinguishable from a thread AMQ never managed.
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
	"strings"

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
// every directory it created is synced into its parent, so a thread is
// current only after its record is durable. The newest publication for a
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
		if _, err := fsq.WriteFileAtomic(filepath.Join(dir, "threads"), r.Thread+".json", append(raw, '\n'), 0o600); err != nil {
			return fmt.Errorf("record the Codex thread identity: %w", err)
		}
		if _, err := fsq.WriteFileAtomic(filepath.Join(dir, "current"), currentKey(r)+".json", append(current, '\n'), 0o600); err != nil {
			return fmt.Errorf("record the current Codex thread: %w", err)
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

// storeDir is $HOME/.amq/codex-threads with its threads/ and current/
// directories. Every directory below the home must be a plain directory,
// not a symlink. With create, missing ones are made with mode 0700 and each
// is synced into its parent before use; without, a missing store returns "".
func storeDir(create bool) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cur := home
	for _, part := range []string{".amq", "codex-threads", "threads", "current"} {
		parent := cur
		if part == "current" {
			parent = filepath.Dir(cur)
		}
		cur = filepath.Join(parent, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				if part == "threads" || part == "current" {
					return filepath.Dir(cur), nil
				}
				return "", nil
			}
			if err := os.Mkdir(cur, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			if err := fsq.SyncDir(parent); err != nil {
				return "", fmt.Errorf("sync %s: %w", parent, err)
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
	return filepath.Dir(cur), nil
}

// readStrict decodes a small regular 0600 file at path into v, opened
// without following a symlink and checked against the file it inspected.
func readStrict(path string, v any) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNone
	}
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || fi.Size() > maxBytes {
		return fmt.Errorf("%s is not a small regular 0600 file; refusing", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if fi2, err := f.Stat(); err != nil || !os.SameFile(fi, fi2) {
		return fmt.Errorf("%s was replaced while opening; refusing", path)
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
