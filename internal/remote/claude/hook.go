package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Stop-hook bridge (bead 611.12 PR2): the hook is how the adapter learns a
// submitted turn ENDED — the transcript user line proves the payload was
// accepted and the assistant line proves the turn started, but only the
// Stop event carries the completion signal. The installer writes the hook
// into the TARGET's ~/.claude/settings.json; the receiver subcommand is
// fail-open by contract (exit 0 on ANY error) so a broken bridge can never
// block Claude Code — Claude Code's blocking-error code is exit 2.

// stopHookMarker is the settings.json ownership signal: every hook command
// the installer writes STARTS with this env assignment, and uninstall
// removes exactly the entries whose nested commands all carry it. It is an
// assignment, not the command word: the shell runs the quoted binary that
// follows it (codex #855 r1 item 3 — the previous `AMQ_STOP_HOOK_BIN=<bin>
// claude stop-hook` form executed the program `claude`).
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
// Install always inserts at the FIRST position of the target container
// with a tight trailing comma when the container is non-empty, and
// uninstall removes exactly that element plus that comma, then the
// `"Stop":[]` and `"hooks":{}` members when they are empty in the tight
// form install creates. That symmetry is what makes install+uninstall a
// byte-for-byte identity on the original file.

// InstallStopHook adds the fail-open receiver to the Stop hook chain in
// home/.claude/settings.json. Idempotent: when an entry carrying the
// marker exists the file is left untouched. The settings leaf must be a
// regular file (lstat rule — a directory or symlink is refused); the write
// is fsq atomic-replace.
func InstallStopHook(home, bin string) error {
	return mutateStopHook(home, true, bin)
}

// UninstallStopHook removes marked entries from the Stop chain. When no
// marked entry exists the file is untouched (no rewrite, no mtime churn).
func UninstallStopHook(home string) error {
	return mutateStopHook(home, false, "")
}

func settingsPath(home string) string {
	return filepath.Join(home, ".claude", "settings.json")
}

func mutateStopHook(home string, install bool, bin string) error {
	path := settingsPath(home)
	raw, err := readRegularBounded(path, maxSettingsBytes)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("settings %s: %w", path, err)
		}
		if !install {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		raw = []byte("{}\n")
	}
	var out []byte
	if install {
		out, err = rawInsertStopEntry(raw, stopHookEntry(bin))
	} else {
		out, err = rawRemoveStopEntries(raw)
	}
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
// assignment, then the single-quoted binary as the command word.
func stopHookCommand(bin string) string {
	return stopHookMarker + shellQuote(bin) + " claude stop-hook"
}

// shellQuote single-quotes s for POSIX sh (Claude Code runs hook commands
// through the shell).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// stopHookEntry renders one Stop matcher group in Claude Code's settings
// schema: an OUTER object whose "hooks" array holds the command hook
// (codex #855 r1 item 4; https://code.claude.com/docs/en/hooks#configuration
// and internal/keepalive/hookinstall's SessionStart entry use the same
// shape). Stop takes no matcher.
func stopHookEntry(bin string) []byte {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	b, _ := json.Marshal(struct {
		Hooks []hook `json:"hooks"`
	}{Hooks: []hook{{Type: "command", Command: stopHookCommand(bin), Timeout: 10}}})
	return b
}

// jsonSpan is one container value in the raw bytes: open is the index of
// its '{' or '[', close the index of the matching '}' or ']'. keyStart is
// the index of the member key's opening quote, or -1 when unknown.
type jsonSpan struct {
	keyStart, open, close int
	empty                 bool
}

