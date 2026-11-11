// Package claudeidentity keeps the AMQ identity bound to one Claude Code
// session. A hook or command of the session finds its identity here by the
// Claude session id, without any AM_* variable in its environment.
//
// The store is per user ($HOME/.amq/claude-sessions, mode 0700): one record
// per Claude session under sessions/, and under current/ the session that
// holds each (root identity, handle). A record whose session is no longer
// the holder of its handle is not current: another session took the handle
// after this one released it, rebound to a different handle, or stopped
// running. A released record stays as a tombstone, so a released or damaged
// record stays distinguishable from a session AMQ never bound. Records are
// never deleted by AMQ.
// The store keeps the owner user's trust boundary; it is no defense against
// another process running as the same user.
package claudeidentity

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

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// Record is the AMQ identity bound to one Claude Code session.
type Record struct {
	Schema        int    `json:"schema"`
	ClaudeSession string `json:"claude_session"`
	ConfigDir     string `json:"config_dir"`
	Released      bool   `json:"released,omitempty"`
	launch.Identity
}

// ErrNone means no AMQ identity is bound to the session: it was never bound,
// or it released its binding.
var ErrNone = errors.New("no AMQ identity is bound to this Claude session")

// HeldError means another Claude session that still runs holds the handle.
type HeldError struct{ ClaudeSession string }

func (e *HeldError) Error() string {
	return fmt.Sprintf("Claude session %s holds this handle; release it there first", e.ClaudeSession)
}

const (
	schema   = 1
	maxBytes = 4096
)

var sessionRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidSession reports whether id has the shape of a Claude Code session id.
func ValidSession(id string) bool { return sessionRe.MatchString(id) }

// Bind durably records r, then makes r.ClaudeSession the holder of its
// (root identity, handle), under the store lock so the record and its
// holder marker change together. Both writes are atomic and synced, and the
// store's directories are synced into their parents first. Bind refuses with
// *HeldError and writes nothing when another unreleased session holds the
// handle and holderRuns reports that the session still runs. A holder that
// no longer runs cannot release its handle, so r's session replaces it: the
// holder marker moves, and the old record stays unreleased, so the old
// session reads as no longer the holder and not as never bound. A session
// that binds a different handle frees its old one. Binding the same session
// to the same identity again succeeds.
func Bind(r Record, holderRuns func(holder Record) bool) error {
	r.Schema = schema
	r.Released = false
	if err := r.check(); err != nil {
		return err
	}
	dir, err := storeDir(true)
	if err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(filepath.Join(dir, ".lock"), func() error {
		if err := syncStoreDirs(dir); err != nil {
			return err
		}
		key := currentKey(r)
		holder, err := readHolder(dir, key)
		if err != nil {
			return err
		}
		if holder != "" && holder != r.ClaudeSession {
			var other Record
			if readStrict(sessionPath(dir, holder), &other) == nil && other.check() == nil &&
				other.ClaudeSession == holder && !other.Released && currentKey(other) == key && holderRuns(other) {
				return &HeldError{ClaudeSession: holder}
			}
		}
		// A session bound to another handle gives that handle up. The marker
		// cannot be deleted safely, so it keeps no holder.
		var old Record
		if readStrict(sessionPath(dir, r.ClaudeSession), &old) == nil && old.check() == nil &&
			old.ClaudeSession == r.ClaudeSession && !old.Released && currentKey(old) != key {
			if err := clearHolder(dir, old); err != nil {
				return err
			}
		}
		if err := writeRecord(dir, r); err != nil {
			return err
		}
		return writeHolder(dir, key, r.ClaudeSession)
	})
}

