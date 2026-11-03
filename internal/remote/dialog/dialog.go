// Package dialog carries harness permission dialogs to Buzz and answers back.
// A harness seam (a hook, an app-server client, a bridge) opens a request
// for the binding that serves its session; the Buzz agent posts it in the
// session's channel and writes the owner's answer; the seam applies the
// answer, unless the terminal answered first. The first create of
// resolved/<id>.json decides how a dialog ended.
//
// A session no binding serves records itself instead: a session record
// "~<name>" (session.json) beside its dialogs, which the machine agent gives
// a channel of its own while the session's process runs. The "~" keeps the
// name apart from every binding name.
//
//	<binding store dir>/dialogs/<binding name or ~session name>/
//	  session.json        a session record: the session no binding serves
//	  requests/<id>.json  the seam: one open dialog
//	  answers/<id>.json   the Buzz agent: the owner's answer
//	  resolved/<id>.json  whoever closed the dialog first
//
// Directories are 0700 and files 0600. A request is written whole by
// rename; an answer and a resolution are written whole and create-exclusive,
// so the first writer wins and a reader never sees a partial file.
package dialog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// Option is one choice the dialog offers.
type Option struct {
	N     int    `json:"n"`              // 1-based, as shown in Buzz
	Label string `json:"label"`          // "Allow once", "Allow this session", "Deny", or the harness's own label
	Value string `json:"value"`          // harness-specific decision token the seam applies
	Deny  bool   `json:"deny,omitempty"` // the option ❌ selects
}

// Request is one open dialog.
type Request struct {
	ID       string    `json:"id"`                  // unique, filename-safe (NewID)
	Harness  string    `json:"harness"`             // claude | codex | pi | acp
	Session  string    `json:"session"`             // native session id when known
	Tool     string    `json:"tool"`                // e.g. Bash, Edit, mcp__srv__tool, exec, apply_patch
	Text     string    `json:"text"`                // the dialog body as Buzz shows it
	Options  []Option  `json:"options"`             // empty for a free-text dialog without choices
	FreeText bool      `json:"free_text,omitempty"` // any reply text is the answer
	OpenedAt time.Time `json:"opened_at"`
	PID      int       `json:"pid"` // the waiting seam process
}

// Answer is the owner's answer from Buzz.
type Answer struct {
	ID     string    `json:"id"`
	Option int       `json:"option,omitempty"` // Option.N chosen
	Text   string    `json:"text,omitempty"`   // free-text answer
	By     string    `json:"by"`               // "buzz"
	At     time.Time `json:"at"`
}

// Resolution outcomes and closers.
const (
	OutcomeAllowed  = "allowed"
	OutcomeDenied   = "denied"
	OutcomeAnswered = "answered"
	OutcomeTerminal = "terminal"
	OutcomeExpired  = "expired"

	ByBuzz     = "buzz"
	ByTerminal = "terminal"
	ByTimeout  = "timeout"
)

// Resolved records how a dialog ended.
type Resolved struct {
	ID      string    `json:"id"`
	Outcome string    `json:"outcome"`         // OutcomeAllowed, OutcomeDenied, OutcomeAnswered, OutcomeTerminal or OutcomeExpired
	Label   string    `json:"label,omitempty"` // chosen option label, for the Buzz edit
	By      string    `json:"by"`              // ByBuzz, ByTerminal or ByTimeout
	At      time.Time `json:"at"`
}

// maxFileBytes bounds every dialog file read.
const maxFileBytes = 256 << 10

var idRe = regexp.MustCompile(`^d-[0-9a-f]{32}$`)

// NewID returns a fresh dialog id: "d-" and 32 hex characters.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return "d-" + hex.EncodeToString(b[:])
}

// Root is the directory that holds every binding's dialogs directory.
func Root() (string, error) {
	path, err := binding.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "dialogs"), nil
}

