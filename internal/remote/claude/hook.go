package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
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
// home/.claude/settings.json. Idempotent: the same entry is left untouched,
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
// exists: AMQ_REMOTE_BINDING, a binding file, the bindings directory, or
// the attachment sentinels. Everywhere else the hook costs one sh.
func InstallStopHook(home, bin string) error {
	return replaceHook(settingsPath(home), stopHook, stopHookEntry(home, bin))
}

// UninstallStopHook removes marked entries from the Stop chain. When no
// marked entry exists the file is untouched (no rewrite, no mtime churn).
func UninstallStopHook(home string) error {
	return removeHook(settingsPath(home), stopHook)
}

// hookSpec names one hook event chain in settings.json and the marker that
// owns AMQ's entry in it. The Stop and PermissionRequest entries share one
// installer and differ only here.
type hookSpec struct {
	event, marker string
}

var (
	stopHook       = hookSpec{event: stopHookType, marker: stopHookMarker}
	permissionSpec = hookSpec{event: permissionHookType, marker: permissionHookMarker}
)

// InstallPermissionHook adds the PermissionRequest hook that answers DM
// approvals: one entry with no matcher (every tool, so a reject can reach
// any call), marked AMQ_APPROVAL_HOOK=1, whose own deadline is wait and
// whose installed timeout is wait plus PermissionHookGrace. It is inert in
// a session no relay share with approve is serving, and its command starts
// amq-remote only when some session has a pin file.
//
// pin.Owner, 64 lowercase hex, pins the owner pubkey on the hook's command
// line: the only key whose signed reaction can allow a call;
// pin.Root (and pin.Session) pin where the share is read.
// Claude cannot edit its settings file without a prompt, so it cannot
// change the pin on its own. An empty owner installs a reject-only hook.
// A marked entry with another command is replaced; the same one is kept.
func InstallPermissionHook(home, bin string, wait time.Duration, pin HookPin) error {
	if pin != (HookPin{}) && !pin.Complete() {
		return fmt.Errorf("an owner pin needs the owner, an absolute AMQ root, the share session, its relay, body, DM channel and target: %+v", pin)
	}
	return replaceHook(settingsPath(home), permissionSpec, permissionHookEntry(home, bin, wait, pin))
}

// replaceHook makes entry the only marked entry of spec's chain in the
// settings file at path. A single marked group equal to entry, field for
// field, is kept untouched; anything else marked (an older binary path,
// other flags, another timeout or matcher) is replaced in one write.
func replaceHook(path string, spec hookSpec, entry []byte) error {
	groups, err := markedGroups(path, spec)
	if err != nil {
		return err
	}
	if len(groups) == 1 && sameJSON(groups[0], entry) {
		return nil // already installed as asked
	}
	return editSettings(path, true, func(raw []byte) ([]byte, error) {
		if cut, err := rawRemoveEntries(raw, spec); err != nil {
			return nil, err
		} else if cut != nil {
			raw = cut
		}
		return rawInsertEntry(raw, spec, entry)
	})
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
	raw, err := readRegularBounded(path, maxSettingsBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	loc, err := locateSettings(raw, spec.event)
	if err != nil || loc.stop == nil {
		return nil, err
	}
	groups, err := arrayElements(raw, *loc.stop)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, g := range groups {
		if groupHasOurHook(raw[g[0]:g[1]], spec.marker) {
			out = append(out, raw[g[0]:g[1]])
		}
	}
	return out, nil
}

// installedCommands lists the commands of spec's marked hooks in the
// settings file at path.
func installedCommands(path string, spec hookSpec) ([]string, error) {
	groups, err := markedGroups(path, spec)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, g := range groups {
		var group struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		}
		if json.Unmarshal(g, &group) != nil {
			continue
		}
		for _, h := range group.Hooks {
			if strings.HasPrefix(h.Command, spec.marker) {
				out = append(out, h.Command)
			}
		}
	}
	return out, nil
}

// pinFlagRe is one pin flag as permissionHookCommand writes it: a name and
// a single-quoted value.
var pinFlagRe = regexp.MustCompile(` --([a-z]+) '((?:[^']|'\\'')*)'`)

// PermissionHookPin is the pin of the installed PermissionRequest hook,
// empty when there is no hook, no pin, or more than one hook.
func PermissionHookPin(home string) HookPin {
	cmds, err := installedCommands(settingsPath(home), permissionSpec)
	if err != nil || len(cmds) != 1 {
		return HookPin{}
	}
	var pin HookPin
	for _, m := range pinFlagRe.FindAllStringSubmatch(cmds[0], -1) {
		if f := pin.field(m[1]); f != nil {
			*f = shellUnquote(m[2])
		}
	}
	return pin
}

