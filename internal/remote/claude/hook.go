package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// Stop-hook bridge: the hook is how the adapter learns a submitted turn ENDED
// — the transcript user line proves the payload was
// accepted and the assistant line proves the turn started, but only the
// Stop event carries the completion signal. The installer writes the hook
// into the TARGET's ~/.claude/settings.json; the receiver subcommand is
// fail-open by contract (exit 0 on ANY error) so a broken bridge can never
// block Claude Code — Claude Code's blocking-error code is exit 2.

// stopHookMarker is the settings.json ownership signal: every hook command
// the installer writes STARTS with this env assignment, and uninstall
// removes exactly the entries whose nested commands all carry it. It is an
// assignment, not the command word: the shell runs the quoted binary that
// follows it (an `AMQ_STOP_HOOK_BIN=<bin> claude stop-hook` form would
// execute the program `claude`).
const stopHookMarker = "AMQ_STOP_HOOK=1 "

// stopHookType is the hook event name in settings.json.
const stopHookType = "Stop"

// maxSettingsBytes bounds the settings.json read (a real file is a few KiB).
const maxSettingsBytes = 4 << 20

// settings.json is edited by BYTE SURGERY, never by decode/re-marshal:
// unrelated content (key order, whitespace, and integer literals beyond
// float64 precision — the 9007199254740993 (2^53) fixture class) must
// survive install AND uninstall unchanged, which a map round-trip cannot
// promise. The decoder is used only to LOCATE byte ranges; the ranges are
// cut from the raw file.
//
// Install appends our group AFTER the operator's groups in the event
// array, with a tight leading comma when the array is non-empty, so the
// position of every operator group stays the same (Codex keys a hook's
// trust by that position). A missing event array or hooks object is
// inserted first in its parent object. Uninstall removes exactly our group
// plus its comma. Uninstall never removes a
// container: formatting cannot prove who created an empty "Stop" array or
// "hooks" object, so an operator's containers are preserved and ours may be
// left behind empty, which Claude Code treats as no hooks. A file that
// already had a Stop array round-trips byte for byte.

// InstallStopHook adds the fail-open receiver to the Stop hook chain in
// the user settings.json in the Claude config directory. Idempotent: the same entry is left untouched,
// and a marked entry with another command (an older binary) is replaced.
// The settings leaf must be a
// regular file (lstat rule — a directory or symlink is refused); the write
// is fsq atomic-replace.
//
// Claude Code's durable hook scopes are the user, project, local, and
// managed settings files. None of them is a single session: session hooks
// exist only in memory inside that process
// (https://code.claude.com/docs/en/hooks). The bound session can live in
// any project, so the installer keeps this user-level hook. The receiver
// returns before any write unless that session is attached or the binding
// names it. That limits who writes, not how many bytes a session's marker
// may hold: the file is one appended line per turn, and it is removed when
// the attachment unsubscribes. User off removes only the Buzz binding.
//
// The command starts amq-remote only when a state the receiver reads
// exists: AMQ_REMOTE_BINDING, a binding file, a binding in the bindings
// directory (its tombstones are none), or the attachment sentinels.
// Everywhere else the hook costs one sh.
func InstallStopHook(home, bin string) error {
	_, err := InstallStopHookAt(home, SettingsPath(home), bin)
	return err
}

// InstallStopHookAt is InstallStopHook for the settings file settings, and
// reports whether it changed the file.
func InstallStopHookAt(home, settings, bin string) (bool, error) {
	return replaceHook(settings, stopHook, stopHookEntry(home, bin))
}

// UninstallStopHook removes marked entries from the Stop chain. When no
// marked entry exists the file is untouched (no rewrite, no mtime churn).
func UninstallStopHook(home string) error {
	return removeHook(SettingsPath(home), stopHook)
}

// hookSpec names one hook event chain in settings.json and the marker that
// owns AMQ's entry in it. The Stop and PermissionRequest entries share one
// installer and differ only here.
type hookSpec struct {
	event, marker string
	// flat: each element of the event array is one hook script, as in
	// Cursor's hooks.json, not a matcher group that holds a "hooks" array.
	flat bool
	// match, when set, decides which commands are this spec's own instead
	// of the marker prefix.
	match func(command string) bool
}

// ownsCommand reports whether a hook command is this spec's own.
func (s hookSpec) ownsCommand(command string) bool {
	if s.match != nil {
		return s.match(command)
	}
	return strings.HasPrefix(command, s.marker)
}

// ownsHook reports whether one inner hook object is this spec's own.
func (s hookSpec) ownsHook(el []byte) bool {
	var h struct {
		Command string `json:"command"`
	}
	return json.Unmarshal(el, &h) == nil && s.ownsCommand(h.Command)
}

// owns reports whether one element of spec's event array is, or holds, one
// of this spec's hooks.
func (s hookSpec) owns(el []byte) bool {
	if s.flat {
		return s.ownsHook(el)
	}
	var g struct {
		Hooks []json.RawMessage `json:"hooks"`
	}
	if json.Unmarshal(el, &g) != nil {
		return false
	}
	for _, h := range g.Hooks {
		if s.ownsHook(h) {
			return true
		}
	}
	return false
}

// duplicate reports whether group i of the chain groups is a byte-identical
// copy of an earlier group and holds a command of the installer's family
// (the marker prefix), when match narrows ownership below that family. A
// second copy is never the owner's choice, so an install removes it.
func (s hookSpec) duplicate(raw []byte, groups [][2]int, i int) bool {
	if s.flat || s.match == nil || s.marker == "" {
		return false
	}
	el := raw[groups[i][0]:groups[i][1]]
	if !(hookSpec{event: s.event, marker: s.marker}).owns(el) {
		return false
	}
	for _, g := range groups[:i] {
		if bytes.Equal(raw[g[0]:g[1]], el) {
			return true
		}
	}
	return false
}

// seed is the content a missing file starts as. Cursor ignores a hooks
// file without a positive integer "version".
func (s hookSpec) seed() []byte {
	if s.flat {
		return []byte("{\"version\":1}\n")
	}
	return []byte("{}\n")
}

