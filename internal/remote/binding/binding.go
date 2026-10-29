// Package binding stores which live session the per-user Buzz agent drives.
// `amq-remote attach --self` writes it at the moment the owner consents
// inside that session; amq-acp in binding mode reads it on every prompt.
// A request keeps the binding it was submitted under, so a later attach
// never moves work that is already running.
package binding

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// EnvPath overrides the binding file location (tests, alternate homes).
const EnvPath = "AMQ_REMOTE_BINDING"

// maxBindingBytes bounds a binding file read.
const maxBindingBytes = 64 * 1024

// Carriers. A mailbox binding delivers each prompt as an AMQ message to
// Handle and completes on its final reply; the handle is the identity, and
// noticing the message is the handle owner's business (a wake, a built-in
// consumer, monitor, or the next drain). A native binding submits into the
// exact native session through amq-remote. The zero value is native.
const (
	CarrierMailbox = "mailbox"
	CarrierNative  = "native"
)

// Binding names what the Buzz agent drives: an AMQ handle, or one native
// session.
type Binding struct {
	// Carrier is CarrierMailbox or CarrierNative (empty means native).
	Carrier string `json:"carrier,omitempty"`
	// Root is the AMQ root: the handle's queue, or the root whose
	// amq-remote endpoint owns Target.
	Root string `json:"root"`
	// Handle is the AMQ handle a mailbox binding delivers to.
	Handle string `json:"handle,omitempty"`
	// Target is the endpoint target id, for example claude:12345.
	Target string `json:"target,omitempty"`
	// NativeSession is the native identity the owner shared. The endpoint
	// refuses every command once the target is attached to another one.
	NativeSession string `json:"native_session,omitempty"`
	// Display is human text for replies; never an identity.
	Display string `json:"display,omitempty"`
	// Identity is the session identity that decides name reuse (see
	// SessionIdentity); empty in a binding written before it was recorded.
	Identity string `json:"identity,omitempty"`
	// Harness is the harness kind that runs the session (claude, codex, pi);
	// empty when it was not detected.
	Harness string `json:"harness,omitempty"`
	// Holder is the harness process of the session, recorded so that a
	// session that ended can be proven ended; zero when it is not known.
	Holder launch.HolderIdentity `json:"holder,omitzero"`
	// Label is the human name of the session and of its Buzz agent:
	// "<harness> · <project>/<session> @ <machine>".
	Label string `json:"label,omitempty"`
	// Machine is the short name of the machine that holds the binding.
	Machine string `json:"machine,omitempty"`
	// AppServer is the Codex app-server thread that runs the session, which
	// has no session process to hold; zero for other harnesses.
	AppServer AppServerThread `json:"app_server,omitzero"`
	// Name is the binding's name; it is the file name, not stored in it.
	Name    string `json:"-"`
	BoundAt string `json:"bound_at"`
	// Removed is set only in a tombstone: why the binding was removed
	// (RemovedEnded, RemovedDetached or RemovedMoved). Empty in a tombstone
	// written before it was recorded.
	Removed string `json:"removed,omitempty"`
}

// Why a binding was removed, recorded in its tombstone.
const (
	RemovedEnded    = "ended"    // its session was proven ended
	RemovedDetached = "detached" // the owner detached it; the session may still run
	RemovedMoved    = "moved"    // the same session attached again under another name
)

// Mailbox reports whether b delivers through an AMQ handle.
func (b Binding) Mailbox() bool { return b.Carrier == CarrierMailbox }

// Same reports whether two bindings name the same destination.
func (b Binding) Same(o Binding) bool {
	if b.Mailbox() != o.Mailbox() {
		return false
	}
	// A root is an identity, so spelling (a trailing slash) does not matter,
	// including for values read back from an older file.
	sameRoot := filepath.Clean(b.Root) == filepath.Clean(o.Root)
	if b.Mailbox() {
		return sameRoot && b.Handle == o.Handle
	}
	return sameRoot && b.Target == o.Target && b.NativeSession == o.NativeSession
}

