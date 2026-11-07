// Package dialog carries harness permission dialogs to Buzz and answers back.
// A harness seam (a hook, an app-server client, a bridge) opens a request
// for a live binding; the Buzz agent posts it and writes the owner's answer;
// the seam applies the answer, unless the terminal answered first. The
// first create of resolved/<id>.json decides how a dialog ended.
//
//	<binding store dir>/dialogs/<binding name>/
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

// DirFor returns the dialogs directory of a binding, creating it and its
// requests/, answers/ and resolved/ subdirectories.
func DirFor(bindingName string) (string, error) {
	if bindingName != binding.LegacyName {
		if err := binding.ValidName(bindingName); err != nil {
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
	_, err = fsq.WriteStateFile(filepath.Dir(dir), filepath.Join(filepath.Base(dir), "requests", r.ID+".json"), raw, 0o600)
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
// mailbox binding with Root==root and Handle==handle, or a binding whose
// NativeSession or Codex app-server thread is session. ("", nil) when none
// does. A binding proven ended serves nothing.
func FindBinding(root, handle, session string) (string, error) {
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
		if !mailbox && !native {
			continue
		}
		if state, _ := b.Liveness(); state == binding.Ended {
			continue
		}
		return b.Name, nil
	}
	return "", nil
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