var (
	stopHook       = hookSpec{event: stopHookType, marker: stopHookMarker}
	permissionSpec = hookSpec{event: permissionHookType, marker: permissionHookMarker}
)

// InstallPermissionHook adds the PermissionRequest hook that carries tool
// dialogs to Buzz: one entry with no matcher (every tool), marked
// AMQ_APPROVAL_HOOK=1, with the timeout permissionHookTimeout. bin must be
// absolute: an app-launched session's PATH may not hold it. A marked entry
// with another command is replaced; the same one is kept.
func InstallPermissionHook(home, bin string) error {
	_, err := InstallPermissionHookAt(home, SettingsPath(home), bin)
	return err
}

// InstallPermissionHookAt is InstallPermissionHook for the settings file
// settings, and reports whether it changed the file.
func InstallPermissionHookAt(home, settings, bin string) (bool, error) {
	if !filepath.IsAbs(bin) {
		return false, fmt.Errorf("the hook binary %q must be an absolute path", bin)
	}
	return replaceHook(settings, permissionSpec, permissionHookEntry(home, bin))
}

// replaceHook makes entry the only marked entry of spec's chain in the
// settings file at path, and reports whether it wrote the file. A single
// marked group equal to entry, field for field, is kept untouched without
// a lock or a write; anything else marked (an older binary path, other
// flags, another timeout or matcher) is replaced in one write.
func replaceHook(path string, spec hookSpec, entry []byte) (bool, error) {
	if same, err := installedAs(path, spec, entry); same || err != nil {
		return false, err
	}
	return editSettings(path, spec.seed(), replaceEdit(spec, entry))
}

// installedAs reports whether spec's one marked group in the settings file
// at path is entry, field for field.
func installedAs(path string, spec hookSpec, entry []byte) (bool, error) {
	groups, err := markedGroups(path, spec)
	return err == nil && len(groups) == 1 && sameJSON(groups[0], entry), err
}

// replaceEdit is the edit that removes spec's entries and appends entry. It
// writes nothing when the bytes already hold exactly entry.
func replaceEdit(spec hookSpec, entry []byte) func([]byte) ([]byte, error) {
	return func(raw []byte) ([]byte, error) {
		original := raw
		if cut, err := rawRemoveEntries(raw, spec); err != nil {
			return nil, err
		} else if cut != nil {
			raw = cut
		}
		out, err := rawInsertEntry(raw, spec, entry)
		if err != nil || out == nil || bytes.Equal(out, original) {
			return nil, err
		}
		return out, nil
	}
}

// sameJSON reports whether two JSON values hold the same members and
// values, whatever their whitespace and key order.
func sameJSON(a, b []byte) bool {
	decode := func(raw []byte) (any, bool) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		return v, dec.Decode(&v) == nil
	}
	va, oka := decode(a)
	vb, okb := decode(b)
	return oka && okb && reflect.DeepEqual(va, vb)
}

// markedGroups returns the raw matcher groups of spec's chain in the
// settings file at path that hold a marked hook. A missing file has none.
func markedGroups(path string, spec hookSpec) ([][]byte, error) {
	raw, exists, err := readSettings(path)
	if err != nil || !exists {
		return nil, err
	}
	groups, err := markedGroupsIn(raw, spec)
	if err != nil {
		return nil, fmt.Errorf("settings %s: %w", path, err)
	}
	return groups, nil
}

// CheckSettings refuses, naming path, a hooks file the writer refuses to
// edit for event: one it cannot read as a regular file within its size
// bound, one that is not a single JSON value, or one whose root, hooks or
// event value has the wrong type or a duplicate key. A missing file
// passes: the writer starts it.
func CheckSettings(path, event string) error {
	raw, exists, err := readSettings(path)
	if err != nil || !exists {
		return err
	}
	if !json.Valid(raw) {
		return fmt.Errorf("settings %s: not one JSON value (a syntax error or trailing JSON)", path)
	}
	if _, err := markedGroupsIn(raw, hookSpec{event: event}); err != nil {
		return fmt.Errorf("settings %s: %w", path, err)
	}
	return nil
}

// markedGroupsIn is markedGroups for the bytes of a settings file.
func markedGroupsIn(raw []byte, spec hookSpec) ([][]byte, error) {
	loc, err := locateSettings(raw, spec.event)
	if err != nil || loc.stop == nil {
		return nil, err
	}
	groups, err := arrayElements(raw, *loc.stop)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for i, g := range groups {
		if err := checkGroup(raw[g[0]:g[1]], spec); err != nil {
			return nil, err
		}
		if spec.owns(raw[g[0]:g[1]]) || spec.duplicate(raw, groups, i) {
			out = append(out, raw[g[0]:g[1]])
		}
	}
	return out, nil
}

// checkGroup refuses a matcher group whose "hooks" member is not an array:
// the writer cannot tell whether it holds an owned hook.
func checkGroup(el []byte, spec hookSpec) error {
	if spec.flat {
		return nil
	}
	var g map[string]json.RawMessage
	if json.Unmarshal(el, &g) != nil {
		return nil
	}
	if h, ok := g["hooks"]; ok {
		if v := bytes.TrimSpace(h); len(v) == 0 || v[0] != '[' && !bytes.Equal(v, []byte("null")) {
			return fmt.Errorf("%s: %q must be a JSON array", spec.event, "hooks")
		}
	}
	return nil
}

// installedCommands lists the commands of spec's marked hooks in the
// settings file at path.
func installedCommands(path string, spec hookSpec) ([]string, error) {
	groups, err := markedGroups(path, spec)
	if err != nil {
		return nil, err
	}
	type hook struct {
		Command string `json:"command"`
	}
	var out []string
	for _, g := range groups {
		var group struct {
			Hooks []hook `json:"hooks"`
		}
		if spec.flat {
			group.Hooks = make([]hook, 1)
			if json.Unmarshal(g, &group.Hooks[0]) != nil {
				continue
			}
		} else if json.Unmarshal(g, &group) != nil {
			continue
		}
		for _, h := range group.Hooks {
			if spec.ownsCommand(h.Command) {
				out = append(out, h.Command)
			}
		}
	}
	return out, nil
}