// SessionIdentity is the session b serves, the key that decides whether an
// attach reuses b's name: the recorded Identity, else one derived from the
// destination, so a binding written before Identity was recorded keeps its
// name for the same session.
func (b Binding) SessionIdentity() string {
	switch {
	case b.Identity != "":
		return b.Identity
	case b.Mailbox():
		return "mailbox:" + b.Handle + "@" + canonicalRoot(b.Root)
	}
	return "native:" + b.Target + "\x00" + b.NativeSession + "\x00" + canonicalRoot(b.Root)
}

// NativeIdentity is the session identity of a native session: its harness,
// root and working directory, symlinks resolved, not its process or native
// session id, so the same session coming back to the same place is the same
// identity.
func NativeIdentity(kind, root, cwd string) string {
	return "native:" + kind + "\x00" + canonicalRoot(cwd) + "\x00" + canonicalRoot(root)
}

// AppServerThread names a thread of a Codex app-server daemon, and the
// daemon process that had it loaded when it was bound: only that same daemon
// saying the thread is no longer loaded proves the session ended.
type AppServerThread struct {
	Socket string                `json:"socket"`
	Thread string                `json:"thread"`
	Daemon launch.HolderIdentity `json:"daemon,omitzero"`
}

// Inspector reports a binding's liveness with its evidence. Binding.Liveness
// is the inspector that needs no harness; a caller that can ask a harness
// (a Codex app-server) passes its own.
type Inspector func(Binding) (State, string)

// State is the liveness of a bound session.
type State string

// Liveness states. Only Ended is proof; Unknown is never treated as ended.
const (
	Live    State = "live"
	Ended   State = "ended"
	Unknown State = "unknown"
)

// Liveness reports whether b's session still runs, with the evidence. It is
// Ended only when that is proven: the root is gone, or the holder process is
// not running, runs with another start token, or ran in another boot. A
// binding without a holder is Unknown, except a native Claude binding from
// before holders were recorded, which is Ended when the pid in its target is
// not running.
func (b Binding) Liveness() (State, string) {
	if _, err := os.Stat(b.Root); errors.Is(err, os.ErrNotExist) {
		return Ended, "root is gone"
	} else if err != nil {
		return Unknown, err.Error()
	}
	h := b.Holder
	if pid, ok := strings.CutPrefix(b.Target, "claude:"); ok && h.PID <= 0 && !b.Mailbox() {
		h.PID, _ = strconv.Atoi(pid)
	}
	if h.PID <= 0 {
		return Unknown, "no holder recorded"
	}
	switch state, evidence := launch.InspectHolder(h); state {
	case launch.LeaseStale:
		return Ended, evidence
	case launch.LeaseValid:
		return Live, evidence
	default:
		return Unknown, evidence
	}
}

// Machine is this machine's short name for labels: the first label of the
// host name, lowercased, at most 32 characters of [a-z0-9-]; empty when the
// host name is unknown.
func Machine() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(host, ".")
	var b strings.Builder
	for _, r := range strings.ToLower(first) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	return out
}

// canonicalRoot is root with its symlinks resolved, so one root reached
// through two spellings is one identity; the clean path when it does not
// resolve.
func canonicalRoot(root string) string {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return resolved
	}
	return filepath.Clean(root)
}

// Valid reports whether b is complete for its carrier.
func (b Binding) Valid() error {
	if !filepath.IsAbs(b.Root) {
		return errors.New("binding root must be absolute")
	}
	switch b.Carrier {
	case CarrierMailbox:
		// A mailbox binding never names a native session, so nothing that
		// keys on one (the Claude Stop receiver) treats it as native.
		if b.Target != "" || b.NativeSession != "" {
			return errors.New("mailbox binding must not name a native target or session")
		}
		return fsq.ValidateHandle(b.Handle)
	case "", CarrierNative:
		if b.Target == "" || b.NativeSession == "" {
			return errors.New("native binding needs a target and a native session")
		}
		return nil
	}
	return fmt.Errorf("unknown binding carrier %q", b.Carrier)
}

// ErrNone is returned when no session is bound.
var ErrNone = errors.New("no session is bound; run /amq-remote (Claude Code) or $amq-remote (Codex) in a session")