// Release gives up the session's binding under the store lock: the record
// stays with Released set, and the handle has no holder when the session
// was its holder. It returns the record as it was before the release, and
// freed false when another session had taken the handle, which that
// session keeps. A session that is already released returns its record and
// ErrNone.
func Release(claudeSession, configDir string) (out Record, freed bool, err error) {
	if !ValidSession(claudeSession) {
		return Record{}, false, fmt.Errorf("%q is not a Claude session id", claudeSession)
	}
	dir, err := storeDir(false)
	if err != nil {
		return Record{}, false, err
	}
	if dir == "" {
		return Record{}, false, ErrNone
	}
	err = lock.WithExclusiveFileLock(filepath.Join(dir, ".lock"), func() error {
		r, err := readSessionRecord(dir, claudeSession, configDir)
		if err != nil {
			return err
		}
		out = r
		if r.Released {
			return ErrNone
		}
		if err := syncStoreDirs(dir); err != nil {
			return err
		}
		holder, err := readHolder(dir, currentKey(r))
		if err != nil {
			return err
		}
		if holder == claudeSession {
			if err := clearHolder(dir, r); err != nil {
				return err
			}
			freed = true
		}
		released := r
		released.Released = true
		return writeRecord(dir, released)
	})
	if err != nil && !errors.Is(err, ErrNone) {
		return Record{}, false, err
	}
	return out, freed, err
}

// Lookup returns the identity bound to claudeSession under configDir. It
// returns ErrNone when AMQ never bound one or the session released it, and
// an error when the record is damaged, belongs to another Claude config
// directory, or is no longer the holder of its handle.
func Lookup(claudeSession, configDir string) (Record, error) {
	if !ValidSession(claudeSession) {
		return Record{}, fmt.Errorf("%q is not a Claude session id", claudeSession)
	}
	dir, err := storeDir(false)
	if err != nil {
		return Record{}, err
	}
	if dir == "" {
		return Record{}, ErrNone
	}
	r, err := readSessionRecord(dir, claudeSession, configDir)
	if err != nil {
		return Record{}, err
	}
	if r.Released {
		return Record{}, ErrNone
	}
	var cur currentRecord
	if err := readStrict(currentPath(dir, currentKey(r)), &cur); err != nil {
		if errors.Is(err, ErrNone) {
			return Record{}, fmt.Errorf("the Claude session %s has an identity record but no holder record for %s", claudeSession, r.Me)
		}
		return Record{}, err
	}
	if cur.ClaudeSession != claudeSession {
		if !ValidSession(cur.ClaudeSession) {
			return Record{}, fmt.Errorf("the binding of Claude session %s to handle %s in %s is not current; no session holds it", claudeSession, r.Me, r.Root)
		}
		return Record{}, fmt.Errorf("handle %s in %s is now held by Claude session %s; this session %s no longer carries that identity", r.Me, r.Root, cur.ClaudeSession, claudeSession)
	}
	return r, nil
}

// readSessionRecord reads and validates the record of claudeSession and
// checks it belongs to configDir. A missing record is ErrNone.
func readSessionRecord(dir, claudeSession, configDir string) (Record, error) {
	var r Record
	if err := readStrict(sessionPath(dir, claudeSession), &r); err != nil {
		return Record{}, err
	}
	if err := r.check(); err != nil {
		return Record{}, fmt.Errorf("the identity record of Claude session %s is invalid: %v", claudeSession, err)
	}
	if r.ClaudeSession != claudeSession {
		return Record{}, fmt.Errorf("the identity record of Claude session %s names session %s", claudeSession, r.ClaudeSession)
	}
	if resolvedDir(r.ConfigDir) != resolvedDir(configDir) {
		return Record{}, fmt.Errorf("the Claude session %s was bound under config directory %s, not %s", claudeSession, r.ConfigDir, configDir)
	}
	return r, nil
}

// resolvedDir is dir with its symlinks resolved, or dir itself when it
// cannot be resolved, so two spellings of one config directory match.
func resolvedDir(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(dir)
}

type currentRecord struct {
	Schema        int    `json:"schema"`
	ClaudeSession string `json:"claude_session"`
}