// CommandHook is one marked command hook in an event chain of the user
// settings file: the command starts with marker, and an empty matcher
// matches every tool or source.
type CommandHook struct {
	Event, Matcher, Marker, Command string
	Timeout                         int
	// Async runs the hook without holding the event (Claude Code's
	// "async"); false writes no field.
	Async bool
	// Owns, when set, decides which installed commands of the event chain
	// this hook replaces, in place of the Marker prefix.
	Owns func(command string) bool
}

// InstallCommandHooks makes each hook the only entry marked with its marker
// in its event chain of the user settings file, with replaceHook's rules:
// the same entry is kept and any other marked entry is replaced.
func InstallCommandHooks(home string, hooks []CommandHook) error {
	_, err := InstallCommandHooksAt(SettingsPath(home), hooks)
	return err
}

// InstallCommandHooksAt is InstallCommandHooks for the hooks file at path,
// any file with the {"hooks":{event:[group...]}} shape. It reports whether
// it changed the file.
func InstallCommandHooksAt(path string, hooks []CommandHook) (bool, error) {
	changed := false
	for _, h := range hooks {
		wrote, err := replaceHook(path, hookSpec{event: h.Event, marker: h.Marker, match: h.Owns}, commandHookEntry(h))
		if err != nil {
			return changed, err
		}
		changed = changed || wrote
	}
	return changed, nil
}

// InstallCursorCommandHooksAt is InstallCommandHooksAt for a Cursor hooks
// file, {"version":1,"hooks":{event:[{command,matcher,timeout}...]}}: each
// array element is one hook script, and a missing file starts with the
// version Cursor requires.
func InstallCursorCommandHooksAt(path string, hooks []CommandHook) error {
	_, err := editSettings(path, (hookSpec{flat: true}).seed(), func(raw []byte) ([]byte, error) {
		original := raw
		for _, h := range hooks {
			spec := hookSpec{event: h.Event, marker: h.Marker, flat: true}
			cut, err := rawRemoveEntries(raw, spec)
			if err != nil {
				return nil, err
			}
			if cut != nil {
				raw = cut
			}
			raw, err = rawInsertEntry(raw, spec, cursorHookEntry(h.Command, h.Matcher, h.Timeout))
			if err != nil {
				return nil, err
			}
		}
		if err := ValidateCursorHooks(raw); err != nil {
			return nil, err
		}
		if bytes.Equal(original, raw) {
			return nil, nil
		}
		return raw, nil
	})
	return err
}

// ValidateCursorHooks checks the bytes with Cursor's native comment
// stripping rules. Unrelated operator content is never rewritten to fix it.
func ValidateCursorHooks(raw []byte) error {
	var file struct {
		Version float64 `json:"version"`
	}
	if json.Unmarshal(raw, &file) != nil || file.Version < 1 || math.Trunc(file.Version) != file.Version {
		return fmt.Errorf("hooks file needs a numeric positive integer \"version\"")
	}
	stripped := cursorLineComment.ReplaceAll(raw, nil)
	stripped = cursorBlockComment.ReplaceAll(stripped, nil)
	if !json.Valid(stripped) || !bytes.Equal(raw, stripped) {
		return fmt.Errorf("comment markers inside strings change the hooks file; escape / as \\/ and * as \\u002a")
	}
	return nil
}

