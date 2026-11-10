package hookfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// A hooks file is edited by BYTE SURGERY, never by decode/re-marshal:
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
// container: formatting cannot prove who created an empty event array or
// "hooks" object, so an operator's containers are preserved and ours may be
// left behind empty, which a harness treats as no hooks. A file that
// already had the event array round-trips byte for byte.

// jsonSpan is one container value in the raw bytes: open is the index of
// its '{' or '[', close the index of the matching '}' or ']'.
type jsonSpan struct {
	open, close int
	empty       bool
}

// settingsLoc is where the root object, hooks object and the event's array
// live.
type settingsLoc struct {
	root  jsonSpan
	hooks *jsonSpan
	stop  *jsonSpan
}

// locFrame is one open container during the locator walk.
type locFrame struct {
	isObj, wantKey bool
	path, key      string
	open           int // index of this container's '{' or '['
	members        int
}

// locateSettings walks the decoder token stream, tracking object keys vs
// values (a string VALUE never counts as a key), and records the byte
// spans of the root object, root.hooks and root.hooks.<event>. Numbers are
// decoded as json.Number so nothing here can lose precision. It refuses a
// non-object root, a non-object hooks, a non-array event value, and duplicate
// hooks/event keys — editing any of those would hide or clobber content.
// A hooks object without the event is recognized, so no duplicate top-level hooks
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
			sp := jsonSpan{open: f.open, close: off - 1, empty: f.members == 0}
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
				f.key, f.wantKey = s, false
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
		stack = append(stack, &locFrame{isObj: d == '{', wantKey: d == '{', path: path, open: off - 1})
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
		return jsonSpan{open: obj[0] + start, close: obj[0] + end - 1, empty: len(bytes.TrimSpace(v[1:len(v)-1])) == 0}, true
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

// InsertEntry appends the matcher group to spec's event chain by byte
// surgery, creating the event array (and "hooks") when absent. Returns
// nil,nil when an owned group already exists.
func InsertEntry(raw []byte, spec Spec, entry []byte) ([]byte, error) {
	loc, err := locateSettings(raw, spec.Event)
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
		member := append([]byte(`"`+spec.Event+`":[`), entry...)
		return insertFirst(raw, *loc.hooks, append(member, ']')), nil
	default:
		member := append([]byte(`"hooks":{"`+spec.Event+`":[`), entry...)
		return insertFirst(raw, loc.root, append(member, "]}"...)), nil
	}
}

// rawRemoveEntries removes every marked hook from the event chain by byte
// surgery, one cut per pass: a matcher group whose hooks are all ours is
// cut whole; in a group that also holds foreign hooks only our inner hook
// is cut and the foreign hooks stay (a mixed group is never skipped, which
// would report success with our hook still installed). Containers are never
// removed (see the header). No marked hook -> nil,nil (no write).
func rawRemoveEntries(raw []byte, spec Spec) ([]byte, error) {
	removed := false
	for {
		loc, err := locateSettings(raw, spec.Event)
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

// cutOneMarkedHook performs the first applicable cut in the event array.
func cutOneMarkedHook(raw []byte, stop jsonSpan, spec Spec) ([]byte, bool, error) {
	groups, err := arrayElements(raw, stop)
	if err != nil {
		return nil, false, err
	}
	for i, g := range groups {
		if spec.duplicate(raw, groups, i) {
			return cutItem(raw, g[0], g[1]), true, nil
		}
		if spec.Flat {
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