// settingsLoc is where the root object, hooks object and Stop array live.
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
// (codex #855 r1 item 5: the previous walker never recognized "hooks
// without Stop" and so added a duplicate top-level hooks key.)
func locateSettings(raw []byte) (settingsLoc, error) {
	const hooksPath, stopPath = "$/hooks", "$/hooks/" + stopHookType
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
			return errors.New("settings hooks.Stop is not an array; refusing to edit")
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

// ownedGroup reports whether a Stop array element is a matcher group this
// installer wrote: an object whose nested hooks all carry the marker. A
// group mixing our hook with foreign ones is left alone (the installer
// never writes one; removing part of it would edit content we do not own).
func ownedGroup(el []byte) bool {
	var g struct {
		Hooks []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(el, &g); err != nil || len(g.Hooks) == 0 {
		return false
	}
	for _, h := range g.Hooks {
		if !strings.HasPrefix(h.Command, stopHookMarker) {
			return false
		}
	}
	return true
}

// stopElements returns the byte spans [start,end) of each element of the
// Stop array. Leading whitespace and separating commas are skipped so a
// span is exactly the element's bytes.
func stopElements(raw []byte, stop jsonSpan) ([][2]int, error) {
	seg := raw[stop.open : stop.close+1]
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
		out = append(out, [2]int{stop.open + s, stop.open + int(dec.InputOffset())})
	}
	return out, nil
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

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

// rawInsertStopEntry inserts the matcher group into the Stop chain by byte
// surgery, creating "Stop" (and "hooks") when absent. Returns nil,nil when
// an owned group already exists.
func rawInsertStopEntry(raw, entry []byte) ([]byte, error) {
	loc, err := locateSettings(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case loc.stop != nil:
		els, err := stopElements(raw, *loc.stop)
		if err != nil {
			return nil, err
		}
		for _, e := range els {
			if ownedGroup(raw[e[0]:e[1]]) {
				return nil, nil // already installed
			}
		}
		return insertFirst(raw, *loc.stop, entry), nil
	case loc.hooks != nil:
		member := append([]byte(`"`+stopHookType+`":[`), entry...)
		return insertFirst(raw, *loc.hooks, append(member, ']')), nil
	default:
		member := append([]byte(`"hooks":{"`+stopHookType+`":[`), entry...)
		return insertFirst(raw, loc.root, append(member, "]}"...)), nil
	}
}

// rawRemoveStopEntries removes owned groups from the Stop chain by byte
// surgery, then the Stop member and the hooks member when they are left
// empty in the tight form install writes. No owned group -> nil,nil (no
// write).
func rawRemoveStopEntries(raw []byte) ([]byte, error) {
	removed := false
	for {
		loc, err := locateSettings(raw)
		if err != nil {
			return nil, err
		}
		if loc.stop == nil {
			break
		}
		els, err := stopElements(raw, *loc.stop)
		if err != nil {
			return nil, err
		}
		cut := false
		for _, e := range els {
			if ownedGroup(raw[e[0]:e[1]]) {
				raw, cut, removed = cutItem(raw, e[0], e[1]), true, true
				break
			}
		}
		if !cut {
			break
		}
	}
	if !removed {
		return nil, nil
	}
	// Drop the members install created, only in their exact tight form so
	// a member the operator wrote (e.g. `"Stop": []` with a space) stays.
	type emptyMember struct {
		tight string
		pick  func(settingsLoc) *jsonSpan
	}
	for _, m := range []emptyMember{
		{`"` + stopHookType + `":[]`, func(l settingsLoc) *jsonSpan { return l.stop }},
		{`"hooks":{}`, func(l settingsLoc) *jsonSpan { return l.hooks }},
	} {
		loc, err := locateSettings(raw)
		if err != nil {
			return nil, err
		}
		sp := m.pick(loc)
		if sp == nil || sp.keyStart < 0 || string(raw[sp.keyStart:sp.close+1]) != m.tight {
			break
		}
		raw = cutItem(raw, sp.keyStart, sp.close+1)
	}
	return raw, nil
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
// leaf is refused rather than followed or waited on (codex #855 r1).
func RunStopHookReceiver(home string, stdin io.Reader, stdout io.Writer) int {
	defer func() { _ = recover() }() // fail-open, unconditionally
	_ = stdout
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
	marker := stopMarkerPath(home, payload.SessionID)
	dir := filepath.Dir(marker)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return 0 // a symlinked marker directory is never written through
	}
	if fi, err := os.Lstat(marker); err == nil && !fi.Mode().IsRegular() {
		return 0
	}
	f, err := os.OpenFile(marker, os.O_APPEND|os.O_CREATE|os.O_WRONLY|openNoFollowFlag, 0o600)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
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