var (
	cursorLineComment  = regexp.MustCompile(`//[^\r\n\x{2028}\x{2029}]*`)
	cursorBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// UninstallCommandHooks removes the entries marked with marker from each
// event chain of the user settings file.
func UninstallCommandHooks(home, marker string, events []string) error {
	return UninstallCommandHooksAt(SettingsPath(home), marker, events)
}

// UninstallCommandHooksAt is UninstallCommandHooks for the hooks file at
// path.
func UninstallCommandHooksAt(path, marker string, events []string) error {
	return uninstallCommandHooksAt(path, marker, false, events)
}

// UninstallCursorCommandHooksAt is UninstallCommandHooksAt for a Cursor
// hooks file.
func UninstallCursorCommandHooksAt(path, marker string, events []string) error {
	return uninstallCommandHooksAt(path, marker, true, events)
}

func uninstallCommandHooksAt(path, marker string, flat bool, events []string) error {
	for _, event := range events {
		if _, err := editSettings(path, nil, removeEdit(hookSpec{event: event, marker: marker, flat: flat})); err != nil {
			return err
		}
	}
	return nil
}

// MarkedHookCommands lists the commands marked with marker in the event
// chain of the user settings file. A missing file lists none.
func MarkedHookCommands(home, event, marker string) ([]string, error) {
	return MarkedHookCommandsAt(SettingsPath(home), event, marker)
}

// MarkedHookCommandsAt is MarkedHookCommands for the hooks file at path.
func MarkedHookCommandsAt(path, event, marker string) ([]string, error) {
	return installedCommands(path, hookSpec{event: event, marker: marker})
}

// MarkedCursorHookCommandsAt is MarkedHookCommandsAt for a Cursor hooks
// file.
func MarkedCursorHookCommandsAt(path, event, marker string) ([]string, error) {
	return installedCommands(path, hookSpec{event: event, marker: marker, flat: true})
}

// UninstallPermissionHook removes only the marked PermissionRequest entry.
func UninstallPermissionHook(home string) error {
	return removeHook(SettingsPath(home), permissionSpec)
}

// PermissionHookState is StopHookState for the PermissionRequest entry in
// the settings file settings.
func PermissionHookState(settings string) (string, error) {
	return hookState(settings, permissionSpec)
}

// PermissionHookCommand is the command of the one marked PermissionRequest
// hook in the settings file settings, "" when it does not hold exactly one.
func PermissionHookCommand(settings string) (string, error) {
	cmds, err := installedCommands(settings, permissionSpec)
	if err != nil || len(cmds) != 1 {
		return "", err
	}
	return cmds[0], nil
}

// Forms of an installed hook command (StopHookForm, PermissionHookForm).
const (
	// HookFormCurrent is what the installer writes now for that binary.
	HookFormCurrent = "current"
	// HookFormUngated starts amq-remote on every event in every session.
	HookFormUngated = "ungated"
	// HookFormStale is gated but differs from what the installer writes
	// now: its gate names paths this home no longer resolves, or its
	// timeout or form is older. The gate may skip a state the receiver
	// reads.
	HookFormStale = "stale"
)

var (
	stopHookBinRe       = regexp.MustCompile(`'((?:[^']|'\\'')*)' claude stop-hook$`)
	permissionHookBinRe = regexp.MustCompile(`'((?:[^']|'\\'')*)' claude permission-hook(?: --wait [0-9]+)?$`)
)

// StopHookForm reports the form of the one installed Stop hook in the
// settings file settings, "" when it does not hold exactly one. home roots
// the markers the current form's gate names.
func StopHookForm(home, settings string) (string, error) {
	return hookForm(settings, stopHook, func(cmd string) []byte {
		m := stopHookBinRe.FindStringSubmatch(cmd)
		if m == nil {
			return nil
		}
		return stopHookEntry(home, shellUnquote(m[1]))
	})
}

// PermissionHookForm is StopHookForm for the PermissionRequest hook.
func PermissionHookForm(home, settings string) (string, error) {
	return hookForm(settings, permissionSpec, func(cmd string) []byte {
		m := permissionHookBinRe.FindStringSubmatch(cmd)
		if m == nil {
			return nil
		}
		return permissionHookEntry(home, shellUnquote(m[1]))
	})
}

// FormFlagged reports whether doctor flags an installed hook's form, so
// doctor --fix and the machine agent upgrade it.
func FormFlagged(form string) bool {
	return form == HookFormStale || form == HookFormUngated
}

// hookForm compares the one marked group of spec's chain with the group
// render builds from its command, nil when the command does not parse.
func hookForm(path string, spec hookSpec, render func(cmd string) []byte) (string, error) {
	groups, err := markedGroups(path, spec)
	if err != nil || len(groups) != 1 {
		return "", err
	}
	cmds, err := installedCommands(path, spec)
	if err != nil || len(cmds) != 1 {
		return "", err
	}
	switch {
	case !strings.Contains(cmds[0], " || exec "):
		return HookFormUngated, nil
	case sameJSON(groups[0], render(cmds[0])):
		return HookFormCurrent, nil
	}
	return HookFormStale, nil
}

func shellUnquote(s string) string { return strings.ReplaceAll(s, `'\''`, `'`) }

// Hook states in a settings file (StopHookState, PermissionHookState).
const (
	HookPresent  = "installed"
	HookMissing  = "missing"
	HookDisabled = "disabled" // present, but disableAllHooks is true
)

// StopHookState reports whether the user settings file under home holds the
// AMQ Stop hook, and whether that file disables all hooks. Without an
// effective hook a Claude request is admitted but never completes. It reads
// only ~/.claude/settings.json, through the installer's bounded reader;
// project or managed settings can still change the effective result.
func StopHookState(home string) (string, error) {
	return StopHookStateAt(SettingsPath(home))
}

// StopHookStateAt is StopHookState for the settings file at path.
func StopHookStateAt(path string) (string, error) {
	return hookState(path, stopHook)
}

func hookState(path string, spec hookSpec) (string, error) {
	raw, err := readRegularBounded(path, maxSettingsBytes)
	if errors.Is(err, os.ErrNotExist) {
		return HookMissing, nil
	}
	if err != nil {
		return "", err
	}
	loc, err := locateSettings(raw, spec.event)
	if err != nil {
		return "", err
	}
	found := false
	if loc.stop != nil {
		els, err := arrayElements(raw, *loc.stop)
		if err != nil {
			return "", err
		}
		for _, el := range els {
			if spec.owns(raw[el[0]:el[1]]) {
				found = true
			}
		}
	}
	switch {
	case !found:
		return HookMissing, nil
	case disablesAllHooks(raw):
		return HookDisabled, nil
	}
	return HookPresent, nil
}

// HooksDisabled reports whether the user settings file under home sets
// disableAllHooks, so Claude Code runs none of its hooks unless a project
// or managed setting turns them on again.
func HooksDisabled(home string) (bool, error) {
	return HooksDisabledAt(SettingsPath(home))
}

// HooksDisabledAt is HooksDisabled for the settings file at path.
func HooksDisabledAt(path string) (bool, error) {
	raw, err := readRegularBounded(path, maxSettingsBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return disablesAllHooks(raw), nil
}

func disablesAllHooks(raw []byte) bool {
	var root struct {
		DisableAllHooks bool `json:"disableAllHooks"`
	}
	_ = json.Unmarshal(raw, &root)
	return root.DisableAllHooks
}

// SettingsPath is the user settings.json Claude Code reads for the user
// whose home is home, and the file every AMQ Claude hook installer writes.
func SettingsPath(home string) string {
	return filepath.Join(harness.ClaudeConfigDir(home), "settings.json")
}

// removeHook removes spec's marked entries from the settings file at path.
func removeHook(path string, spec hookSpec) error {
	_, err := editSettings(path, nil, removeEdit(spec))
	return err
}

func removeEdit(spec hookSpec) func([]byte) ([]byte, error) {
	return func(raw []byte) ([]byte, error) { return rawRemoveEntries(raw, spec) }
}

// stopHookCommand is the shell command the hook runs: the ownership marker
// assignment, then a gate of shell tests, then the single-quoted binary,
// exec'd so Claude's signals reach it. The gate repeats what the receiver
// reads before it writes (stopSessionAllowed): the binding env path, the
// binding file and every binding in the bindings directory under the AMQ
// home that home resolves now, and the attachment sentinels under $HOME,
// the home the receiver resolves. With none of them present the receiver
// writes nothing, so the hook starts nothing.
func stopHookCommand(home, bin string) string {
	remote := filepath.Join(amqhome.Under(home), "remote")
	return stopHookMarker + `[ -z "${` + binding.EnvPath + `-}" ] && [ ! -e ` + shellQuote(filepath.Join(remote, "binding.json")) +
		` ] && [ ! -e "$HOME/.claude/sessions/amq-bound" ] && ` + noLiveBindingTest(filepath.Join(remote, "bindings")) +
		` || exec ` + shellQuote(bin) + " claude stop-hook"
}

// noLiveBindingTest is a shell test that succeeds when the bindings
// directory dir holds no binding. A tombstone is no binding, so a directory
// of tombstones alone passes. A path the shell cannot list as a plain
// directory, or an entry it cannot classify, fails the test, so the
// receiver decides. Under zsh an unmatched glob is an error, so the test
// turns on nullglob there; sh and bash keep the pattern itself, which
// names no file.
func noLiveBindingTest(dir string) string {
	d := shellQuote(dir)
	return `{ [ -z "${ZSH_VERSION-}" ] || setopt nullglob; } && [ ! -L ` + d + ` ] && { [ ! -e ` + d +
		` ] || { [ -d ` + d + ` ] && [ -r ` + d + ` ] && [ -x ` + d + ` ] && f= && for f in ` + d +
		`/*.json; do case $f in *` + binding.TombstoneSuffix + `) f= ;; esac; { [ -z "$f" ] || [ ! -e "$f" ]; } && [ ! -L "$f" ] || break; f=; done && [ -z "$f" ]; }; }`
}

// shellQuote single-quotes s for POSIX sh (Claude Code runs hook commands
// through the shell).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// permissionHookCommand runs the PermissionRequest receiver by its
// absolute path, exec'd so Claude's TERM reaches it, and only
// when something can post a dialog: a machine agent's marker, or a binding
// (the binding env path, the binding file, a named binding that is not a
// tombstone) under the AMQ home that home resolves now. With none, no
// dialog can reach Buzz, so the hook starts nothing and the terminal dialog
// decides.
func permissionHookCommand(home, bin string) string {
	remote := filepath.Join(amqhome.Under(home), "remote")
	return permissionHookMarker + `[ ! -e "${` + binding.EnvPath + `-}" ] && [ ! -e ` + shellQuote(filepath.Join(remote, "binding.json")) +
		` ] && ` + noLiveBindingTest(filepath.Join(remote, "bindings")) + ` && f= && for f in ` +
		shellQuote(filepath.Join(remote, "acp", "agents", "_machine")) + `/*.json; do [ -e "$f" ] && break; f=; done && [ -z "$f" ] || exec ` +
		shellQuote(bin) + " claude permission-hook"
}

// permissionHookTimeout is the installed timeout of the PermissionRequest
// hook, in seconds, and the hook's own deadline. The hook waits until Buzz
// or the terminal answers, so the timeout only bounds a dialog nobody
// answers for a day. Claude Code documents no maximum.
const permissionHookTimeout = 86400

// permissionHookEntry is the PermissionRequest group: no matcher, and the
// timeout permissionHookTimeout.
func permissionHookEntry(home, bin string) []byte {
	return commandHookEntry(CommandHook{Command: permissionHookCommand(home, bin), Timeout: permissionHookTimeout})
}

// stopHookEntry renders one Stop matcher group in Claude Code's settings
// schema: an OUTER object whose "hooks" array holds the command hook
// (https://code.claude.com/docs/en/hooks#configuration and
// internal/keepalive/hookinstall's SessionStart entry use the same shape).
// Stop takes no matcher. The receiver appends one line, so 5 seconds bounds
// a stuck file system without holding a stop longer.
func stopHookEntry(home, bin string) []byte {
	return commandHookEntry(CommandHook{Command: stopHookCommand(home, bin), Timeout: 5})
}

// commandHookEntry renders one matcher group holding one command hook. An
// empty matcher writes none: the group matches every tool or source.
func commandHookEntry(h CommandHook) []byte {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
		Async   bool   `json:"async,omitempty"`
	}
	// No HTML escaping: a gate's && stays readable in the settings file.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		Matcher string `json:"matcher,omitempty"`
		Hooks   []hook `json:"hooks"`
	}{Matcher: h.Matcher, Hooks: []hook{{Type: "command", Command: h.Command, Timeout: h.Timeout, Async: h.Async}}})
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// cursorHookEntry renders one Cursor hook script: the command itself, with
// no "type" (Cursor defaults to command) and no matcher when it is empty.
// Before it parses a hooks file, Cursor deletes every "//" to the end of
// its line and everything from "/*" to the next "*/", inside strings too,
// so the entry writes each "/" as \/ and each "*" as \u002a: its bytes
// hold none of those sequences, and it decodes to the same command.
func cursorHookEntry(command, matcher string, timeout int) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		Command string `json:"command"`
		Matcher string `json:"matcher,omitempty"`
		Timeout int    `json:"timeout"`
	}{Command: command, Matcher: matcher, Timeout: timeout})
	out := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	out = bytes.ReplaceAll(out, []byte("/"), []byte(`\/`))
	return bytes.ReplaceAll(out, []byte("*"), []byte(`\u002a`))
}

