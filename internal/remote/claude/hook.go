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
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
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
// Install always inserts at the FIRST position of the target container,
// with a tight trailing comma when the container is non-empty; uninstall
// removes exactly our hook plus that comma. Uninstall never removes a
// container: formatting cannot prove who created an empty "Stop" array or
// "hooks" object, so an operator's containers are preserved and ours may be
// left behind empty, which Claude Code treats as no hooks (codex #855 r2
// item 3). A file that already had a Stop array round-trips byte for byte.

// InstallStopHook adds the fail-open receiver to the Stop hook chain in
// home/.claude/settings.json. Idempotent: when an entry carrying the
// marker exists the file is left untouched. The settings leaf must be a
// regular file (lstat rule — a directory or symlink is refused); the write
// is fsq atomic-replace.
//
// Claude Code's durable hook scopes are the user, project, local, and
// managed settings files. None of them is a single session: session hooks
// exist only in memory inside that process
// (https://code.claude.com/docs/en/hooks). The bound session can live in
// any project, so the installer keeps this user-level hook. The receiver
// returns before any write unless that session is attached or bound.
func InstallStopHook(home, bin string) error {
	return mutateStopHook(home, true, bin)
}

// UninstallStopHook removes marked entries from the Stop chain. When no
// marked entry exists the file is untouched (no rewrite, no mtime churn).
func UninstallStopHook(home string) error {
	return mutateStopHook(home, false, "")
}

// Stop hook states in the user settings file (StopHookState).
const (
	StopHookPresent  = "installed"
	StopHookMissing  = "missing"
	StopHookDisabled = "disabled" // present, but disableAllHooks is true
)

// StopHookState reports whether the user settings file under home holds the
// AMQ Stop hook, and whether that file disables all hooks. Without an
// effective hook a Claude request is admitted but never completes. It reads
// only ~/.claude/settings.json, through the installer's bounded reader
// (codex #869 r1); project or managed settings can still change the
// effective result.
func StopHookState(home string) (string, error) {
	raw, err := readRegularBounded(settingsPath(home), maxSettingsBytes)
	if errors.Is(err, os.ErrNotExist) {
		return StopHookMissing, nil
	}
	if err != nil {
		return "", err
	}
	loc, err := locateSettings(raw)
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
			if groupHasOurHook(raw[el[0]:el[1]]) {
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
func isOurHook(el []byte) bool {
	var h struct {
		Command string `json:"command"`
	}
	return json.Unmarshal(el, &h) == nil && strings.HasPrefix(h.Command, stopHookMarker)
}

// groupHasOurHook reports whether a matcher group holds any marked hook.
func groupHasOurHook(el []byte) bool {
	var g struct {
		Hooks []json.RawMessage `json:"hooks"`
	}
	if json.Unmarshal(el, &g) != nil {
		return false
	}
	for _, h := range g.Hooks {
		if isOurHook(h) {
			return true
		}
	}
	return false
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
		els, err := arrayElements(raw, *loc.stop)
		if err != nil {
			return nil, err
		}
		for _, e := range els {
			if groupHasOurHook(raw[e[0]:e[1]]) {
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

// rawRemoveStopEntries removes every marked hook from the Stop chain by byte
// surgery, one cut per pass: a matcher group whose hooks are all ours is
// cut whole; in a group that also holds foreign hooks only our inner hook
// is cut and the foreign hooks stay (codex #855 r2 item 4 — the previous
// version skipped mixed groups and reported success with our hook still
// installed). Containers are never removed (see the header). No marked
// hook -> nil,nil (no write).
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
		next, cut, err := cutOneMarkedHook(raw, *loc.stop)
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
func cutOneMarkedHook(raw []byte, stop jsonSpan) ([]byte, bool, error) {
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
			if isOurHook(raw[h[0]:h[1]]) {
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
// leaf is refused rather than followed or waited on (codex #855 r1).
func RunStopHookReceiver(home string, stdin io.Reader, stdout io.Writer) int {
	defer func() { _ = recover() }() // fail-open, unconditionally
	_ = stdout
	if !noFollowSupported {
		return 0 // no safe create on this platform (codex #855 r3 item 2)
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
	// directory is created (bead 611.37).
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("stop bound dir %s is not a plain directory", dir)
	}
	f, err := os.CreateTemp(dir, "owner-")
	if err != nil {
		return "", err
	}
	token := filepath.Base(f.Name())
	if err := f.Close(); err != nil {
		return "", err
	}
	return token, nil
}

// unbindStopSession drops one attachment's sentinel. The marker stays while
// any other attachment still owns the session.
func unbindStopSession(home, sessionID, token string) {
	if !sessionIDRe.MatchString(sessionID) || !noFollowSupported || token == "" {
		return
	}
	removeRegular(filepath.Join(stopBoundDir(home, sessionID), token))
	if stopSentinelPresent(home, sessionID) {
		return
	}
	removeRegular(stopMarkerPath(home, sessionID))
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
// With no binding file, an attached sentinel is enough.
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

// bindingDecision reads the per-user binding. The hook process has only the
// Claude home, so an unset AMQ_REMOTE_BINDING resolves under that home.
func bindingDecision(home, sessionID string) int {
	path := filepath.Join(home, ".amq", "remote", "binding.json")
	if p := strings.TrimSpace(os.Getenv(binding.EnvPath)); p != "" && filepath.IsAbs(p) {
		path = filepath.Clean(p)
	}
	raw, err := readRegularBounded(path, bindingMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return bindingAbsent
	}
	if err != nil {
		return bindingDeny
	}
	var b struct {
		NativeSession string `json:"native_session"`
	}
	if json.Unmarshal(raw, &b) != nil || b.NativeSession != sessionID {
		return bindingDeny
	}
	return bindingAllow
}

// bindingMaxBytes matches the binding package's read bound.
const bindingMaxBytes = 64 << 10