// Path is the binding file: $AMQ_REMOTE_BINDING, or ~/.amq/remote/binding.json.
func Path() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvPath)); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be absolute", EnvPath)
		}
		return filepath.Clean(p), nil
	}
	dir, err := amqhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "remote", "binding.json"), nil
}

// Read returns the current binding, or ErrNone.
func Read() (Binding, error) {
	path, err := confinedPath(false)
	if err != nil {
		return Binding{}, err
	}
	return read(path)
}

// Write replaces the binding atomically with mode 0600.
func Write(b Binding) error {
	if err := b.Valid(); err != nil {
		return err
	}
	if b.BoundAt == "" {
		b.BoundAt = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return transact(func(path string) error {
		_, err := fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), append(raw, '\n'), 0o600)
		return err
	})
}

// transact runs fn under the binding's interprocess lock, after confining
// its directory.
func transact(fn func(path string) error) error {
	if !lock.AdvisoryLockAvailable() {
		return errors.New("refusing to change the binding without an advisory file lock")
	}
	path, err := confinedPath(true)
	if err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(path+".lock", func() error { return fn(path) })
}

// confinedPath returns the binding path after checking that no directory
// between the home (or the override's parent) and the file is a symlink.
// With create, the AMQ home is made and checked by amqhome and missing
// directories below it are made one at a time with mode 0700.
func confinedPath(create bool) (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	base := dir
	if os.Getenv(EnvPath) == "" {
		if create {
			if _, err := amqhome.EnsureDir(); err != nil {
				return "", err
			}
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = home
	}
	rel, err := filepath.Rel(base, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("binding directory %s escapes %s", dir, base)
	}
	// Under the home, check each directory below it (the home itself may be
	// a symlink on some systems). An override checks its own directory.
	cur := base
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	if os.Getenv(EnvPath) != "" {
		// An override must be canonical: no symlink anywhere on its chain,
		// below an ancestor this user owns. The missing levels are made by
		// the loop below, one Mkdir + Lstat at a time:
		// MkdirAll would follow a symlink swapped in after this check.
		existing, err := canonicalAncestor(dir)
		if err != nil {
			return "", err
		}
		if existing != dir && !create {
			return path, nil // nothing bound yet; read reports ErrNone
		}
		cur, parts = existing, []string{}
		if r, _ := filepath.Rel(existing, dir); r != "." {
			parts = strings.Split(r, string(filepath.Separator))
		}
		if overrideCheckedHook != nil {
			overrideCheckedHook()
		}
	}
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := os.Mkdir(cur, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			fi, err = os.Lstat(cur)
		}
		if errors.Is(err, os.ErrNotExist) && !create {
			return path, nil // nothing bound yet; read reports ErrNone
		}
		if err != nil {
			return "", err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("binding directory %s is not a plain directory; refusing", cur)
		}
	}
	return path, nil
}

// read reads the leaf no-follow and bounded; the opened file must be a
// regular file.
func read(path string) (Binding, error) {
	if !noFollowSupported {
		return Binding{}, errors.New("the binding needs a no-follow file open, which this platform lacks")
	}
	raw, err := fsq.ReadRegularNoFollowBounded(path, maxBindingBytes)
	if errors.Is(err, os.ErrNotExist) {
		return Binding{}, ErrNone
	}
	if err != nil {
		return Binding{}, fmt.Errorf("binding: %w", err)
	}
	var b Binding
	if err := json.Unmarshal(raw, &b); err != nil {
		return Binding{}, fmt.Errorf("binding %s: %w", path, err)
	}
	if err := b.Valid(); err != nil {
		return Binding{}, fmt.Errorf("binding %s: %v; run /amq-remote again", path, err)
	}
	return b, nil
}

// Named bindings: one per shared session, so each session has its own Buzz
// agent. They live in a bindings directory beside the legacy single binding
// file, which still reads as the
// name "default". All writes share the binding lock.

// LegacyName is the name the single legacy binding file reads as.
const LegacyName = "default"

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// overrideCheckedHook runs between the override's ancestor check and the
// creation of its missing levels; tests swap a symlink in there.
var overrideCheckedHook func()