// jsonSpan is one container value in the raw bytes: open is the index of
// its '{' or '[', close the index of the matching '}' or ']'. keyStart is
// the index of the member key's opening quote, or -1 when unknown.
type jsonSpan struct {
	keyStart, open, close int
	empty                 bool
}

// settingsLoc is where the root object, hooks object and the event's array
// (Stop or PermissionRequest) live.
type settingsLoc struct {
	root  jsonSpan
	hooks *jsonSpan
	stop  *jsonSpan
}

// locFrame is one open container during the locator walk.
type locFrame struct {
	isObj, wantKey bool
	path, key      string
	keyStart       int // opening quote of the current key (objects)
	open           int // index of this container's '{' or '['
	ownKeyStart    int // opening quote of this container's key in its parent
	members        int
}

// locateSettings walks the decoder token stream, tracking object keys vs
// values (a string VALUE never counts as a key), and records the byte
// spans of the root object, root.hooks and root.hooks.Stop. Numbers are
// decoded as json.Number so nothing here can lose precision. It refuses a
// non-object root, a non-object hooks, a non-array Stop, and duplicate
// hooks/Stop keys — editing any of those would hide or clobber content.
// A hooks object without Stop is recognized, so no duplicate top-level hooks
// key is added.
func locateSettings(raw []byte, event string) (settingsLoc, error) {
	hooksPath, stopPath := "$/hooks", "$/hooks/"+event
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var loc settingsLoc
	var stack []*locFrame
	top := func() *locFrame {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}
	// childPath is the path of a value about to be read under the top frame.
	childPath := func() string {
		f := top()
		switch {
		case f == nil:
			return "$"
		case f.isObj:
			return f.path + "/" + f.key
		default:
			return f.path + "/[]"
		}
	}
	valueDone := func() {
		if f := top(); f != nil {
			f.members++
			if f.isObj {
				f.wantKey = true
			}
		}
	}
	refuseShape := func(path string, isObj, isArr bool) error {
		switch {
		case path == hooksPath && !isObj:
			return errors.New("settings hooks is not an object; refusing to edit")
		case path == stopPath && !isArr:
			return fmt.Errorf("settings hooks.%s is not an array; refusing to edit", event)
		}
		return nil
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return loc, fmt.Errorf("parse settings: %w", err)
		}
		off := int(dec.InputOffset())
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			f := top()
			stack = stack[:len(stack)-1]
			sp := jsonSpan{keyStart: f.ownKeyStart, open: f.open, close: off - 1, empty: f.members == 0}
			switch f.path {
			case "$":
				loc.root = sp
			case hooksPath:
				loc.hooks = &sp
			case stopPath:
				loc.stop = &sp
			}
			valueDone()
			continue
		}
		if s, ok := tok.(string); ok {
			if f := top(); f != nil && f.isObj && f.wantKey {
				f.key, f.wantKey, f.keyStart = s, false, -1
				// The key token ends at off. Its opening quote is found by
				// re-encoding the decoded key; a key spelled with escapes
				// does not match and keeps keyStart -1, which only disables
				// empty-member removal (the element itself is still cut).
				enc, _ := json.Marshal(s)
				if st := off - len(enc); st >= 0 && bytes.Equal(raw[st:off], enc) {
					f.keyStart = st
				}
				continue
			}
		}
		path := childPath()
		d, isDelim := tok.(json.Delim)
		if path == "$" && (!isDelim || d != '{') {
			return loc, errors.New("settings root is not a JSON object; refusing")
		}
		if err := refuseShape(path, isDelim && d == '{', isDelim && d == '['); err != nil {
			return loc, err
		}
		if !isDelim {
			valueDone()
			continue
		}
		if (path == hooksPath && loc.hooks != nil) || (path == stopPath && loc.stop != nil) {
			return loc, fmt.Errorf("settings has a duplicate %s key; refusing to edit", strings.TrimPrefix(path, "$/"))
		}
		ownKey := -1
		if f := top(); f != nil && f.isObj {
			ownKey = f.keyStart
		}
		stack = append(stack, &locFrame{isObj: d == '{', wantKey: d == '{', path: path, open: off - 1, ownKeyStart: ownKey})
	}
	if len(stack) != 0 || loc.root.close == 0 {
		return loc, errors.New("settings is not a complete JSON object; refusing")
	}
	return loc, nil
}

