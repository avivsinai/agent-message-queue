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
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
	BoundAt string `json:"bound_at"`
}

// Mailbox reports whether b delivers through an AMQ handle.
func (b Binding) Mailbox() bool { return b.Carrier == CarrierMailbox }

// Same reports whether two bindings name the same destination.
func (b Binding) Same(o Binding) bool {
	if b.Mailbox() != o.Mailbox() {
		return false
	}
	if b.Mailbox() {
		return b.Root == o.Root && b.Handle == o.Handle
	}
	return b.Root == o.Root && b.Target == o.Target && b.NativeSession == o.NativeSession
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
var ErrNone = errors.New("no session is bound; run /amq-remote in a session")

// Path is the binding file: $AMQ_REMOTE_BINDING, or ~/.amq/remote/binding.json.
func Path() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvPath)); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%s must be absolute", EnvPath)
		}
		return filepath.Clean(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".amq", "remote", "binding.json"), nil
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

// Remove deletes the binding when match accepts it, all under the same lock
// as Write, so a concurrent attach is never undone by an older off.
// A nil match removes any binding.
func Remove(match func(Binding) bool) (bool, error) {
	removed := false
	err := transact(func(path string) error {
		b, err := read(path)
		if errors.Is(err, ErrNone) {
			return nil
		}
		if err != nil {
			return err
		}
		if match != nil && !match(b) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed = true
		return nil
	})
	return removed, err
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
// With create, missing directories are made one at a time with mode 0700.
func confinedPath(create bool) (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	base := dir
	if os.Getenv(EnvPath) == "" {
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
		// An override must be canonical: no symlink anywhere on its chain
		// (codex #885 r2 P2). EvalSymlinks returns a different path exactly
		// when some component is a symlink.
		parent := filepath.Dir(dir)
		if resolved, err := filepath.EvalSymlinks(parent); err != nil || resolved != parent {
			return "", fmt.Errorf("%s directory %s must have no symlink on its path; refusing", EnvPath, parent)
		}
		cur, parts = parent, []string{filepath.Base(dir)}
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

// read opens the leaf no-follow and bounded, and checks it is the file the
// lstat gate saw.
func read(path string) (Binding, error) {
	if !noFollowSupported {
		return Binding{}, errors.New("the binding needs a no-follow file open, which this platform lacks")
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Binding{}, ErrNone
	}
	if err != nil {
		return Binding{}, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxBindingBytes {
		return Binding{}, fmt.Errorf("binding %s is not a small regular file; refusing", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollowFlag, 0)
	if err != nil {
		return Binding{}, err
	}
	defer func() { _ = f.Close() }()
	if fi2, err := f.Stat(); err != nil || !os.SameFile(fi, fi2) {
		return Binding{}, fmt.Errorf("binding %s was replaced while opening; refusing", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBindingBytes+1))
	if err != nil {
		return Binding{}, err
	}
	if len(raw) > maxBindingBytes {
		return Binding{}, fmt.Errorf("binding %s is larger than %d bytes; refusing", path, maxBindingBytes)
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
