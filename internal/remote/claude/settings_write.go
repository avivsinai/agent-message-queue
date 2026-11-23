package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/hookfile"
)

// ErrSettingsBusy is the refusal of a hook writer that did not get the
// settings file's lock within its wait.
var ErrSettingsBusy = hookfile.ErrBusy

// The owner's opt-in: install-approval-hook, install-stop-hook and
// install-prompt-hook record, per settings file, that the owner wants that
// hook there, with the binary it was installed with; the uninstall commands
// clear it. The machine agent restores a hook that left a file only while
// its opt-in exists, so a removal the owner made stays removed.

// Hook kinds of an opt-in.
const (
	HookApproval = "approval"
	HookStop     = "stop"
	HookPrompt   = "prompt"
)

// HookOptIn is what the owner installed in one settings file.
type HookOptIn struct {
	Settings string     `json:"settings"`
	Approval *HookShape `json:"approval,omitempty"`
	Stop     *HookShape `json:"stop,omitempty"`
	Prompt   *HookShape `json:"prompt,omitempty"`
	// ApprovalRemoved, StopRemoved and PromptRemoved record that the owner
	// removed that hook with its uninstall command: the machine agent
	// neither restores it nor warns about it.
	ApprovalRemoved bool `json:"approval_removed,omitempty"`
	StopRemoved     bool `json:"stop_removed,omitempty"`
	PromptRemoved   bool `json:"prompt_removed,omitempty"`
}

// hookKind is one hook the owner installs: the chain and marker that own
// its entry, the binary its command names, the entry it writes now, and
// its two fields of the opt-in record.
type hookKind struct {
	spec    hookfile.Spec
	binRe   *regexp.Regexp
	entry   func(home, bin string) []byte
	shape   func(*HookOptIn) **HookShape
	removed func(*HookOptIn) *bool
	// ownerOnly makes every repair require this kind's opt-in in the file:
	// only the owner's install puts the hook into a settings file, never
	// another hook's opt-in or a file that lacks it.
	ownerOnly bool
}

// hookKinds holds one row per hook kind.
var hookKinds = map[string]hookKind{
	HookApproval: {spec: permissionSpec, binRe: permissionHookBinRe, entry: permissionHookEntry,
		shape: func(in *HookOptIn) **HookShape { return &in.Approval }, removed: func(in *HookOptIn) *bool { return &in.ApprovalRemoved }},
	HookStop: {spec: stopHook, binRe: stopHookBinRe, entry: stopHookEntry,
		shape: func(in *HookOptIn) **HookShape { return &in.Stop }, removed: func(in *HookOptIn) *bool { return &in.StopRemoved }},
	HookPrompt: {spec: promptHook, binRe: promptHookBinRe, entry: promptHookEntry,
		shape: func(in *HookOptIn) **HookShape { return &in.Prompt }, removed: func(in *HookOptIn) *bool { return &in.PromptRemoved },
		ownerOnly: true},
}

// kindOf is the row of kind; an unknown kind is an error.
func kindOf(kind string) (hookKind, error) {
	row, ok := hookKinds[kind]
	if !ok {
		return hookKind{}, fmt.Errorf("unknown hook kind %q", kind)
	}
	return row, nil
}

// OwnerOnly reports whether only the owner's install puts the hook of kind
// into a settings file, so a repair or a warning about it applies only
// where the owner's opt-in of it exists.
func OwnerOnly(kind string) bool {
	return hookKinds[kind].ownerOnly
}

// Removed reports whether the owner removed the hook of kind from this
// file with its uninstall command, and has not installed it since.
func (in HookOptIn) Removed(kind string) bool {
	row, ok := hookKinds[kind]
	return ok && *row.removed(&in)
}