// arrayElements returns the byte spans [start,end) of each element of the
// array at span arr. Leading whitespace and separating commas are skipped
// so a span is exactly the element's bytes.
func arrayElements(raw []byte, arr jsonSpan) ([][2]int, error) {
	seg := raw[arr.open : arr.close+1]
	dec := json.NewDecoder(bytes.NewReader(seg))
	dec.UseNumber()
	if _, err := dec.Token(); err != nil { // '['
		return nil, err
	}
	var out [][2]int
	for dec.More() {
		s := int(dec.InputOffset())
		for s < len(seg) && (isJSONSpace(seg[s]) || seg[s] == ',') {
			s++
		}
		var el json.RawMessage
		if err := dec.Decode(&el); err != nil {
			return nil, fmt.Errorf("parse settings hook entry: %w", err)
		}
		out = append(out, [2]int{arr.open + s, arr.open + int(dec.InputOffset())})
	}
	return out, nil
}

// groupHooksArray returns the span of the "hooks" array member of the
// matcher-group object at raw[obj[0]:obj[1]], or false when it has none.
func groupHooksArray(raw []byte, obj [2]int) (jsonSpan, bool) {
	seg := raw[obj[0]:obj[1]]
	dec := json.NewDecoder(bytes.NewReader(seg))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return jsonSpan{}, false
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return jsonSpan{}, false
		}
		start := int(dec.InputOffset())
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return jsonSpan{}, false
		}
		if k != "hooks" {
			continue
		}
		for start < len(seg) && (isJSONSpace(seg[start]) || seg[start] == ':') {
			start++
		}
		end := int(dec.InputOffset())
		if start >= end || seg[start] != '[' {
			return jsonSpan{}, false
		}
		return jsonSpan{keyStart: -1, open: obj[0] + start, close: obj[0] + end - 1, empty: len(bytes.TrimSpace(v[1:len(v)-1])) == 0}, true
	}
	return jsonSpan{}, false
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// insertLast inserts item right after the array's last element, with a
// tight leading comma, or as its only element when it is empty.
func insertLast(raw []byte, c jsonSpan, item []byte) []byte {
	if c.empty {
		return insertFirst(raw, c, item)
	}
	at := c.close
	for at > c.open && isJSONSpace(raw[at-1]) {
		at--
	}
	var b bytes.Buffer
	b.Write(raw[:at])
	b.WriteByte(',')
	b.Write(item)
	b.Write(raw[at:])
	return b.Bytes()
}

// insertFirst inserts item right after the container's opening delimiter,
// with a tight trailing comma when the container already has members.
func insertFirst(raw []byte, c jsonSpan, item []byte) []byte {
	var b bytes.Buffer
	b.Write(raw[:c.open+1])
	b.Write(item)
	if !c.empty {
		b.WriteByte(',')
	}
	b.Write(raw[c.open+1:])
	return b.Bytes()
}

