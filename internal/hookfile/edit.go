// Package hookfile edits the JSON hooks files of coding harnesses (a
// {"hooks":{event:[group...]}} settings file, or a Cursor hooks file) by
// byte surgery under a per-file lock.
package hookfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// Every AMQ write to a hooks file goes through Edit. It holds one
// advisory lock per file under ~/.amq/locks, so two AMQ writers never lose
// each other's entry. It also reads the file again just before the rename
// and applies the edit again when the bytes changed. That narrows, and does
// not close, the window of a writer that takes no lock (an older AMQ, or
// another program): its write between that re-read and the rename is lost.
// A rewrite keeps the file's permission bits; a new file is 0600.

// settingsEditTries bounds how often one edit is applied again because the
// file changed under it.
const settingsEditTries = 5

// Edit applies edit to the bytes of the settings file at path and
// writes the result atomically, and reports whether it wrote; a nil result
// writes nothing. A missing file is started as seed, and is left alone when
// seed is nil.
func Edit(path string, seed []byte, edit func([]byte) ([]byte, error)) (bool, error) {
	var wrote bool
	err := WithLock(path, func() error {
		var err error
		wrote, err = EditLocked(path, seed, edit)
		return err
	})
	return wrote, err
}

// EditLocked is Edit for a caller that holds the file's
// lock.
func EditLocked(path string, seed []byte, edit func([]byte) ([]byte, error)) (bool, error) {
	for range settingsEditTries {
		raw, exists, err := Read(path)
		if err != nil {
			return false, err
		}
		if !exists {
			if seed == nil {
				return false, nil
			}
			raw = seed
		}
		out, err := edit(raw)
		if err != nil {
			return false, fmt.Errorf("settings %s: %w", path, err)
		}
		if out == nil {
			return false, nil // nothing changed
		}
		if !json.Valid(out) {
			// Defense in depth: never write a file the harness cannot parse.
			return false, fmt.Errorf("settings %s: edit would produce invalid JSON; refusing", path)
		}
		again, stillExists, err := Read(path)
		if err != nil {
			return false, err
		}
		if stillExists != exists || exists && !bytes.Equal(again, raw) {
			continue // another writer changed it: apply the edit to its bytes
		}
		perm := os.FileMode(0o600)
		if exists {
			if info, err := os.Lstat(path); err == nil {
				perm = info.Mode().Perm()
			}
		} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return false, fmt.Errorf("settings %s: %w", path, err)
		}
		if _, err := fsq.WriteFileAtomicFollowLinks(filepath.Dir(path), filepath.Base(path), out, perm); err != nil {
			return false, fmt.Errorf("settings %s: %w", path, err)
		}
		return true, nil
	}
	return false, fmt.Errorf("settings %s: it changed during each of %d edits; run again", path, settingsEditTries)
}

// maxSettingsBytes bounds a hooks file read (a real file is a few KiB).
const maxSettingsBytes = 4 << 20

// errUnsupportedPlatform is the refusal on a platform without a no-follow,
// non-blocking open (Windows).
var errUnsupportedPlatform = errors.New("hooks file: unsupported on this platform (no no-follow file open)")

// ReadBounded reads the hooks file at path no-follow within the writer's
// size bound. os.ErrNotExist passes through unwrapped.
func ReadBounded(path string) ([]byte, error) {
	if runtime.GOOS == "windows" {
		return nil, errUnsupportedPlatform
	}
	return fsq.ReadRegularNoFollowBounded(path, maxSettingsBytes)
}

// Read reads the settings file at path; exists is false when there
// is no file.
func Read(path string) (raw []byte, exists bool, err error) {
	raw, err = ReadBounded(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("settings %s: %w", path, err)
	}
	return raw, true, nil
}

// WithLock runs fn while it holds the lock of the settings file at
// path: ~/.amq/locks/settings-<key>.lock, where key names the file with its
// directory's symlinks resolved, so one file reached two ways has one lock.
// A platform with no advisory lock runs fn without one.
func WithLock(path string, fn func() error) error {
	return WithLockWait(path, LockWait, fn)
}

// ErrBusy is the refusal of a writer that did not get a settings
// file's lock within its wait.
var ErrBusy = errors.New("another AMQ writer holds the settings file's lock")

const (
	// LockWait bounds how long a command waits for a lock.
	LockWait = 10 * time.Second
	// LockTry bounds how long the machine agent's tick waits: a
	// busy file is checked again on the next tick.
	LockTry = 50 * time.Millisecond
)

// WithLockWait is WithLock that gives up after wait with
// ErrBusy, without running fn.
func WithLockWait(path string, wait time.Duration, fn func() error) error {
	if !lock.AdvisoryLockAvailable() {
		return fn()
	}
	home, err := amqhome.Dir()
	if err != nil {
		return fmt.Errorf("settings lock: %w", err)
	}
	dir := filepath.Join(home, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("settings lock: %w", err)
	}
	lockPath := filepath.Join(dir, "settings-"+Key(path)+".lock")
	deadline := time.Now().Add(wait)
	err = lock.WithExclusiveFileLockUntil(lockPath, func() bool { return time.Now().After(deadline) }, fn)
	if errors.Is(err, lock.ErrStopped) {
		return fmt.Errorf("settings %s: %w (%s, waited %s); run again once it is done", path, ErrBusy, lockPath, wait)
	}
	return err
}

// Canonical is path with its directory's symlinks resolved, or
// clean when the directory does not resolve.
func Canonical(path string) string {
	path = filepath.Clean(path)
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(dir, filepath.Base(path))
	}
	return path
}

// Key names the settings file at path by a hash of Canonical(path).
func Key(path string) string {
	sum := sha256.Sum256([]byte(Canonical(path)))
	return hex.EncodeToString(sum[:12])
}