// Has reports whether the owner's opt-in of kind exists for this file.
func (in HookOptIn) Has(kind string) bool {
	row, ok := hookKinds[kind]
	return ok && *row.shape(&in) != nil
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
	row, err := kindOf(kind)
	if err != nil {
		return err
	}
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
	*row.shape(&in), *row.removed(&in) = shape, shape == nil
	if reflect.DeepEqual(before, in) {
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

// OptInPermissionHook installs the approval hook in the settings file
// settings as InstallPermissionHookAt does, and records the owner's opt-in,
// both under the file's lock, so an uninstall never runs between them.
func OptInPermissionHook(home, settings, bin string) (bool, error) {
	if !filepath.IsAbs(bin) {
		return false, fmt.Errorf("the hook binary %q must be an absolute path", bin)
	}
	return optIn(settings, HookApproval, bin, permissionHookEntry(home, bin), optInInstall, hookfile.LockWait)
}

// OptInPromptHook is OptInPermissionHook for the prompt hook: the
// UserPromptSubmit receiver of the user settings, which reports a prompt's
// permission mode to the endpoints that serve its session. Its gate is the
// Stop hook's, so a session that no attachment serves starts nothing. Only
// this install records its opt-in: no other hook's opt-in installs it.
func OptInPromptHook(home, settings, bin string) (bool, error) {
	if !filepath.IsAbs(bin) {
		return false, fmt.Errorf("the hook binary %q must be an absolute path", bin)
	}
	return optIn(settings, HookPrompt, bin, promptHookEntry(home, bin), optInInstall, hookfile.LockWait)
}

// OptInStopHook is OptInPermissionHook for the Stop hook: the fail-open
// receiver at the end of the Stop chain of the user settings.
//
// Claude Code's durable hook scopes are the user, project, local, and
// managed settings files. None of them is a single session: session hooks
// exist only in memory inside that process
// (https://code.claude.com/docs/en/hooks). The bound session can live in
// any project, so the hook is user-level. The receiver returns before any
// write unless that session is attached or the binding names it. The
// command starts amq-remote only when a state the receiver reads exists:
// AMQ_REMOTE_BINDING, a binding file, a binding in the bindings directory
// (its tombstones are none), or the attachment sentinels. Everywhere else
// the hook costs one sh.
func OptInStopHook(home, settings, bin string) (bool, error) {
	return optIn(settings, HookStop, bin, stopHookEntry(home, bin), optInInstall, hookfile.LockWait)
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

// ErrHookRemoved refuses to install a hook that the owner removed from that
// settings file with its uninstall command.
var ErrHookRemoved = errors.New("the owner removed this hook with its uninstall command")

// optIn makes entry the one marked entry of kind's chain in the settings
// file settings and records the opt-in of kind for bin, under one lock. A
// repair (any mode but optInInstall) refuses with ErrHookRemoved when the
// owner removed that hook, and with ErrNoOptIn when no opt-in of kind exists
// while the mode or the kind requires one. It waits for the lock at most
// wait (ErrSettingsBusy).
func optIn(settings, kind, bin string, entry []byte, mode optInMode, wait time.Duration) (bool, error) {
	row, err := kindOf(kind)
	if err != nil {
		return false, err
	}
	changed := false
	err = hookfile.WithLockWait(settings, wait, func() error {
		if mode != optInInstall {
			in, err := ReadHookOptIn(settings)
			if err != nil {
				return err
			}
			if in.Removed(kind) {
				return ErrHookRemoved
			}
			if (mode == optInUpgrade || row.ownerOnly) && !in.Has(kind) {
				return ErrNoOptIn
			}
		}
		same, err := hookfile.InstalledAs(settings, row.spec, entry)
		if err != nil {
			return err
		}
		if !same {
			// An upgrade writes only into a file that exists, as a restore
			// does: a file the owner deleted stays deleted.
			seed := row.spec.Seed()
			if mode == optInUpgrade {
				seed = nil
			}
			if changed, err = hookfile.EditLocked(settings, seed, hookfile.ReplaceEdit(row.spec, entry)); err != nil {
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
	row, err := kindOf(kind)
	if err != nil {
		return err
	}
	return hookfile.WithLock(settings, func() error {
		if _, err := hookfile.EditLocked(settings, nil, hookfile.RemoveEdit(row.spec)); err != nil {
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
		row, ok := hookKinds[kind]
		if !ok || !in.Has(kind) {
			return nil
		}
		spec, entry := row.spec, row.entry(home, bin)
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

// EnsurePromptHook is EnsurePermissionHook for the prompt hook, which it
// writes only where the owner's opt-in of the prompt hook exists in that
// settings file (ErrNoOptIn).
func EnsurePromptHook(home, settings, bin string) (bool, error) {
	return ensureHook(home, settings, HookPrompt, bin, optInEnsure, hookfile.LockWait)
}

// UpgradeHook is the Ensure function of kind for the machine agent's tick:
// it acts only while the owner's opt-in of that kind exists (ErrNoOptIn),
// never creates one, and waits for the file's lock at most
// hookfile.LockTry (ErrSettingsBusy).
func UpgradeHook(home, settings, kind, bin string) (bool, error) {
	return ensureHook(home, settings, kind, bin, optInUpgrade, hookfile.LockTry)
}

func ensureHook(home, settings, kind, bin string, mode optInMode, wait time.Duration) (bool, error) {
	row, err := kindOf(kind)
	if err != nil {
		return false, err
	}
	cmds, _ := hookfile.InstalledCommands(settings, row.spec)
	bin = KeptHookBin(cmds, row.binRe, bin)
	if !filepath.IsAbs(bin) {
		return false, fmt.Errorf("the hook binary %q must be an absolute path", bin)
	}
	return optIn(settings, kind, bin, row.entry(home, bin), mode, wait)
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