// cutItem removes raw[start:end) plus one separating comma: the comma
// directly following the item (the form insertFirst writes), else the
// nearest preceding comma (the item was last).
func cutItem(raw []byte, start, end int) []byte {
	if end < len(raw) && raw[end] == ',' {
		end++
	} else {
		j := end
		for j < len(raw) && isJSONSpace(raw[j]) {
			j++
		}
		if j < len(raw) && raw[j] == ',' {
			end = j + 1
		} else {
			i := start - 1
			for i >= 0 && isJSONSpace(raw[i]) {
				i--
			}
			if i >= 0 && raw[i] == ',' {
				start = i
			}
		}
	}
	out := make([]byte, 0, len(raw)-(end-start))
	out = append(out, raw[:start]...)
	return append(out, raw[end:]...)
}

// rawInsertEntry appends the matcher group to spec's event chain by byte
// surgery, creating the event array (and "hooks") when absent. Returns
// nil,nil when an owned group already exists.
func rawInsertEntry(raw []byte, spec hookSpec, entry []byte) ([]byte, error) {
	loc, err := locateSettings(raw, spec.event)
	if err != nil {
		return nil, err
	}
	switch {
	case loc.stop != nil:
		els, err := arrayElements(raw, *loc.stop)
		if err != nil {
			return nil, err
		}
		for _, e := range els {
			if spec.owns(raw[e[0]:e[1]]) {
				return nil, nil // already installed
			}
		}
		return insertLast(raw, *loc.stop, entry), nil
	case loc.hooks != nil:
		member := append([]byte(`"`+spec.event+`":[`), entry...)
		return insertFirst(raw, *loc.hooks, append(member, ']')), nil
	default:
		member := append([]byte(`"hooks":{"`+spec.event+`":[`), entry...)
		return insertFirst(raw, loc.root, append(member, "]}"...)), nil
	}
}

// rawRemoveStopEntries removes every marked hook from the Stop chain by byte
// surgery, one cut per pass: a matcher group whose hooks are all ours is
// cut whole; in a group that also holds foreign hooks only our inner hook
// is cut and the foreign hooks stay (a mixed group is never skipped, which
// would report success with our hook still installed). Containers are never
// removed (see the header). No marked hook -> nil,nil (no write).
func rawRemoveEntries(raw []byte, spec hookSpec) ([]byte, error) {
	removed := false
	for {
		loc, err := locateSettings(raw, spec.event)
		if err != nil {
			return nil, err
		}
		if loc.stop == nil {
			break
		}
		next, cut, err := cutOneMarkedHook(raw, *loc.stop, spec)
		if err != nil {
			return nil, err
		}
		if !cut {
			break
		}
		raw, removed = next, true
	}
	if !removed {
		return nil, nil
	}
	return raw, nil
}

// cutOneMarkedHook performs the first applicable cut in the Stop array.
func cutOneMarkedHook(raw []byte, stop jsonSpan, spec hookSpec) ([]byte, bool, error) {
	groups, err := arrayElements(raw, stop)
	if err != nil {
		return nil, false, err
	}
	for i, g := range groups {
		if spec.duplicate(raw, groups, i) {
			return cutItem(raw, g[0], g[1]), true, nil
		}
		if spec.flat {
			if spec.ownsHook(raw[g[0]:g[1]]) {
				return cutItem(raw, g[0], g[1]), true, nil
			}
			continue
		}
		arr, ok := groupHooksArray(raw, g)
		if !ok {
			continue
		}
		hooks, err := arrayElements(raw, arr)
		if err != nil {
			return nil, false, err
		}
		ours := 0
		first := -1
		for i, h := range hooks {
			if spec.ownsHook(raw[h[0]:h[1]]) {
				ours++
				if first < 0 {
					first = i
				}
			}
		}
		switch {
		case ours == 0:
			continue
		case ours == len(hooks):
			return cutItem(raw, g[0], g[1]), true, nil
		default:
			h := hooks[first]
			return cutItem(raw, h[0], h[1]), true, nil
		}
	}
	return raw, false, nil
}