// canonicalAncestor returns dir, or its nearest existing ancestor when dir is
// missing, after checking that it is a directory with no symlink on its path,
// owned by this user and not writable by group or others.
func canonicalAncestor(dir string) (string, error) {
	existing := dir
	fi, err := os.Lstat(existing)
	for errors.Is(err, os.ErrNotExist) && filepath.Dir(existing) != existing {
		existing = filepath.Dir(existing)
		fi, err = os.Lstat(existing)
	}
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(existing); err != nil || resolved != existing {
		return "", fmt.Errorf("%s directory %s must have no symlink on its path; refusing", EnvPath, existing)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s directory %s is not a directory; refusing", EnvPath, existing)
	}
	if err := ownedByUser(fi); err != nil {
		return "", fmt.Errorf("%s directory %s %v; refusing", EnvPath, existing, err)
	}
	return existing, nil
}

// ValidName reports whether name can name a binding.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("binding name %q must match [a-z0-9][a-z0-9_-]{0,63}", name)
	}
	return nil
}

// SanitizeName turns free text (a handle and project) into a binding name.
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 64 {
		out = strings.Trim(out[:64], "-_")
	}
	if out == "" {
		return LegacyName
	}
	return out
}

// namedDir is the bindings directory, confined like the binding file.
func namedDir(create bool) (string, error) {
	path, err := confinedPath(create)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(path), "bindings")
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return dir, nil
		}
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil {
		return "", err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("bindings directory %s is not a plain directory; refusing", dir)
	}
	return dir, nil
}

// ReadNamed returns the binding called name, or ErrNone.
func ReadNamed(name string) (Binding, error) {
	if name == LegacyName {
		if b, err := Read(); !errors.Is(err, ErrNone) {
			b.Name = LegacyName
			return b, err
		}
	}
	if err := ValidName(name); err != nil {
		return Binding{}, err
	}
	dir, err := namedDir(false)
	if err != nil {
		return Binding{}, err
	}
	b, err := read(filepath.Join(dir, name+".json"))
	if err != nil {
		return Binding{}, err
	}
	b.Name = name
	return b, nil
}

// NameTakenError is returned by WriteNamedIf when the binding of that name is
// not one it may replace.
type NameTakenError struct{ Name string }

func (e *NameTakenError) Error() string {
	return fmt.Sprintf("binding name %q already names another session; attach with --name <unique-name>", e.Name)
}

// WriteNamed adds or replaces only the binding called b.Name.
func WriteNamed(b Binding) error { return writeNamed(b, nil) }

// WriteNamedIf writes the binding called b.Name when no binding of that name
// exists or allow accepts the one that does; otherwise it refuses with
// *NameTakenError. The check and the write share one transaction.
func WriteNamedIf(b Binding, allow func(old Binding) bool) error { return writeNamed(b, allow) }

