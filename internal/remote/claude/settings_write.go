package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// Every AMQ write to a hooks file goes through editSettings. It holds one
// advisory lock per file under ~/.amq/locks, so two AMQ writers never lose
// each other's entry. It also reads the file again just before the rename
// and applies the edit again when the bytes changed. That narrows, and does
// not close, the window of a writer that takes no lock (an older AMQ, or
// another program): its write between that re-read and the rename is lost.
// A rewrite keeps the file's permission bits; a new file is 0600.

// settingsEditTries bounds how often one edit is applied again because the
// file changed under it.
const settingsEditTries = 5

// editSettings applies edit to the bytes of the settings file at path and
// writes the result atomically, and reports whether it wrote; a nil result
// writes nothing. A missing file is started as seed, and is left alone when
// seed is nil.
func editSettings(path string, seed []byte, edit func([]byte) ([]byte, error)) (bool, error) {
	var wrote bool
	err := withSettingsLock(path, func() error {
		var err error
		wrote, err = editSettingsLocked(path, seed, edit)
		return err
	})
	return wrote, err
}

// editSettingsLocked is editSettings for a caller that holds the file's
// lock.
func editSettingsLocked(path string, seed []byte, edit func([]byte) ([]byte, error)) (bool, error) {
	for range settingsEditTries {
		raw, exists, err := readSettings(path)
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
			// Defense in depth: never write a file Claude Code cannot parse.
			return false, fmt.Errorf("settings %s: edit would produce invalid JSON; refusing", path)
		}
		again, stillExists, err := readSettings(path)
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
		if _, err := fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), out, perm); err != nil {
			return false, fmt.Errorf("settings %s: %w", path, err)
		}
		return true, nil
	}
	return false, fmt.Errorf("settings %s: it changed during each of %d edits; run again", path, settingsEditTries)
}

// readSettings reads the settings file at path; exists is false when there
// is no file.
func readSettings(path string) (raw []byte, exists bool, err error) {
	raw, err = readRegularBounded(path, maxSettingsBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("settings %s: %w", path, err)
	}
	return raw, true, nil
}

// withSettingsLock runs fn while it holds the lock of the settings file at
// path: ~/.amq/locks/settings-<key>.lock, where key names the file with its
// directory's symlinks resolved, so one file reached two ways has one lock.
// A platform with no advisory lock runs fn without one.
func withSettingsLock(path string, fn func() error) error {
	return withSettingsLockWait(path, settingsLockWait, fn)
}

// ErrSettingsBusy is the refusal of a writer that did not get a settings
// file's lock within its wait.
var ErrSettingsBusy = errors.New("another AMQ writer holds the settings file's lock")

const (
	// settingsLockWait bounds how long a command waits for a lock.
	settingsLockWait = 10 * time.Second
	// settingsLockTry bounds how long the machine agent's tick waits: a
	// busy file is checked again on the next tick.
	settingsLockTry = 50 * time.Millisecond
)

// withSettingsLockWait is withSettingsLock that gives up after wait with
// ErrSettingsBusy, without running fn.
func withSettingsLockWait(path string, wait time.Duration, fn func() error) error {
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
	lockPath := filepath.Join(dir, "settings-"+settingsKey(path)+".lock")
	deadline := time.Now().Add(wait)
	err = lock.WithExclusiveFileLockUntil(lockPath, func() bool { return time.Now().After(deadline) }, fn)
	if errors.Is(err, lock.ErrStopped) {
		return fmt.Errorf("settings %s: %w (%s, waited %s); run again once it is done", path, ErrSettingsBusy, lockPath, wait)
	}
	return err
}

// canonicalSettings is path with its directory's symlinks resolved, or
// clean when the directory does not resolve.
func canonicalSettings(path string) string {
	path = filepath.Clean(path)
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(dir, filepath.Base(path))
	}
	return path
}

func settingsKey(path string) string {
	sum := sha256.Sum256([]byte(canonicalSettings(path)))
	return hex.EncodeToString(sum[:12])
}

// The owner's opt-in: install-approval-hook and install-stop-hook record,
// per settings file, that the owner wants that hook there, with the binary
// it was installed with; the uninstall commands clear it. The
// machine agent restores a hook that left a file only while its opt-in
// exists, so a removal the owner made stays removed.

