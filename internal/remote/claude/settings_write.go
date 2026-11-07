package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/hookfile"
)

// ErrSettingsBusy is the refusal of a hook writer that did not get the
// settings file's lock within its wait.
var ErrSettingsBusy = hookfile.ErrBusy

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
	return filepath.Join(home, "remote", "hooks", hookfile.Key(path)+".json"), nil
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
		return HookOptIn{}, fmt.Errorf("opt-in %s: %w", file, err)
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
	in.Settings = hookfile.Canonical(path)
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
	// Claude Code owns its settings file and may keep it behind a link.
	_, err = fsq.WriteFileAtomicFollowLinks(filepath.Dir(file), filepath.Base(file), append(raw, '\n'), 0o600)
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
	return optIn(settings, HookApproval, bin, permissionSpec, permissionHookEntry(home, bin), optInInstall, hookfile.LockWait)
}

// OptInStopHook is OptInPermissionHook for the Stop hook.
func OptInStopHook(home, settings, bin string) (bool, error) {
	return optIn(settings, HookStop, bin, stopHook, stopHookEntry(home, bin), optInInstall, hookfile.LockWait)
}

// optInMode is what optIn requires of the owner's opt-in before it writes.
type optInMode int

const (
	optInInstall optInMode = iota // the owner installs: nothing
	optInEnsure                   // a repair: the owner did not remove the hook
	optInUpgrade                  // the machine agent: an opt-in of that kind exists
)

// ErrNoOptIn refuses the machine agent's upgrade of a hook the owner has
// no opt-in for in that settings file.
var ErrNoOptIn = errors.New("the owner has no opt-in for this hook in this settings file")

// has reports whether the opt-in of kind exists.
func (in HookOptIn) has(kind string) bool {
	if kind == HookApproval {
		return in.Approval != nil
	}
	return in.Stop != nil
}

// ErrHookRemoved refuses to install a hook that the owner removed from that
// settings file with its uninstall command.
var ErrHookRemoved = errors.New("the owner removed this hook with its uninstall command")

// optIn makes entry spec's one marked entry in the settings file settings
// and records the opt-in of kind for bin, under one lock. With ensure set it
// refuses with ErrHookRemoved when the owner removed that hook. It waits
// for the lock at most wait (ErrSettingsBusy).
func optIn(settings, kind, bin string, spec hookfile.Spec, entry []byte, mode optInMode, wait time.Duration) (bool, error) {
	changed := false
	err := hookfile.WithLockWait(settings, wait, func() error {
		if mode != optInInstall {
			in, err := ReadHookOptIn(settings)
			if err != nil {
				return err
			}
			if in.Removed(kind) {
				return ErrHookRemoved
			}
			if mode == optInUpgrade && !in.has(kind) {
				return ErrNoOptIn
			}
		}
		same, err := hookfile.InstalledAs(settings, spec, entry)
		if err != nil {
			return err
		}
		if !same {
			// An upgrade writes only into a file that exists, as a restore
			// does: a file the owner deleted stays deleted.
			seed := spec.Seed()
			if mode == optInUpgrade {
				seed = nil
			}
			if changed, err = hookfile.EditLocked(settings, seed, hookfile.ReplaceEdit(spec, entry)); err != nil {
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
	return hookfile.WithLock(settings, func() error {
		if _, err := hookfile.EditLocked(settings, nil, hookfile.RemoveEdit(spec)); err != nil {
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
// at most hookfile.LockTry (ErrSettingsBusy). It reports whether it wrote the
// hook; false with no error means no opt-in, no file or a hook already
// there.
func RestoreHook(home, settings, kind, bin string) (bool, error) {
	if !filepath.IsAbs(bin) || !installedFile(bin) {
		return false, fmt.Errorf("the hook binary %q is not an installed file", bin)
	}
	restored := false
	err := hookfile.WithLockWait(settings, hookfile.LockTry, func() error {
		in, err := ReadHookOptIn(settings)
		if err != nil {
			return err
		}
		var spec hookfile.Spec
		var entry []byte
		switch {
		case kind == HookApproval && in.Approval != nil:
			spec, entry = permissionSpec, permissionHookEntry(home, bin)
		case kind == HookStop && in.Stop != nil:
			spec, entry = stopHook, stopHookEntry(home, bin)
		default:
			return nil
		}
		restored, err = hookfile.EditLocked(settings, nil, func(raw []byte) ([]byte, error) {
			if groups, err := hookfile.MarkedGroupsIn(raw, spec); err != nil || len(groups) > 0 {
				return nil, err
			}
			return hookfile.InsertEntry(raw, spec, entry)
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
	return ensureHook(home, settings, HookApproval, bin, optInEnsure, hookfile.LockWait)
}

// EnsureStopHook is EnsurePermissionHook for the Stop hook.
func EnsureStopHook(home, settings, bin string) (bool, error) {
	return ensureHook(home, settings, HookStop, bin, optInEnsure, hookfile.LockWait)
}

// UpgradeHook is EnsurePermissionHook (kind HookApproval) or EnsureStopHook
// for the machine agent's tick: it acts only while the owner's opt-in of
// that kind exists (ErrNoOptIn), never creates one, and waits for the
// file's lock at most hookfile.LockTry (ErrSettingsBusy).
func UpgradeHook(home, settings, kind, bin string) (bool, error) {
	return ensureHook(home, settings, kind, bin, optInUpgrade, hookfile.LockTry)
}

func ensureHook(home, settings, kind, bin string, mode optInMode, wait time.Duration) (bool, error) {
	spec, binRe := permissionSpec, permissionHookBinRe
	if kind == HookStop {
		spec, binRe = stopHook, stopHookBinRe
	}
	cmds, _ := hookfile.InstalledCommands(settings, spec)
	bin = KeptHookBin(cmds, binRe, bin)
	if !filepath.IsAbs(bin) {
		return false, fmt.Errorf("the hook binary %q must be an absolute path", bin)
	}
	entry := permissionHookEntry(home, bin)
	if kind == HookStop {
		entry = stopHookEntry(home, bin)
	}
	return optIn(settings, kind, bin, spec, entry, mode, wait)
}

// KeptHookBin is the binary that the installed hook commands all run, as
// the first group of binRe names it, when they name one installed file;
// else bin. A repair keeps the binary an install chose, so two installers
// never rewrite a file back and forth, except the versioned target of bin
// (bin is a stable symlink to it), which an upgrade removes: then bin.
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
	if target, err := filepath.EvalSymlinks(bin); err == nil && target == found && found != bin {
		return bin
	}
	return found
}

// installedFile reports whether path is an existing regular file.
func installedFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