// pinFlags are the pin's command line flags, in the order written.
var pinFlags = []string{"owner", "root", "session", "relay", "body", "channel", "target"}

func (p *HookPin) field(name string) *string {
	switch name {
	case "owner":
		return &p.Owner
	case "root":
		return &p.Root
	case "session":
		return &p.Session
	case "relay":
		return &p.Relay
	case "body":
		return &p.Body
	case "channel":
		return &p.Channel
	case "target":
		return &p.Target
	}
	return nil
}

// PinWarnings names the user settings that defeat the owner pin: a
// bypassPermissions or auto default mode, or a permissions.allow entry for
// every Bash command. Each lets Claude rewrite its settings, or run
// install-approval-hook with another owner, without a prompt. Only
// ~/.claude/settings.json is read; managed settings can add the same, and a
// session started with --permission-mode on the command line is not visible
// here.
func PinWarnings(home string) ([]string, error) {
	raw, err := readRegularBounded(settingsPath(home), maxSettingsBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st struct {
		Permissions struct {
			DefaultMode string   `json:"defaultMode"`
			Allow       []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	var out []string
	if st.Permissions.DefaultMode == "bypassPermissions" {
		out = append(out, "permissions.defaultMode is bypassPermissions: Claude can rewrite the approval hook's owner pin without a prompt")
	}
	if st.Permissions.DefaultMode == "auto" {
		out = append(out, "permissions.defaultMode is auto: Claude can rewrite the approval hook's owner pin without a prompt")
	}
	for _, a := range st.Permissions.Allow {
		if a := strings.TrimSpace(a); a == "Bash" || a == "Bash(*)" {
			out = append(out, fmt.Sprintf("permissions.allow holds %q: Claude can rewrite the approval hook's owner pin without a prompt", a))
			break
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
}

// InstallCommandHooks makes each hook the only entry marked with its marker
// in its event chain of the user settings file, with replaceHook's rules:
// the same entry is kept and any other marked entry is replaced.
func InstallCommandHooks(home string, hooks []CommandHook) error {
	return InstallCommandHooksAt(settingsPath(home), hooks)
}

// InstallCommandHooksAt is InstallCommandHooks for the hooks file at path,
// any file with the {"hooks":{event:[group...]}} shape.
func InstallCommandHooksAt(path string, hooks []CommandHook) error {
	for _, h := range hooks {
		spec := hookSpec{event: h.Event, marker: h.Marker}
		if err := replaceHook(path, spec, commandHookEntry(h.Command, h.Matcher, h.Timeout)); err != nil {
			return err
		}
	}
	return nil
}

// UninstallCommandHooks removes the entries marked with marker from each
// event chain of the user settings file.
func UninstallCommandHooks(home, marker string, events []string) error {
	return UninstallCommandHooksAt(settingsPath(home), marker, events)
}

// UninstallCommandHooksAt is UninstallCommandHooks for the hooks file at
// path.
func UninstallCommandHooksAt(path, marker string, events []string) error {
	for _, event := range events {
		if err := removeHook(path, hookSpec{event: event, marker: marker}); err != nil {
			return err
		}
	}
	return nil
}

// MarkedHookCommands lists the commands marked with marker in the event
// chain of the user settings file. A missing file lists none.
func MarkedHookCommands(home, event, marker string) ([]string, error) {
	return MarkedHookCommandsAt(settingsPath(home), event, marker)
}

// MarkedHookCommandsAt is MarkedHookCommands for the hooks file at path.
func MarkedHookCommandsAt(path, event, marker string) ([]string, error) {
	return installedCommands(path, hookSpec{event: event, marker: marker})
}

// UninstallPermissionHook removes only the marked PermissionRequest entry.
func UninstallPermissionHook(home string) error {
	return removeHook(settingsPath(home), permissionSpec)
}

// PermissionHookState is StopHookState for the PermissionRequest entry.
func PermissionHookState(home string) (string, error) {
	return hookState(home, permissionSpec)
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
	permissionHookBinRe = regexp.MustCompile(`'((?:[^']|'\\'')*)' claude permission-hook --wait ([0-9]+)`)
)

// StopHookForm reports the form of the one installed Stop hook, "" when
// the user settings file does not hold exactly one.
func StopHookForm(home string) (string, error) {
	return hookForm(home, stopHook, func(cmd string) []byte {
		m := stopHookBinRe.FindStringSubmatch(cmd)
		if m == nil {
			return nil
		}
		return stopHookEntry(home, shellUnquote(m[1]))
	})
}

// PermissionHookForm is StopHookForm for the PermissionRequest hook; the
// current form keeps the installed wait and pin.
func PermissionHookForm(home string) (string, error) {
	return hookForm(home, permissionSpec, func(cmd string) []byte {
		m := permissionHookBinRe.FindStringSubmatch(cmd)
		if m == nil {
			return nil
		}
		wait, err := strconv.Atoi(m[2])
		if err != nil {
			return nil
		}
		return permissionHookEntry(home, shellUnquote(m[1]), time.Duration(wait)*time.Second, PermissionHookPin(home))
	})
}

// hookForm compares the one marked group of spec's chain with the group
// render builds from its command, nil when the command does not parse.
func hookForm(home string, spec hookSpec, render func(cmd string) []byte) (string, error) {
	path := settingsPath(home)
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

// Stop hook states in the user settings file (StopHookState).
const (
	StopHookPresent  = "installed"
	StopHookMissing  = "missing"
	StopHookDisabled = "disabled" // present, but disableAllHooks is true
)

// StopHookState reports whether the user settings file under home holds the
// AMQ Stop hook, and whether that file disables all hooks. Without an
// effective hook a Claude request is admitted but never completes. It reads
// only ~/.claude/settings.json, through the installer's bounded reader;
// project or managed settings can still change the effective result.
func StopHookState(home string) (string, error) {
	return hookState(home, stopHook)
}

func hookState(home string, spec hookSpec) (string, error) {
	raw, err := readRegularBounded(settingsPath(home), maxSettingsBytes)
	if errors.Is(err, os.ErrNotExist) {
		return StopHookMissing, nil
	}
	if err != nil {
		return "", err
	}
	loc, err := locateSettings(raw, spec.event)
	if err != nil {
		return "", err
	}
	var root struct {
		DisableAllHooks bool `json:"disableAllHooks"`
	}
	_ = json.Unmarshal(raw, &root)
	found := false
	if loc.stop != nil {
		els, err := arrayElements(raw, *loc.stop)
		if err != nil {
			return "", err
		}
		for _, el := range els {
			if groupHasOurHook(raw[el[0]:el[1]], spec.marker) {
				found = true
			}
		}
	}
	switch {
	case !found:
		return StopHookMissing, nil
	case root.DisableAllHooks:
		return StopHookDisabled, nil
	}
	return StopHookPresent, nil
}

func settingsPath(home string) string {
	return filepath.Join(home, ".claude", "settings.json")
}

// removeHook removes spec's marked entries from the settings file at path.
func removeHook(path string, spec hookSpec) error {
	return editSettings(path, false, func(raw []byte) ([]byte, error) {
		return rawRemoveEntries(raw, spec)
	})
}

// editSettings applies edit to the bytes of the settings file at path and
// writes the result atomically; a nil result writes nothing. A missing file
// is started as {} when create is set, and is left alone otherwise.
func editSettings(path string, create bool, edit func([]byte) ([]byte, error)) error {
	raw, err := readRegularBounded(path, maxSettingsBytes)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("settings %s: %w", path, err)
		}
		if !create {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		raw = []byte("{}\n")
	}
	out, err := edit(raw)
	if err != nil {
		return fmt.Errorf("settings %s: %w", path, err)
	}
	if out == nil {
		return nil // nothing changed
	}
	if !json.Valid(out) {
		// Defense in depth: never write a file Claude Code cannot parse.
		return fmt.Errorf("settings %s: edit would produce invalid JSON; refusing", path)
	}
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), out, 0o600)
	return err
}

// stopHookCommand is the shell command the hook runs: the ownership marker
// assignment, then a gate of shell tests, then the single-quoted binary,
// exec'd so Claude's signals reach it. The gate repeats what the receiver
// reads before it writes (stopSessionAllowed): the binding env path, the
// binding file and bindings directory under the AMQ home that home
// resolves now, and the attachment sentinels under $HOME, the home the
// receiver resolves. With none of them present the receiver writes
// nothing, so the hook starts nothing.
func stopHookCommand(home, bin string) string {
	remote := filepath.Join(amqhome.Under(home), "remote")
	return stopHookMarker + `[ -z "${` + binding.EnvPath + `-}" ] && [ ! -e ` + shellQuote(filepath.Join(remote, "binding.json")) +
		` ] && [ ! -e ` + shellQuote(filepath.Join(remote, "bindings")) +
		` ] && [ ! -e "$HOME/.claude/sessions/amq-bound" ] || exec ` + shellQuote(bin) + " claude stop-hook"
}

// shellQuote single-quotes s for POSIX sh (Claude Code runs hook commands
// through the shell).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// permissionHookCommand runs the PermissionRequest receiver with its wait
// and the pin, exec'd so Claude's TERM reaches it, and only when some
// session under home's amq-approve directory has a pin file: the receiver
// decides nothing without a live pin. The glob runs inside sh; with no
// match "$1" is the pattern itself, which does not exist.
func permissionHookCommand(home, bin string, wait time.Duration, pin HookPin) string {
	approve := filepath.Join(claudeSessionsDir(home), "amq-approve")
	cmd := permissionHookMarker + "set -- " + shellQuote(approve) + `/*/pin-*; [ ! -e "$1" ] || exec ` +
		shellQuote(bin) + " claude permission-hook --wait " + strconv.Itoa(int(wait/time.Second))
	if pin != (HookPin{}) {
		for _, name := range pinFlags {
			cmd += " --" + name + " " + shellQuote(*pin.field(name))
		}
	}
	return cmd
}

// permissionHookEntry is the PermissionRequest group: no matcher, and a
// timeout PermissionHookGrace longer than the hook's own wait, so the hook
// ends first and the terminal dialog decides.
func permissionHookEntry(home, bin string, wait time.Duration, pin HookPin) []byte {
	return commandHookEntry(permissionHookCommand(home, bin, wait, pin), "", int((wait+PermissionHookGrace)/time.Second))
}

// stopHookEntry renders one Stop matcher group in Claude Code's settings
// schema: an OUTER object whose "hooks" array holds the command hook
// (https://code.claude.com/docs/en/hooks#configuration and
// internal/keepalive/hookinstall's SessionStart entry use the same shape).
// Stop takes no matcher. The receiver appends one line, so 5 seconds bounds
// a stuck file system without holding a stop longer.
func stopHookEntry(home, bin string) []byte {
	return commandHookEntry(stopHookCommand(home, bin), "", 5)
}

// commandHookEntry renders one matcher group holding one command hook. An
// empty matcher writes none: the group matches every tool or source.
func commandHookEntry(command, matcher string, timeout int) []byte {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	// No HTML escaping: a gate's && stays readable in the settings file.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		Matcher string `json:"matcher,omitempty"`
		Hooks   []hook `json:"hooks"`
	}{Matcher: matcher, Hooks: []hook{{Type: "command", Command: command, Timeout: timeout}}})
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
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

// isOurHook reports whether one inner hook object carries the marker.
func isOurHook(el []byte, marker string) bool {
	var h struct {
		Command string `json:"command"`
	}
	return json.Unmarshal(el, &h) == nil && strings.HasPrefix(h.Command, marker)
}

// groupHasOurHook reports whether a matcher group holds any marked hook.
func groupHasOurHook(el []byte, marker string) bool {
	var g struct {
		Hooks []json.RawMessage `json:"hooks"`
	}
	if json.Unmarshal(el, &g) != nil {
		return false
	}
	for _, h := range g.Hooks {
		if isOurHook(h, marker) {
			return true
		}
	}
	return false
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
			if groupHasOurHook(raw[e[0]:e[1]], spec.marker) {
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
		next, cut, err := cutOneMarkedHook(raw, *loc.stop, spec.marker)
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
func cutOneMarkedHook(raw []byte, stop jsonSpan, marker string) ([]byte, bool, error) {
	groups, err := arrayElements(raw, stop)
	if err != nil {
		return nil, false, err
	}
	for _, g := range groups {
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
			if isOurHook(raw[h[0]:h[1]], marker) {
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
// the receiver consumes (session identity + completion).
type StopHookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
}

// sessionIDRe bounds the session id the receiver will turn into a file
// name: no separators, no dot segments.
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// RunStopHookReceiver is the fail-open receiver body: it appends one
// turn-completion marker line {"ts":<unix ms>,"session_id":"<sid>"} to
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
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
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
	line, err := json.Marshal(map[string]any{"ts": time.Now().UnixMilli(), "session_id": payload.SessionID})
	if err != nil {
		return 0
	}
	_, _ = f.Write(append(line, '\n'))
	return 0
}

func stopMarkerPath(home, sessionID string) string {
	return filepath.Join(claudeSessionsDir(home), "amq-stop", sessionID+".jsonl")
}

func stopBoundDir(home, sessionID string) string {
	return filepath.Join(claudeSessionsDir(home), "amq-bound", sessionID)
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