// Hook kinds of an opt-in.
const (
	HookApproval = "approval"
	HookStop     = "stop"
)

// HookOptIn is what the owner installed in one settings file.
type HookOptIn struct {
	Settings string     `json:"settings"`
	Approval *HookShape `json:"approval,omitempty"`
	Stop     *HookShape `json:"stop,omitempty"`
	// ApprovalRemoved and StopRemoved record that the owner removed that
	// hook with its uninstall command: the machine agent neither restores
	// it nor warns about it.
	ApprovalRemoved bool `json:"approval_removed,omitempty"`
	StopRemoved     bool `json:"stop_removed,omitempty"`
}

// Removed reports whether the owner removed the hook of kind from this
// file with its uninstall command, and has not installed it since.
func (in HookOptIn) Removed(kind string) bool {
	if kind == HookApproval {
		return in.ApprovalRemoved
	}
	return in.StopRemoved
}

// HookShape is the binary a hook runs.
type HookShape struct {
	Bin string `json:"bin"`
}

func optInPath(path string) (string, error) {
	home, err := amqhome.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "remote", "hooks", settingsKey(path)+".json"), nil
}

// ReadHookOptIn reads the opt-in of the settings file at path; a file with
// none has a zero value.
func ReadHookOptIn(path string) (HookOptIn, error) {
	file, err := optInPath(path)
	if err != nil {
		return HookOptIn{}, err
	}
	raw, err := fsq.ReadRegularNoFollowBounded(file, 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return HookOptIn{}, nil
	}
	if err != nil {
		return HookOptIn{}, err
	}
	var in HookOptIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return HookOptIn{}, fmt.Errorf("opt-in %s: %w", file, err)
	}
	return in, nil
}

// setHookOptIn records (shape not nil) the opt-in of kind for the settings
// file at path, or (nil) replaces it with the owner's removal. The caller
// holds the file's lock. An unchanged record is not written.
func setHookOptIn(path, kind string, shape *HookShape) error {
	file, err := optInPath(path)
	if err != nil {
		return err
	}
	in, err := ReadHookOptIn(path)
	if err != nil {
		return err
	}
	before := in
	in.Settings = canonicalSettings(path)
	if kind == HookApproval {
		in.Approval, in.ApprovalRemoved = shape, shape == nil
	} else {
		in.Stop, in.StopRemoved = shape, shape == nil
	}
	if sameShape(before.Approval, in.Approval) && sameShape(before.Stop, in.Stop) && before.Settings == in.Settings &&
		before.ApprovalRemoved == in.ApprovalRemoved && before.StopRemoved == in.StopRemoved {
		return nil
	}
	raw, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(filepath.Dir(file), filepath.Base(file), append(raw, '\n'), 0o600)
	return err
}

func sameShape(a, b *HookShape) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// OptInPermissionHook installs the approval hook in the settings file
// settings as InstallPermissionHookAt does, and records the owner's opt-in,
// both under the file's lock, so an uninstall never runs between them.
func OptInPermissionHook(home, settings, bin string) (bool, error) {
	if !filepath.IsAbs(bin) {
		return false, fmt.Errorf("the hook binary %q must be an absolute path", bin)
	}
	return optIn(settings, HookApproval, bin, permissionSpec, permissionHookEntry(home, bin), false)
}

// OptInStopHook is OptInPermissionHook for the Stop hook.
func OptInStopHook(home, settings, bin string) (bool, error) {
	return optIn(settings, HookStop, bin, stopHook, stopHookEntry(home, bin), false)
}

// ErrHookRemoved refuses to install a hook that the owner removed from that
// settings file with its uninstall command.
var ErrHookRemoved = errors.New("the owner removed this hook with its uninstall command")

// optIn makes entry spec's one marked entry in the settings file settings
// and records the opt-in of kind for bin, under one lock. With ensure set it
// refuses with ErrHookRemoved when the owner removed that hook.
func optIn(settings, kind, bin string, spec hookSpec, entry []byte, ensure bool) (bool, error) {
	changed := false
	err := withSettingsLock(settings, func() error {
		if ensure {
			in, err := ReadHookOptIn(settings)
			if err != nil {
				return err
			}
			if in.Removed(kind) {
				return ErrHookRemoved
			}
		}
		same, err := installedAs(settings, spec, entry)
		if err != nil {
			return err
		}
		if !same {
			if changed, err = editSettingsLocked(settings, spec.seed(), replaceEdit(spec, entry)); err != nil {
				return err
			}
		}
		return setHookOptIn(settings, kind, &HookShape{Bin: bin})
	})
	return changed, err
}