// WriteSession writes b under the name b's own destination already holds, else
// under a name its session identity holds, else under base, else under the
// first of base-2, base-3, ... that is free or held by the same identity, and
// returns the name written and whether this session already held it (reused).
// A binding of the same identity with another
// destination is taken over only when its holder is b's own or proven ended:
// a live or unknown holder may be a concurrent session in the same place,
// which keeps its name, so uncertain state is never weakened. A name whose
// binding was pruned stays held by its identity through its tombstone (see
// PruneEnded): that identity takes it back, and no other identity takes it.
// Another session's binding is never replaced, so a session never inherits
// another session's Buzz agent, and the same session always gets back its own
// name. The choice and the write share one transaction.
func WriteSession(b Binding, base string, inspect Inspector) (name string, reused bool, err error) {
	if err := ValidName(base); err != nil {
		return "", false, err
	}
	if err := b.Valid(); err != nil {
		return "", false, err
	}
	id := b.SessionIdentity()
	b.Identity = id
	if b.BoundAt == "" {
		b.BoundAt = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return "", false, err
	}
	err = transact(func(string) error {
		dir, err := namedDir(true)
		if err != nil {
			return err
		}
		bound, err := scanNamed(dir)
		if err != nil {
			return err
		}
		tombs, err := scanTombstones(dir)
		if err != nil {
			return err
		}
		reusable := func(o Binding) bool {
			if o.SessionIdentity() != id {
				return false
			}
			// A mailbox identity is its destination, so it is never a
			// concurrent session, whatever spelling its root has.
			if b.Mailbox() || b.Holder.PID > 0 && o.Holder == b.Holder {
				return true
			}
			state, _ := inspect(o)
			return state == Ended
		}
		held := map[string]Binding{}
		for _, o := range bound {
			held[o.Name] = o
			if o.Same(b) && name == "" {
				name = o.Name
			}
		}
		for _, o := range bound {
			if name == "" && reusable(o) {
				name = o.Name
			}
		}
		for _, t := range tombs {
			if o, taken := held[t.Name]; name == "" && t.SessionIdentity() == id && (!taken || reusable(o)) {
				name = t.Name
			}
		}
		tombOf := map[string]string{}
		for _, t := range tombs {
			tombOf[t.Name] = t.SessionIdentity()
		}
		for n := 1; name == ""; n++ {
			candidate := base
			if n > 1 {
				suffix := fmt.Sprintf("-%d", n)
				candidate = strings.TrimRight(base[:min(len(base), 64-len(suffix))], "-_") + suffix
			}
			// A file that does not read as a binding or a tombstone is
			// taken, never overwritten.
			o, taken := held[candidate]
			// Any other Lstat error means the directory cannot be read, so
			// no name is provably free: fail closed.
			_, statErr := os.Lstat(filepath.Join(dir, candidate+".json"))
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
			_, tombErr := os.Lstat(filepath.Join(dir, candidate+tombstoneSuffix))
			if tombErr != nil && !errors.Is(tombErr, os.ErrNotExist) {
				return tombErr
			}
			tombID, tombed := tombOf[candidate]
			free := !taken && statErr != nil && (tombed && tombID == id || !tombed && tombErr != nil)
			if taken && reusable(o) || free {
				name = candidate
			}
		}
		_, wasBound := held[name]
		_, wasTomb := tombOf[name]
		reused = wasBound || wasTomb
		if _, err := fsq.WriteFileAtomic(dir, name+".json", append(raw, '\n'), 0o600); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(dir, name+tombstoneSuffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return name, reused, nil
}

// Replace writes now in place of the binding called was.Name when that
// binding is still exactly was, and reports whether it wrote. A binding that
// changed or was removed meanwhile is left as it is. The check and the write
// share one transaction.
func Replace(was, now Binding) (bool, error) {
	if err := ValidName(was.Name); err != nil {
		return false, err
	}
	if err := now.Valid(); err != nil {
		return false, err
	}
	raw, err := json.MarshalIndent(now, "", "  ")
	if err != nil {
		return false, err
	}
	replaced := false
	err = transact(func(string) error {
		dir, err := namedDir(false)
		if err != nil {
			return err
		}
		old, err := read(filepath.Join(dir, was.Name+".json"))
		if errors.Is(err, ErrNone) {
			return nil
		}
		if err != nil {
			return err
		}
		if old.Name = was.Name; old != was {
			return nil
		}
		if _, err := fsq.WriteFileAtomic(dir, was.Name+".json", append(raw, '\n'), 0o600); err != nil {
			return err
		}
		replaced = true
		return nil
	})
	return replaced, err
}

func writeNamed(b Binding, allow func(old Binding) bool) error {
	if err := ValidName(b.Name); err != nil {
		return err
	}
	if err := b.Valid(); err != nil {
		return err
	}
	if b.BoundAt == "" {
		b.BoundAt = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return transact(func(string) error {
		dir, err := namedDir(true)
		if err != nil {
			return err
		}
		if allow != nil {
			switch old, err := read(filepath.Join(dir, b.Name+".json")); {
			case errors.Is(err, ErrNone):
			case err != nil:
				return err
			case !allow(old):
				return &NameTakenError{Name: b.Name}
			}
		}
		_, err = fsq.WriteFileAtomic(dir, b.Name+".json", append(raw, '\n'), 0o600)
		return err
	})
}

// List returns every binding, named ones sorted by name, then the legacy one.
func List() ([]Binding, error) {
	dir, err := namedDir(false)
	if err != nil {
		return nil, err
	}
	out, err := scanNamed(dir)
	if err != nil {
		return nil, err
	}
	if b, err := Read(); err == nil {
		b.Name = LegacyName
		out = append(out, b)
	}
	return out, nil
}

// tombstoneSuffix names the tombstone a removed binding leaves:
// <name>.ended.json, the binding as it was when it was removed.
const tombstoneSuffix = ".ended.json"

// IsTombstone reports whether a file name in the bindings directory is a
// tombstone. A tombstone is never a binding, so it is no evidence that a
// session is bound: every reader of the directory skips it.
func IsTombstone(fileName string) bool { return strings.HasSuffix(fileName, tombstoneSuffix) }

// PruneEnded removes every binding whose session inspect proves ended and
// returns the names removed. Each binding is checked as read under the
// binding lock, so a session that attached again meanwhile is kept, and an
// unknown one is kept too. Like every removal, it leaves tombstones (see
// Retire).
func PruneEnded(inspect Inspector) ([]string, error) {
	return Retire(RemovedEnded, func(b Binding) bool {
		state, _ := inspect(b)
		return state == Ended
	})
}

// Retire removes every binding match accepts, as read under the binding lock,
// and returns the names removed. A named binding is replaced by its tombstone,
// which keeps its name held by its session identity, so a later session never
// inherits the removed session's Buzz agent through a freed name. The
// tombstone, which records why (one of the Removed values), is written
// before the binding is removed, so a reader that finds the binding gone
// finds the reason too; a stop between the two leaves the binding bound.
// Tombstones are never removed. Their number is bounded by the
// distinct session identities, and an identity derived from a target (a link
// share, or a native session whose working directory is unknown) names a
// process, so each such session leaves one. The legacy single binding has no
// name to hold and is deleted.
func Retire(why string, match func(Binding) bool) ([]string, error) {
	var removed []string
	err := transact(func(path string) error {
		dir, err := namedDir(false)
		if err != nil {
			return err
		}
		bound, err := scanNamed(dir)
		if err != nil {
			return err
		}
		for _, b := range bound {
			if !match(b) {
				continue
			}
			b.Removed = why
			raw, err := json.MarshalIndent(b, "", "  ")
			if err != nil {
				return err
			}
			if _, err := fsq.WriteFileAtomic(dir, b.Name+tombstoneSuffix, append(raw, '\n'), 0o600); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(dir, b.Name+".json")); err != nil {
				return err
			}
			removed = append(removed, b.Name)
		}
		if b, err := read(path); err == nil {
			if b.Name = LegacyName; match(b) {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				removed = append(removed, LegacyName)
			}
		}
		return nil
	})
	return removed, err
}

// Tombstones returns the binding each tombstone recorded, named by its
// binding name and sorted by it.
func Tombstones() ([]Binding, error) {
	dir, err := namedDir(false)
	if err != nil {
		return nil, err
	}
	return scanTombstones(dir)
}

// scanTombstones reads the tombstones in dir, sorted by name. A file that
// does not read as a binding is skipped.
func scanTombstones(dir string) ([]Binding, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var out []Binding
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), tombstoneSuffix)
		if !IsTombstone(e.Name()) || e.IsDir() || ValidName(name) != nil {
			continue
		}
		b, err := read(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		b.Name = name
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// scanNamed reads the named bindings in dir, sorted by name. A file that does
// not read as a binding is skipped.
func scanNamed(dir string) ([]Binding, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var out []Binding
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || IsTombstone(e.Name()) || name == e.Name() || ValidName(name) != nil {
			continue
		}
		b, err := read(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		b.Name = name
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemoveMatching removes every binding match accepts, named and legacy,
// under the binding lock, and returns the names removed.
func RemoveMatching(match func(Binding) bool) ([]string, error) {
	var removed []string
	err := transact(func(path string) error {
		dir, err := namedDir(false)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, e := range entries {
			name := strings.TrimSuffix(e.Name(), ".json")
			if e.IsDir() || IsTombstone(e.Name()) || name == e.Name() || ValidName(name) != nil {
				continue
			}
			b, err := read(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			b.Name = name
			if !match(b) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removed = append(removed, name)
		}
		if b, err := read(path); err == nil && func() bool { b.Name = LegacyName; return match(b) }() {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removed = append(removed, LegacyName)
		}
		return nil
	})
	return removed, err
}