// DirFor returns the dialogs directory of a binding or a session record,
// creating it and its requests/, answers/ and resolved/ subdirectories.
func DirFor(bindingName string) (string, error) {
	if bindingName != binding.LegacyName {
		if err := binding.ValidName(strings.TrimPrefix(bindingName, sessionPrefix)); err != nil {
			return "", err
		}
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, bindingName)
	for _, sub := range []string{"requests", "answers", "resolved"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// Open writes requests/<id>.json.
func Open(dir string, r Request) error {
	if !idRe.MatchString(r.ID) {
		return fmt.Errorf("dialog id %q is not one NewID mints", r.ID)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = fsq.WriteFileAtomic(filepath.Join(dir, "requests"), r.ID+".json", raw, 0o600)
	return err
}

// Requests returns the open requests, those with no resolved/<id>.json,
// oldest first. An unreadable request file is skipped.
func Requests(dir string) ([]Request, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "requests"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !idRe.MatchString(id) {
			continue
		}
		if _, done, _ := ReadResolved(dir, id); done {
			continue
		}
		var r Request
		if found, err := readJSON(filepath.Join(dir, "requests", e.Name()), &r); err != nil || !found || r.ID != id {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].OpenedAt.Before(out[j].OpenedAt) })
	return out, nil
}

// WriteAnswer writes answers/<id>.json; false when an answer exists already.
func WriteAnswer(dir string, a Answer) (bool, error) {
	return createExclusive(filepath.Join(dir, "answers"), a.ID, a)
}

// ReadAnswer reads answers/<id>.json; ok=false when there is none.
func ReadAnswer(dir, id string) (Answer, bool, error) {
	var a Answer
	ok, err := readID(dir, "answers", id, &a)
	return a, ok, err
}

// RemoveAnswer removes answers/<id>.json, so the next answer can be written.
// A seam calls it for an answer it cannot apply.
func RemoveAnswer(dir, id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("dialog id %q is not one NewID mints", id)
	}
	err := os.Remove(filepath.Join(dir, "answers", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Resolve writes resolved/<id>.json; false when the dialog was resolved first
// by someone else.
func Resolve(dir string, r Resolved) (bool, error) {
	return createExclusive(filepath.Join(dir, "resolved"), r.ID, r)
}

// ReadResolved reads resolved/<id>.json; ok=false when the dialog is open.
func ReadResolved(dir, id string) (Resolved, bool, error) {
	var r Resolved
	ok, err := readID(dir, "resolved", id, &r)
	return r, ok, err
}

// FindBinding returns the live binding name that serves this seam: a
// mailbox binding with Root==root and Handle==handle, a binding whose
// NativeSession or Codex app-server thread is session, or one whose holder
// is one of ancestors, the seam's own process ancestry (launch.Ancestors),
// so a session keeps its binding when its session id changes (/clear). A
// holder matches by its whole identity; a binding with no recorded holder
// matches by the pid in its native Claude target. ("", nil) when none does.
// A binding proven ended serves nothing.
func FindBinding(root, handle, session string, ancestors []launch.HolderIdentity) (string, error) {
	list, err := binding.List()
	if err != nil {
		if errors.Is(err, binding.ErrNone) || errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	root = canonical(root)
	for _, b := range list {
		mailbox := b.Mailbox() && root != "" && handle != "" && b.Handle == handle && canonical(b.Root) == root
		native := session != "" && (b.NativeSession == session || b.AppServer.Thread == session)
		if !mailbox && !native && !HeldBy(b, ancestors) {
			continue
		}
		if state, _ := b.Liveness(); state == binding.Ended {
			continue
		}
		return b.Name, nil
	}
	return "", nil
}

// HeldBy reports whether b's session runs in one of procs: the whole
// recorded holder identity matches, or, for a binding with no recorded
// holder, the pid of its native Claude target.
func HeldBy(b binding.Binding, procs []launch.HolderIdentity) bool {
	for _, p := range procs {
		if b.Holder.PID > 0 && SameProcess(b.Holder, p) || b.Holder.PID <= 0 && b.HolderPID() > 0 && b.HolderPID() == p.PID {
			return true
		}
	}
	return false
}

// SameProcess reports whether a and b name one process: the same pid, start
// token, boot and pid namespace. An identity without a start token names no
// process.
func SameProcess(a, b launch.HolderIdentity) bool {
	return a.PID > 0 && a.ProcessStart != "" && a == b
}

// sessionPrefix starts every session record name; no binding name has it.
const sessionPrefix = "~"

// IsSessionName reports whether name is a session record's, not a binding's.
func IsSessionName(name string) bool { return strings.HasPrefix(name, sessionPrefix) }

// SessionRecord is a session no binding serves, as its seam recorded it.
type SessionRecord struct {
	Harness string `json:"harness"`
	Session string `json:"session"` // native session id
	Label   string `json:"label"`   // "<harness> · <project> @ <machine>", the channel's name
	// Holder is the process that runs the session: its channel lives while
	// that process runs.
	Holder launch.HolderIdentity `json:"holder"`
	// Binding is the binding the record's channel was handed to once the
	// session attached; "" while the record has its own channel. A handed
	// record only keeps its open dialogs served in that binding's channel.
	Binding string `json:"binding,omitempty"`
}

// RecordSession returns the name of r's session record, writing it when
// needed: the record r's holder already has, else "~<base>", "~<base>-2",
// ... the first that no live session holds. Seams of one session at once
// get one record: the choice and the write share one lock.
func RecordSession(r SessionRecord, base string) (string, error) {
	if r.Holder.PID <= 0 {
		return "", errors.New("a session record needs its process")
	}
	if err := binding.ValidName(base); err != nil {
		return "", err
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	name := ""
	err = withSessions(func() error {
		held, err := Sessions()
		if err != nil {
			return err
		}
		for n, o := range held {
			if o.Holder == r.Holder {
				name = n
			}
		}
		for i := 1; name == "" && i <= 100; i++ {
			candidate := sessionPrefix + base
			if i > 1 {
				suffix := fmt.Sprintf("-%d", i)
				candidate = sessionPrefix + strings.TrimRight(base[:min(len(base), 64-len(suffix))], "-_") + suffix
			}
			o, taken := held[candidate]
			_, statErr := os.Lstat(filepath.Join(root, candidate))
			switch {
			case taken:
				if state, _ := launch.InspectHolder(o.Holder); state == launch.LeaseStale {
					name = candidate // its session ended; the name is free
				}
			case errors.Is(statErr, os.ErrNotExist):
				name = candidate
			case statErr != nil:
				return statErr
			}
		}
		if name == "" {
			return errors.New("no free session record name")
		}
		if o, ok := held[name]; ok && o == r {
			return nil
		}
		dir, err := DirFor(name)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		_, err = fsq.WriteFileAtomic(dir, sessionFile, raw, 0o600)
		return err
	})
	return name, err
}

// sessionFile is a session record's file in its dialogs directory.
const sessionFile = "session.json"

// Sessions returns every readable session record by name.
func Sessions() (map[string]SessionRecord, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]SessionRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]SessionRecord{}
	for _, e := range entries {
		if !e.IsDir() || !IsSessionName(e.Name()) {
			continue
		}
		if r, ok, _ := ReadSession(e.Name()); ok {
			out[e.Name()] = r
		}
	}
	return out, nil
}

// ReadSession reads the session record name; ok=false when there is none.
func ReadSession(name string) (SessionRecord, bool, error) {
	var r SessionRecord
	path, err := sessionPath(name)
	if err != nil {
		return r, false, err
	}
	ok, err := readJSON(path, &r)
	return r, ok, err
}

// withSessions runs fn under the lock every session record write takes.
func withSessions(fn func() error) error {
	root, err := Root()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(filepath.Join(root, ".sessions.lock"), fn)
}

// sessionPath is the session record file of name.
func sessionPath(name string) (string, error) {
	if !IsSessionName(name) || binding.ValidName(name[len(sessionPrefix):]) != nil {
		return "", fmt.Errorf("%q is not a session record name", name)
	}
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name, sessionFile), nil
}

// RemoveSessionDir removes the dialogs directory of a session record that
// is gone or handed to a binding, under the session lock, so a record
// written meanwhile is never removed; removed=false when the record serves
// its own channel.
func RemoveSessionDir(name string) (removed bool, err error) {
	path, err := sessionPath(name)
	if err != nil {
		return false, err
	}
	err = withSessions(func() error {
		if r, ok, err := ReadSession(name); err != nil || ok && r.Binding == "" {
			return err
		}
		removed = true
		return os.RemoveAll(filepath.Dir(path))
	})
	return removed, err
}

// DropSession removes the session record name when it is still holder's,
// under the session lock: a session that took the name over keeps its
// record. Its dialogs stay until they are resolved and dropped like those of
// a binding that is gone.
func DropSession(name string, holder launch.HolderIdentity) error {
	path, err := sessionPath(name)
	if err != nil {
		return err
	}
	return withSessions(func() error {
		r, ok, err := ReadSession(name)
		if err != nil || !ok || r.Holder != holder {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// HandOver marks the session record name handed to bindingName when it is
// still holder's, under the session lock.
func HandOver(name string, holder launch.HolderIdentity, bindingName string) error {
	path, err := sessionPath(name)
	if err != nil {
		return err
	}
	return withSessions(func() error {
		r, ok, err := ReadSession(name)
		if err != nil || !ok || r.Holder != holder || r.Binding == bindingName {
			return err
		}
		r.Binding = bindingName
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		_, err = fsq.WriteFileAtomic(filepath.Dir(path), sessionFile, raw, 0o600)
		return err
	})
}

// ParseAnswer maps a Buzz reply to an answer: "N" picks option N;
// "yes"/"y"/"approve"/"✅" picks option 1; "no"/"n"/"reject"/"❌" picks the
// Deny option; any other non-empty text is a free-text answer when
// r.FreeText. ok=false when the reply is not an answer.
func ParseAnswer(r Request, reply string) (Answer, bool) {
	text := strings.TrimSpace(reply)
	word := strings.ToLower(strings.TrimRight(strings.TrimSuffix(text, "️"), ".!"))
	pick := func(n int) (Answer, bool) {
		return Answer{ID: r.ID, Option: n, By: ByBuzz, At: time.Now().UTC()}, true
	}
	if n, err := strconv.Atoi(word); err == nil {
		for _, o := range r.Options {
			if o.N == n {
				return pick(n)
			}
		}
	}
	switch word {
	case "yes", "y", "approve", "✅":
		for _, o := range r.Options {
			if o.N == 1 {
				return pick(1)
			}
		}
	case "no", "n", "reject", "❌":
		for _, o := range r.Options {
			if o.Deny {
				return pick(o.N)
			}
		}
	}
	if r.FreeText && text != "" {
		return Answer{ID: r.ID, Text: text, By: ByBuzz, At: time.Now().UTC()}, true
	}
	return Answer{}, false
}

// canonical is root with symlinks resolved, so two spellings of one root
// match; the clean path when it does not resolve.
func canonical(root string) string {
	if root == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return resolved
	}
	return filepath.Clean(root)
}

// createExclusive writes v whole to <sub>/<id>.json unless that file
// exists: the content goes to a temp file first and is hard-linked into
// place, so the link either creates the complete file or fails.
func createExclusive(sub, id string, v any) (bool, error) {
	if !idRe.MatchString(id) {
		return false, fmt.Errorf("dialog id %q is not one NewID mints", id)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(sub, "."+id+".tmp-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmp.Name(), filepath.Join(sub, id+".json")); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	_ = fsq.SyncDir(sub)
	return true, nil
}

func readID(dir, sub, id string, v any) (bool, error) {
	if !idRe.MatchString(id) {
		return false, fmt.Errorf("dialog id %q is not one NewID mints", id)
	}
	return readJSON(filepath.Join(dir, sub, id+".json"), v)
}

func readJSON(path string, v any) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, err
	}
	return true, nil
}