// OptOutHook removes the hook of kind from the settings file settings and
// records the owner's removal in place of its opt-in, under one lock.
func OptOutHook(settings, kind string) error {
	spec := permissionSpec
	if kind == HookStop {
		spec = stopHook
	}
	return withSettingsLock(settings, func() error {
		if _, err := editSettingsLocked(settings, nil, removeEdit(spec)); err != nil {
			return err
		}
		return setHookOptIn(settings, kind, nil)
	})
}

// RestoreHook puts the hook of kind back into the settings file settings
// when it holds none and the owner's opt-in for that file exists. It never
// creates the file: one the owner deleted stays deleted. The hook runs bin,
// the amq-remote installed now, which must exist: a recorded path that an
// upgrade removed is never written back. It reads the opt-in under the
// file's lock, so an uninstall that ran first wins, and waits for that lock
// at most settingsLockTry (ErrSettingsBusy). It reports whether it wrote the
// hook; false with no error means no opt-in, no file or a hook already
// there.
func RestoreHook(home, settings, kind, bin string) (bool, error) {
	if !filepath.IsAbs(bin) || !installedFile(bin) {
		return false, fmt.Errorf("the hook binary %q is not an installed file", bin)
	}
	restored := false
	err := withSettingsLockWait(settings, settingsLockTry, func() error {
		in, err := ReadHookOptIn(settings)
		if err != nil {
			return err
		}
		var spec hookSpec
		var entry []byte
		switch {
		case kind == HookApproval && in.Approval != nil:
			spec, entry = permissionSpec, permissionHookEntry(home, bin)
		case kind == HookStop && in.Stop != nil:
			spec, entry = stopHook, stopHookEntry(home, bin)
		default:
			return nil
		}
		restored, err = editSettingsLocked(settings, nil, func(raw []byte) ([]byte, error) {
			if groups, err := markedGroupsIn(raw, spec); err != nil || len(groups) > 0 {
				return nil, err
			}
			return rawInsertEntry(raw, spec, entry)
		})
		return err
	})
	return restored, err
}

// EnsurePermissionHook is OptInPermissionHook that keeps the binary of the
// installed approval hook (KeptHookBin) and writes the entry in its current
// form, so an older form (another timeout or gate) is upgraded. It refuses
// with ErrHookRemoved a hook the owner removed. It reports whether it
// changed the settings file.
func EnsurePermissionHook(home, settings, bin string) (bool, error) {
	cmds, _ := installedCommands(settings, permissionSpec)
	bin = KeptHookBin(cmds, permissionHookBinRe, bin)
	if !filepath.IsAbs(bin) {
		return false, fmt.Errorf("the hook binary %q must be an absolute path", bin)
	}
	return optIn(settings, HookApproval, bin, permissionSpec, permissionHookEntry(home, bin), true)
}

// EnsureStopHook is EnsurePermissionHook for the Stop hook.
func EnsureStopHook(home, settings, bin string) (bool, error) {
	cmds, _ := installedCommands(settings, stopHook)
	bin = KeptHookBin(cmds, stopHookBinRe, bin)
	return optIn(settings, HookStop, bin, stopHook, stopHookEntry(home, bin), true)
}

// KeptHookBin is the binary that the installed hook commands all run, as
// the first group of binRe names it, when they name one binary and it is an
// installed file; else bin. A repair keeps the binary an install chose, so
// two installers never rewrite a file back and forth.
func KeptHookBin(commands []string, binRe *regexp.Regexp, bin string) string {
	found := ""
	for _, command := range commands {
		m := binRe.FindStringSubmatch(command)
		if m == nil {
			return bin
		}
		got := shellUnquote(m[1])
		if found != "" && got != found {
			return bin
		}
		found = got
	}
	if found == "" || !installedFile(found) {
		return bin
	}
	return found
}

// installedFile reports whether path is an existing regular file.
func installedFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