func (r Record) check() error {
	switch {
	case r.Schema != schema:
		return fmt.Errorf("unknown schema %d", r.Schema)
	case !ValidSession(r.ClaudeSession):
		return fmt.Errorf("session %q is not a Claude session id", r.ClaudeSession)
	case !filepath.IsAbs(r.ConfigDir) || !filepath.IsAbs(r.Root) || !filepath.IsAbs(r.BaseRoot):
		return errors.New("the Claude config directory and both roots must be absolute")
	case strings.TrimSpace(r.Me) == "" || strings.TrimSpace(r.RootID) == "" || strings.TrimSpace(r.BaseRootID) == "":
		return errors.New("the handle and both root identity tokens are required")
	}
	return nil
}

// currentKey names the holder marker of one handle in one root, by the
// root's persisted key so a recreated root starts a new lineage and a
// reboot does not.
func currentKey(r Record) string {
	sum := sha256.Sum256([]byte(fsq.PersistedTreeKey(r.Root, r.RootID) + "\x00" + r.Me))
	return hex.EncodeToString(sum[:16])
}

// readHolder returns the session named by the holder marker of key, or ""
// when the marker is missing or names no valid session.
func readHolder(dir, key string) (string, error) {
	var cur currentRecord
	if err := readStrict(currentPath(dir, key), &cur); err != nil {
		if errors.Is(err, ErrNone) {
			return "", nil
		}
		return "", err
	}
	if !ValidSession(cur.ClaudeSession) {
		return "", nil
	}
	return cur.ClaudeSession, nil
}

func writeRecord(dir string, r Record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := fsq.WriteStateFile(filepath.Dir(dir), filepath.Join(storeName, "sessions", r.ClaudeSession+".json"), append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("record the Claude session identity: %w", err)
	}
	return nil
}

func writeHolder(dir, key, session string) error {
	raw, err := json.Marshal(currentRecord{Schema: schema, ClaudeSession: session})
	if err != nil {
		return err
	}
	if _, err := fsq.WriteStateFile(filepath.Dir(dir), filepath.Join(storeName, "current", key+".json"), append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("record the holder of the handle: %w", err)
	}
	return nil
}

// clearHolder overwrites the holder marker of r's handle with an empty
// holder when it still names r's session.
func clearHolder(dir string, r Record) error {
	key := currentKey(r)
	holder, err := readHolder(dir, key)
	if err != nil || holder != r.ClaudeSession {
		return err
	}
	return writeHolder(dir, key, "")
}

// syncStoreDirs makes every directory of the store durable in its parent.
// It runs on every publication, not only when the call created the
// directory: an earlier publication may have failed after mkdir.
func syncStoreDirs(dir string) error {
	for _, d := range []string{filepath.Dir(filepath.Dir(dir)), filepath.Dir(dir), dir, filepath.Join(dir, "sessions"), filepath.Join(dir, "current")} {
		if err := fsq.SyncDir(d); err != nil {
			return fmt.Errorf("sync %s: %w", d, err)
		}
	}
	return nil
}

func sessionPath(dir, session string) string {
	return filepath.Join(dir, "sessions", session+".json")
}

func currentPath(dir, key string) string {
	return filepath.Join(dir, "current", key+".json")
}

const storeName = "claude-sessions"

// SessionsDir is the sessions/ directory of the store below home's AMQ
// home. It exists once any session was bound; a hook gate tests a file in
// it before it starts amq.
func SessionsDir(home string) string {
	return filepath.Join(amqhome.Under(home), storeName, "sessions")
}

// storeDir is $HOME/.amq/claude-sessions with its sessions/ and current/
// directories. Every directory below the home must be a plain directory,
// not a symlink. With create, the AMQ home is made and checked by amqhome
// and missing directories below it are made with mode 0700 (the caller then
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
	for _, cur := range []string{store, filepath.Join(store, "sessions"), filepath.Join(store, "current")} {
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				if cur == store {
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
// without following a symlink; the opened file must be regular and 0600. A
// missing file is ErrNone.
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