// StopHookPayload is the subset of Claude Code's Stop-hook stdin payload
// the receiver consumes (session identity + completion). Claude Code writes
// the transcript asynchronously, so the turn's final text may reach the
// transcript only after the hook runs; LastAssistantMessage carries it.
type StopHookPayload struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	Cwd                  string `json:"cwd"`
	HookEventName        string `json:"hook_event_name"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

// maxStopTextBytes bounds the final text one marker line carries. Escaped
// as JSON it stays within maxStopMarkerBytes, so a line is always read
// whole.
const maxStopTextBytes = 128 * 1024

// maxStopPayloadBytes bounds the hook's stdin. The payload carries the
// turn's whole final text, JSON-escaped; a cut payload does not decode and
// would leave the turn without its marker.
const maxStopPayloadBytes = 64 << 20

// sessionIDRe bounds the session id the receiver will turn into a file
// name: no separators, no dot segments.
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// RunStopHookReceiver is the fail-open receiver body: it appends one
// turn-completion marker line {"ts":<unix ms>,"session_id":"<sid>"}, with
// the turn's final text as "text" (and "truncated" when it was cut), to
// ~/.claude/sessions/amq-stop/<sid>.jsonl for the adapter's confirmation
// poller. EVERY error exits 0 — the hook must never block Claude Code
// (exit 2 is the harness's blocking code). It must also never HANG: the
// session id is validated before it becomes a path, and the marker is
// opened no-follow and non-blocking so a symlink or FIFO planted at the
// leaf is refused rather than followed or waited on. Scope limits who may
// append. It does not cap the file. With no binding
// file the receiver follows the attachment sentinel, so user off leaves
// writes in place while that attachment still owns the session.
func RunStopHookReceiver(home string, stdin io.Reader, stdout io.Writer) int {
	defer func() { _ = recover() }() // fail-open, unconditionally
	_ = stdout
	if !noFollowSupported {
		return 0 // no safe create on this platform
	}
	var payload StopHookPayload
	raw, err := io.ReadAll(io.LimitReader(stdin, maxStopPayloadBytes))
	if err != nil {
		return 0
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0
	}
	if !sessionIDRe.MatchString(payload.SessionID) {
		return 0
	}
	// An unbound session writes nothing. The check is before the marker
	// directory is created.
	if !stopSessionAllowed(home, payload.SessionID) {
		return 0
	}
	marker := stopMarkerPath(home, payload.SessionID)
	dir := filepath.Dir(marker)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return 0 // a symlinked marker directory is never written through
	}
	pre, preErr := os.Lstat(marker)
	if preErr == nil && !pre.Mode().IsRegular() {
		return 0
	}
	f, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY|openNoFollowFlag, 0o600)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	// Same-file check before writing, behind the no-follow open: the description must
	// be the regular file the path names without following a link — the
	// one lstat saw, or, when the file was just created, the one a fresh
	// lstat sees now.
	post, err := f.Stat()
	if err != nil || !post.Mode().IsRegular() {
		return 0
	}
	if preErr != nil {
		if pre, err = os.Lstat(marker); err != nil || !pre.Mode().IsRegular() {
			return 0
		}
	}
	if !os.SameFile(pre, post) {
		return 0
	}
	m := map[string]any{"ts": time.Now().UnixMilli(), "session_id": payload.SessionID}
	if payload.LastAssistantMessage != "" {
		text, cut := protocol.TruncateText(payload.LastAssistantMessage, maxStopTextBytes)
		m["text"] = text
		if cut {
			m["truncated"] = true
		}
	}
	line, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	_, _ = f.Write(append(line, '\n'))
	return 0
}

func stopMarkerPath(home, sessionID string) string {
	return filepath.Join(markerDir(home), "amq-stop", sessionID+".jsonl")
}

func stopBoundDir(home, sessionID string) string {
	return filepath.Join(markerDir(home), "amq-bound", sessionID)
}

// bindStopSession records one attachment's ownership of this session.
// Each attachment gets its own sentinel file, so one attachment's cleanup
// cannot drop a replacement that still owns the session.
func bindStopSession(home, sessionID string) (string, error) {
	if !sessionIDRe.MatchString(sessionID) {
		return "", fmt.Errorf("stop session %q is not a file-safe id", sessionID)
	}
	if !noFollowSupported {
		return "", errUnsupportedPlatform
	}
	dir := stopBoundDir(home, sessionID)
	var f *os.File
	// Another attachment's unbind removes an empty directory, so a create
	// that loses that race makes the directory again, once.
	for attempt := 0; ; attempt++ {
		err := os.MkdirAll(dir, 0o700)
		if err == nil {
			var fi os.FileInfo
			if fi, err = os.Lstat(dir); err == nil && !fi.IsDir() {
				return "", fmt.Errorf("stop bound dir %s is not a plain directory", dir)
			}
		}
		if err == nil {
			f, err = os.CreateTemp(dir, "owner-")
		}
		if err == nil {
			break
		}
		if attempt > 0 || !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	token := filepath.Base(f.Name())
	if err := f.Close(); err != nil {
		return "", err
	}
	return token, nil
}

// unbindStopSession drops one attachment's sentinel when that attachment
// unsubscribes. The marker stays while any other attachment still owns the
// session. This is not user off: off removes only the Buzz binding.
func unbindStopSession(home, sessionID, token string) {
	if !sessionIDRe.MatchString(sessionID) || !noFollowSupported || token == "" {
		return
	}
	removeRegular(filepath.Join(stopBoundDir(home, sessionID), token))
	if stopSentinelPresent(home, sessionID) {
		return
	}
	removeRegular(stopMarkerPath(home, sessionID))
	// The installed gate tests amq-bound itself, so the empty directories
	// go too: with nothing attached the Stop hook starts nothing.
	removeEmptyDir(stopBoundDir(home, sessionID))
	removeEmptyDir(filepath.Dir(stopBoundDir(home, sessionID)))
}

// removeEmptyDir removes path when it is a plain empty directory; a
// directory that holds anything stays.
func removeEmptyDir(path string) {
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		_ = os.Remove(path)
	}
}

func removeRegular(path string) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	_ = os.Remove(path)
}

// stopSessionAllowed reports whether this Stop may write. A binding file
// decides on its own: write only when it names this session. Mailbox
// bindings have no native session, so a stale sentinel must not write.
// With no binding file, an attached sentinel is enough, including after
// user off while the endpoint attachment is still subscribed.
func stopSessionAllowed(home, sessionID string) bool {
	switch bindingDecision(home, sessionID) {
	case bindingAllow:
		return true
	case bindingDeny:
		return false
	default:
		return stopSentinelPresent(home, sessionID)
	}
}

func stopSentinelPresent(home, sessionID string) bool {
	entries, err := os.ReadDir(stopBoundDir(home, sessionID))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			return true
		}
	}
	return false
}

const (
	bindingAbsent = iota
	bindingAllow
	bindingDeny
)

// bindingDecision reads every binding file: the legacy binding.json and each
// bindings/*.json beside it. Any readable file naming this session allows.
// Otherwise any binding file at all denies, readable or not. The hook
// process has only the Claude home, so an unset AMQ_REMOTE_BINDING resolves
// under that home.
func bindingDecision(home, sessionID string) int {
	path := filepath.Join(amqhome.Under(home), "remote", "binding.json")
	if p := strings.TrimSpace(os.Getenv(binding.EnvPath)); p != "" && filepath.IsAbs(p) {
		path = filepath.Clean(p)
	}
	exists := false
	if raw, err := readRegularBounded(path, bindingMaxBytes); !errors.Is(err, os.ErrNotExist) {
		exists = true
		if err == nil && nativeSessionIs(raw, sessionID) {
			return bindingAllow
		}
	}
	allow, present := bindingsDirState(filepath.Join(filepath.Dir(path), "bindings"), sessionID)
	if allow {
		return bindingAllow
	}
	if exists || present {
		return bindingDeny
	}
	return bindingAbsent
}

func nativeSessionIs(raw []byte, sessionID string) bool {
	var b struct {
		NativeSession string `json:"native_session"`
	}
	return json.Unmarshal(raw, &b) == nil && b.NativeSession == sessionID
}

// bindingMaxBytes matches the binding package's read bound.
const bindingMaxBytes = 64 << 10
