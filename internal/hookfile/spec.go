package hookfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
)

// Spec names one hook event chain in a hooks file and the marker that owns
// AMQ's entry in it. Every installer shares one writer and differs only
// here.
type Spec struct {
	Event, Marker string
	// Flat: each element of the event array is one hook script, as in
	// Cursor's hooks.json, not a matcher group that holds a "hooks" array.
	Flat bool
	// Match, when set, decides which commands are this spec's own instead
	// of the marker prefix.
	Match func(command string) bool
}

// ownsCommand reports whether a hook command is this spec's own.
func (s Spec) ownsCommand(command string) bool {
	if s.Match != nil {
		return s.Match(command)
	}
	return strings.HasPrefix(command, s.Marker)
}

// ownsHook reports whether one inner hook object is this spec's own.
func (s Spec) ownsHook(el []byte) bool {
	var h struct {
		Command string `json:"command"`
	}
	return json.Unmarshal(el, &h) == nil && s.ownsCommand(h.Command)
}

// owns reports whether one element of spec's event array is, or holds, one
// of this spec's hooks.
func (s Spec) owns(el []byte) bool {
	if s.Flat {
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

// Installed reports whether spec's event chain in the bytes of a hooks
// file holds one of spec's hooks.
func (s Spec) Installed(raw []byte) (bool, error) {
	loc, err := locateSettings(raw, s.Event)
	if err != nil || loc.stop == nil {
		return false, err
	}
	els, err := arrayElements(raw, *loc.stop)
	if err != nil {
		return false, err
	}
	for _, el := range els {
		if s.owns(raw[el[0]:el[1]]) {
			return true, nil
		}
	}
	return false, nil
}

// duplicate reports whether group i of the chain groups is a byte-identical
// copy of an earlier group and holds a command of the installer's family
// (the marker prefix), when match narrows ownership below that family. A
// second copy is never the owner's choice, so an install removes it.
func (s Spec) duplicate(raw []byte, groups [][2]int, i int) bool {
	if s.Flat || s.Match == nil || s.Marker == "" {
		return false
	}
	el := raw[groups[i][0]:groups[i][1]]
	if !(Spec{Event: s.Event, Marker: s.Marker}).owns(el) {
		return false
	}
	for _, g := range groups[:i] {
		if bytes.Equal(raw[g[0]:g[1]], el) {
			return true
		}
	}
	return false
}

// Seed is the content a missing file starts as. Cursor ignores a hooks
// file without a positive integer "version".
func (s Spec) Seed() []byte {
	if s.Flat {
		return []byte("{\"version\":1}\n")
	}
	return []byte("{}\n")
}

// Replace makes entry the only marked entry of spec's chain in the
// settings file at path, and reports whether it wrote the file. A single
// marked group equal to entry, field for field, is kept untouched without
// a lock or a write; anything else marked (an older binary path, other
// flags, another timeout or matcher) is replaced in one write.
func Replace(path string, spec Spec, entry []byte) (bool, error) {
	if same, err := InstalledAs(path, spec, entry); same || err != nil {
		return false, err
	}
	return Edit(path, spec.Seed(), ReplaceEdit(spec, entry))
}

// InstalledAs reports whether spec's one marked group in the settings file
// at path is entry, field for field.
func InstalledAs(path string, spec Spec, entry []byte) (bool, error) {
	groups, err := MarkedGroups(path, spec)
	return err == nil && len(groups) == 1 && SameJSON(groups[0], entry), err
}

// ReplaceEdit is the edit that removes spec's entries and appends entry. It
// writes nothing when the bytes already hold exactly entry.
func ReplaceEdit(spec Spec, entry []byte) func([]byte) ([]byte, error) {
	return func(raw []byte) ([]byte, error) {
		original := raw
		if cut, err := rawRemoveEntries(raw, spec); err != nil {
			return nil, err
		} else if cut != nil {
			raw = cut
		}
		out, err := InsertEntry(raw, spec, entry)
		if err != nil || out == nil || bytes.Equal(out, original) {
			return nil, err
		}
		return out, nil
	}
}

// SameJSON reports whether two JSON values hold the same members and
// values, whatever their whitespace and key order.
func SameJSON(a, b []byte) bool {
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

// MarkedGroups returns the raw matcher groups of spec's chain in the
// settings file at path that hold a marked hook. A missing file has none.
func MarkedGroups(path string, spec Spec) ([][]byte, error) {
	raw, exists, err := Read(path)
	if err != nil || !exists {
		return nil, err
	}
	groups, err := MarkedGroupsIn(raw, spec)
	if err != nil {
		return nil, fmt.Errorf("settings %s: %w", path, err)
	}
	return groups, nil
}

// Check refuses, naming path, a hooks file the writer refuses to
// edit for event: one it cannot read as a regular file within its size
// bound, one that is not a single JSON value, or one whose root, hooks or
// event value has the wrong type or a duplicate key. A missing file
// passes: the writer starts it.
func Check(path, event string) error {
	raw, exists, err := Read(path)
	if err != nil || !exists {
		return err
	}
	if !json.Valid(raw) {
		return fmt.Errorf("settings %s: not one JSON value (a syntax error or trailing JSON)", path)
	}
	if _, err := MarkedGroupsIn(raw, Spec{Event: event}); err != nil {
		return fmt.Errorf("settings %s: %w", path, err)
	}
	return nil
}

// MarkedGroupsIn is MarkedGroups for the bytes of a settings file.
func MarkedGroupsIn(raw []byte, spec Spec) ([][]byte, error) {
	loc, err := locateSettings(raw, spec.Event)
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
func checkGroup(el []byte, spec Spec) error {
	if spec.Flat {
		return nil
	}
	var g map[string]json.RawMessage
	if json.Unmarshal(el, &g) != nil {
		return nil
	}
	if h, ok := g["hooks"]; ok {
		if v := bytes.TrimSpace(h); len(v) == 0 || v[0] != '[' && !bytes.Equal(v, []byte("null")) {
			return fmt.Errorf("%s: %q must be a JSON array", spec.Event, "hooks")
		}
	}
	return nil
}

// InstalledCommands lists the commands of spec's marked hooks in the
// settings file at path.
func InstalledCommands(path string, spec Spec) ([]string, error) {
	groups, err := MarkedGroups(path, spec)
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
		if spec.Flat {
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

// CommandHook is one marked command hook in an event chain of a hooks
// file: the command starts with marker, and an empty matcher
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

// InstallCommandHooksAt makes each hook the only entry marked with its
// marker in its event chain of the hooks file at path, any file with the
// {"hooks":{event:[group...]}} shape, with Replace's rules: the same entry
// is kept and any other marked entry is replaced. It reports whether it
// changed the file.
func InstallCommandHooksAt(path string, hooks []CommandHook) (bool, error) {
	changed := false
	for _, h := range hooks {
		wrote, err := Replace(path, Spec{Event: h.Event, Marker: h.Marker, Match: h.Owns}, CommandEntry(h))
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
	_, err := Edit(path, (Spec{Flat: true}).Seed(), func(raw []byte) ([]byte, error) {
		original := raw
		for _, h := range hooks {
			spec := Spec{Event: h.Event, Marker: h.Marker, Flat: true}
			cut, err := rawRemoveEntries(raw, spec)
			if err != nil {
				return nil, err
			}
			if cut != nil {
				raw = cut
			}
			raw, err = InsertEntry(raw, spec, cursorHookEntry(h.Command, h.Matcher, h.Timeout))
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

// UninstallCommandHooksAt removes the entries marked with marker from each
// event chain of the hooks file at path.
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
		if _, err := Edit(path, nil, RemoveEdit(Spec{Event: event, Marker: marker, Flat: flat})); err != nil {
			return err
		}
	}
	return nil
}

// MarkedHookCommandsAt lists the commands marked with marker in the event
// chain of the hooks file at path. A missing file lists none.
func MarkedHookCommandsAt(path, event, marker string) ([]string, error) {
	return InstalledCommands(path, Spec{Event: event, Marker: marker})
}

// MarkedCursorHookCommandsAt is MarkedHookCommandsAt for a Cursor hooks
// file.
func MarkedCursorHookCommandsAt(path, event, marker string) ([]string, error) {
	return InstalledCommands(path, Spec{Event: event, Marker: marker, Flat: true})
}

// Remove removes spec's marked entries from the settings file at path.
func Remove(path string, spec Spec) error {
	_, err := Edit(path, nil, RemoveEdit(spec))
	return err
}

func RemoveEdit(spec Spec) func([]byte) ([]byte, error) {
	return func(raw []byte) ([]byte, error) { return rawRemoveEntries(raw, spec) }
}

// CommandEntry renders one matcher group holding one command hook. An
// empty matcher writes none: the group matches every tool or source.
func CommandEntry(h CommandHook) []byte {
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
